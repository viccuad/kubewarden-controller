#!/usr/bin/env bash
# kubewarden-unified-adm-controller-chart-migration.sh
#
# Migrates an existing Kubewarden installation from the legacy three-chart
# stack (kubewarden-crds, kubewarden-controller, kubewarden-defaults) to
# the unified single admission-controller chart — with zero downtime.
#
# The migration proceeds in five phases:
#
#   1. Preflight
#      Verifies required tools (helm v4+, kubectl, yq, jq), checks cluster
#      connectivity, detects the three legacy Helm releases, and confirms
#      they are at the latest published chart version.
#
#   2. Inject keep annotations
#      Creates a Helm 4 post-renderer plugin that wraps yq, then runs
#      `helm upgrade --reuse-values --post-renderer` on each legacy
#      release. This bakes `helm.sh/resource-policy: keep` into the stored
#      release manifest so that a subsequent `helm uninstall` leaves all
#      resources behind.
#
#   3. Uninstall legacy releases
#      Runs `helm uninstall --no-hooks` for all three legacy releases in
#      reverse order. The --no-hooks flag is critical because the
#      kubewarden-controller chart ships a pre-delete hook that would
#      delete all PolicyServers. After this step, the releases are gone
#      from Helm but every resource remains live in the cluster.
#
#   4. Install unified chart
#      Runs `helm install --take-ownership` with the unified admission-controller
#      chart under a fresh release name (admission-controller). No nameOverride
#      is needed: the controller's stateless plumbing (Deployment, Services,
#      ConfigMap, RBAC, webhook configs) is recreated with the chart's natural
#      names, while only the constant-named data/identity resources (CRDs, cert
#      Secrets, policy-server SA, context-watcher RBAC, default PolicyServer and
#      recommended policies) are adopted in place via Helm 4 Server-Side Apply.
#      Only the kubewarden-ca Secret is kept and reused by the chart's `lookup`
#      (no CA churn); the leaf certs are regenerated from it with SANs matching
#      the recreated webhook Service name. The merged values file (the
#      concatenation of the three legacy
#      releases' `helm get values`, with name/fullname overrides and the three
#      component image tags stripped so the unified chart's appVersion images
#      are used) preserves the user's prior configuration. The migrated release is vanilla
#      — it stores no overrides, so future upgrades need no special values.
#
#   5. Post-migration verification
#      Confirms CRDs are owned by the new release, the controller
#      Deployment is running (located via its hardened label selector), RBAC and
#      cert Secrets were adopted in place (UIDs unchanged), and the
#      DefaultsApplier has labeled the default PolicyServer and recommended
#      policies.
#
# Requirements:
#   - helm v4+ (Server-Side Apply and post-renderer plugin support)
#   - kubectl
#   - yq v4 (github.com/mikefarah/yq)
#   - jq
#
# CLI usage:
#   ./kubewarden-unified-adm-controller-chart-migration.sh
#       --unified-chart PATH_OR_CHART_NAME
#       [--namespace NS]
#       [--kube-context CTX]
#       [--repo-name NAME]
#       [--timeout DURATION]
#       [--set KEY=VALUE]
#       [--values FILE]
#       [--no-interactive]
#       [--dry-run]
#       [--verbose]
#       [--help]
#
# When --unified-chart is in the form <alias>/<chart>, <alias> must match
# --repo-name and the alias must already be configured in your local Helm
# repo list (run: helm repo add <name> <url> && helm repo update).
#
# Examples:
#   # Migrate using a local chart tarball:
#   ./kubewarden-unified-adm-controller-chart-migration.sh \
#       --unified-chart ./admission-controller-6.0.0.tgz
#
#   # Migrate using the chart from the Helm repo:
#   ./kubewarden-unified-adm-controller-chart-migration.sh \
#       --unified-chart kubewarden/admission-controller
#
#   # Dry run (no changes):
#   ./kubewarden-unified-adm-controller-chart-migration.sh \
#       --unified-chart ./admission-controller-6.0.0.tgz --dry-run

set -euo pipefail

#------------------------------------------------------------------------------
# Defaults
#------------------------------------------------------------------------------
KW_NAMESPACE="${KW_NAMESPACE:-kubewarden}"
KUBE_CONTEXT=""
UNIFIED_CHART=""
HELM_REPO_NAME="${HELM_REPO_NAME:-kubewarden}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-5m}"
INTERACTIVE=1
DRY_RUN=0
VERBOSE=0

# Extra arguments forwarded to `helm install` for the unified chart.
HELM_INSTALL_EXTRA_ARGS=()

LOG_FILE="${LOG_FILE:-./kubewarden-migration.log}"

POSTRENDER_PLUGIN_DIR="./kw-keep-postrenderer"

KW_CRDS=(
  policyservers.policies.kubewarden.io
  clusteradmissionpolicies.policies.kubewarden.io
  admissionpolicies.policies.kubewarden.io
  clusteradmissionpolicygroups.policies.kubewarden.io
  admissionpolicygroups.policies.kubewarden.io
)

LEGACY_RELEASES=(
  "kubewarden-crds=kubewarden-crds"
  "kubewarden-controller=kubewarden-controller"
  "kubewarden-defaults=kubewarden-defaults"
)

# Detected at runtime.
LEGACY_CONTROLLER_RELEASE_NAME=""
RECOMMENDED_POLICIES=()
# Values file built by concatenating the three legacy releases' user values.
MERGED_VALUES_FILE="${MERGED_VALUES_FILE:-./kw-merged-values.yaml}"
# The unified chart is installed under this fresh release name. The controller
# plumbing is recreated with the chart's natural names, so NO nameOverride is
# needed and future upgrades are vanilla.
UNIFIED_RELEASE_NAME="${UNIFIED_RELEASE_NAME:-admission-controller}"

#------------------------------------------------------------------------------
# CLI parsing
#------------------------------------------------------------------------------
print_help() {
  sed -n '2,72p' "$0"
}

require_flag_value() {
  local flag="$1" value="${2:-}"
  [[ -n "$value" ]] || { echo "missing value for $flag" >&2; exit 2; }
}

while (( $# > 0 )); do
  case "$1" in
    --unified-chart)
      require_flag_value "$1" "${2:-}"; UNIFIED_CHART="$2"; shift 2 ;;
    --unified-chart=*)
      UNIFIED_CHART="${1#--unified-chart=}"; shift ;;
    --namespace|-n)
      require_flag_value "$1" "${2:-}"; KW_NAMESPACE="$2"; shift 2 ;;
    --namespace=*)
      KW_NAMESPACE="${1#--namespace=}"; shift ;;
    --kube-context)
      require_flag_value "$1" "${2:-}"; KUBE_CONTEXT="$2"; shift 2 ;;
    --kube-context=*)
      KUBE_CONTEXT="${1#--kube-context=}"; shift ;;
    --repo-name)
      require_flag_value "$1" "${2:-}"; HELM_REPO_NAME="$2"; shift 2 ;;
    --repo-name=*)
      HELM_REPO_NAME="${1#--repo-name=}"; shift ;;

    --timeout)
      require_flag_value "$1" "${2:-}"; WAIT_TIMEOUT="$2"; shift 2 ;;
    --timeout=*)
      WAIT_TIMEOUT="${1#--timeout=}"; shift ;;
    --no-interactive)
      INTERACTIVE=0; shift ;;
    --dry-run)
      DRY_RUN=1; shift ;;
    --verbose)
      VERBOSE=1; shift ;;
    --set)
      require_flag_value "$1" "${2:-}"; HELM_INSTALL_EXTRA_ARGS+=(--set "$2"); shift 2 ;;
    --set=*)
      HELM_INSTALL_EXTRA_ARGS+=(--set "${1#--set=}"); shift ;;
    --values|-f)
      require_flag_value "$1" "${2:-}"; HELM_INSTALL_EXTRA_ARGS+=(--values "$2"); shift 2 ;;
    --values=*)
      HELM_INSTALL_EXTRA_ARGS+=(--values "${1#--values=}"); shift ;;
    -h|--help)
      print_help; exit 0 ;;
    *)
      echo "unknown argument: $1" >&2
      echo "run with --help for usage" >&2
      exit 2
      ;;
  esac
done

if [[ -z "$UNIFIED_CHART" ]]; then
  echo "--unified-chart is required" >&2
  echo "run with --help for usage" >&2
  exit 2
fi

#------------------------------------------------------------------------------
# Logging / helpers
#------------------------------------------------------------------------------
: > "$LOG_FILE"
exec > >(tee -a "$LOG_FILE") 2>&1

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '    \033[1;33mWARN:\033[0m %s\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }
ok()   { printf '    \033[1;32mOK\033[0m %s\n' "$*"; }

confirm() {
  if (( INTERACTIVE == 0 )); then return 0; fi
  local msg="$1"
  printf '\n    \033[1;33m%s\033[0m\n' "$msg"
  read -r -p "    Continue? (y/n) " choice
  case "$choice" in
    y|Y) return 0 ;;
    *) echo "Aborted by user."; exit 0 ;;
  esac
}

dry_run_guard() {
  if (( DRY_RUN == 1 )); then
    info "[dry-run] would run: $*"
    return 1
  fi
  return 0
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

# kubectl wrapper that injects --context if set.
kctl() {
  if [[ -n "$KUBE_CONTEXT" ]]; then
    kubectl --context "$KUBE_CONTEXT" "$@"
  else
    kubectl "$@"
  fi
}

# helm wrapper that injects --kube-context if set.
hctl() {
  if [[ -n "$KUBE_CONTEXT" ]]; then
    helm --kube-context "$KUBE_CONTEXT" "$@"
  else
    helm "$@"
  fi
}

helm_major_version() {
  helm version --short 2>/dev/null | sed -E 's/^v?([0-9]+)\..*/\1/' | head -n1
}

# Prints overridden component image tags (path=value) found in a values blob
# read from stdin, one per line. Only the three Kubewarden component tags matter
# — they must match the unified chart's appVersion, so any override is dropped
# during the merge (see phase_build_merged_values).
detect_overridden_image_tags() {
  yq -r '
    [
      {"p":"image.tag","v":.image.tag},
      {"p":"auditScanner.image.tag","v":.auditScanner.image.tag},
      {"p":"policyServer.image.tag","v":.policyServer.image.tag}
    ] | .[] | select(.v != null) | .p + "=" + (.v|tostring)
  ' 2>/dev/null || true
}

# classify_unified_chart CHART
# Prints one of: local | oci | repo
# local — an existing file or a directory that contains Chart.yaml
# oci   — starts with oci://
# repo  — <alias>/<chart> (remaining form)
classify_unified_chart() {
  local chart="$1"
  if [[ "$chart" == oci://* ]]; then
    echo "oci"
  elif [[ -f "$chart" ]] || [[ -d "$chart" && -f "$chart/Chart.yaml" ]]; then
    echo "local"
  else
    echo "repo"
  fi
}

cleanup_on_exit() {
  # Uninstall plugin on exit, but leave the plugin directory for inspection.
  hctl plugin uninstall kw-keep-postrenderer 2>/dev/null || true
}
trap cleanup_on_exit EXIT

# wait_managed_by_label SCOPE KIND NAME [NAMESPACE]
wait_managed_by_label() {
  local scope="$1" kind="$2" name="$3" ns="${4:-}"
  local kctl_args=()
  if [[ "$scope" == "namespaced" ]]; then
    [[ -n "$ns" ]] || fail "wait_managed_by_label: namespace required for namespaced kind"
    kctl_args=(-n "$ns")
  fi
  info "waiting for $kind/$name to be labeled kubewarden.io/managed-by=kubewarden-controller-defaults"
  local i v
  for i in $(seq 1 60); do
    v="$(kctl "${kctl_args[@]}" get "$kind" "$name" \
          -o jsonpath='{.metadata.labels.kubewarden\.io/managed-by}' 2>/dev/null || true)"
    if [[ "$v" == "kubewarden-controller-defaults" ]]; then
      ok "$kind/$name has DefaultsApplier label"
      return 0
    fi
    sleep 5
  done
  kctl "${kctl_args[@]}" get "$kind" "$name" -o yaml || true
  fail "$kind/$name was not stamped with kubewarden.io/managed-by=kubewarden-controller-defaults within timeout"
}

#------------------------------------------------------------------------------
# Phase 1: Preflight
#------------------------------------------------------------------------------
phase_preflight() {
  step "Phase 1: Preflight checks"

  for c in helm kubectl yq jq; do require_cmd "$c"; done

  info "using helm: $(helm version --short 2>/dev/null)"

  local helm_major
  helm_major="$(helm_major_version)"
  [[ -n "$helm_major" ]] || fail "could not determine Helm version"
  (( helm_major >= 4 )) \
    || fail "Helm v4+ is required (Server-Side Apply + post-renderer plugins). Found: $(helm version --short)"
  ok "Helm v4+ detected"

  if [[ -n "$KUBE_CONTEXT" ]]; then
    info "using kube context: $KUBE_CONTEXT"
  else
    info "using current kube context: $(kubectl config current-context 2>/dev/null || echo '<none>')"
  fi
  kctl cluster-info >/dev/null 2>&1 \
    || fail "cannot connect to Kubernetes cluster"
  ok "cluster is reachable"

  # Classify the unified chart source and validate repo alias consistency.
  local chart_kind
  chart_kind="$(classify_unified_chart "$UNIFIED_CHART")"
  info "unified chart source type: $chart_kind ($UNIFIED_CHART)"

  if [[ "$chart_kind" == "repo" ]]; then
    local chart_alias="${UNIFIED_CHART%%/*}"
    if [[ "$chart_alias" != "$HELM_REPO_NAME" ]]; then
      fail "unified chart alias '$chart_alias' does not match --repo-name '$HELM_REPO_NAME'." \
           $'\nAlign them (e.g. --repo-name '"$chart_alias"') or pass a local tarball to --unified-chart.'
    fi

    # Verify the alias exists in the user's local Helm config; never mutate it.
    info "verifying Helm repo alias '$HELM_REPO_NAME' is configured locally"
    local repo_url_local
    repo_url_local="$(helm repo list -o json 2>/dev/null \
      | jq -r --arg name "$HELM_REPO_NAME" '.[] | select(.name==$name) | .url' 2>/dev/null || true)"
    if [[ -z "$repo_url_local" ]]; then
      fail "Helm repo alias '$HELM_REPO_NAME' is not configured locally." \
           $'\nRun: helm repo add '"$HELM_REPO_NAME"' <url> && helm repo update\nThen re-run the migration script.'
    fi
    info "repo alias '$HELM_REPO_NAME' points to: $repo_url_local"

    if ! hctl repo update "$HELM_REPO_NAME" >/dev/null 2>&1; then
      warn "could not refresh Helm repo index for '$HELM_REPO_NAME' (offline?); proceeding with cached index"
    else
      ok "Helm repo '$HELM_REPO_NAME' index refreshed"
    fi
  fi

  # Detect legacy releases.
  info "detecting legacy Kubewarden releases in namespace $KW_NAMESPACE"
  local entry release chart app_version
  for entry in "${LEGACY_RELEASES[@]}"; do
    release="${entry%%=*}"
    chart="${entry##*=}"

    if ! hctl status "$release" -n "$KW_NAMESPACE" >/dev/null 2>&1; then
      fail "legacy release '$release' not found in namespace '$KW_NAMESPACE'"
    fi

    app_version="$(hctl get metadata "$release" -n "$KW_NAMESPACE" -o json | jq -r '.appVersion')"
    [[ -n "$app_version" && "$app_version" != "null" ]] \
      || fail "could not determine installed appVersion for $release"

    # Migration requires legacy charts to be at appVersion v1.36.0 before proceeding.
    [[ "$app_version" == "v1.36.0" ]] \
      || fail "$release has appVersion $app_version; this migration requires appVersion v1.36.0"
    ok "$release appVersion is $app_version (v1.36.0 — OK)"

    # Remember the kubewarden-controller release name for unified chart install.
    if [[ "$chart" == "kubewarden-controller" ]]; then
      LEGACY_CONTROLLER_RELEASE_NAME="$release"
    fi

    # Warn early (before any destructive step) if the release pins a component
    # image tag. Such overrides are dropped during the merge so the unified
    # chart's appVersion images are used; a stale tag would run an old image
    # that lacks the new controller flags and crashloop, failing the install.
    local overridden
    overridden="$(hctl get values "$release" -n "$KW_NAMESPACE" -o yaml 2>/dev/null \
      | detect_overridden_image_tags)"
    if [[ -n "$overridden" ]]; then
      warn "$release overrides component image tag(s): ${overridden//$'\n'/, }"
      warn "  these overrides will be DROPPED; the unified chart's appVersion images will be used."
      warn "  (a stale component tag would run an old image incompatible with the new controller flags and crashloop)"
    fi
  done

  [[ -n "$LEGACY_CONTROLLER_RELEASE_NAME" ]] \
    || fail "could not determine the legacy kubewarden-controller release name"
  info "unified chart will be installed with release name: $UNIFIED_RELEASE_NAME"

  # Resolve the unified chart now — before any destructive step — so a bad
  # reference is caught while the legacy releases are still intact.
  info "resolving unified chart '$UNIFIED_CHART'"
  local chart_meta
  if ! chart_meta="$(hctl show chart "$UNIFIED_CHART" 2>/dev/null)"; then
    fail "unified chart '$UNIFIED_CHART' could not be resolved." \
         $'\nFor repo charts: ensure the alias is correct and the repo index is current.\nFor local charts: check the path exists.'
  fi
  local resolved_name resolved_version
  resolved_name="$(printf '%s\n' "$chart_meta" | yq '.name' 2>/dev/null || true)"
  resolved_version="$(printf '%s\n' "$chart_meta" | yq '.version' 2>/dev/null || true)"
  ok "unified chart resolved: $resolved_name $resolved_version"

  # Verify CRDs exist.
  for crd in "${KW_CRDS[@]}"; do
    kctl get crd "$crd" >/dev/null 2>&1 \
      || fail "CRD $crd not found; the kubewarden-crds release may be incomplete"
  done
  ok "all Kubewarden CRDs present"

  # Snapshot recommended policies.
  RECOMMENDED_POLICIES=()
  local name
  while IFS= read -r name; do
    [[ -n "$name" ]] && RECOMMENDED_POLICIES+=("$name")
  done < <(
    kctl get clusteradmissionpolicies \
      -o jsonpath='{range .items[?(@.metadata.annotations.meta\.helm\.sh/release-name=="kubewarden-defaults")]}{.metadata.name}{"\n"}{end}' 2>/dev/null
  )
  if (( ${#RECOMMENDED_POLICIES[@]} > 0 )); then
    ok "found ${#RECOMMENDED_POLICIES[@]} recommended policies: ${RECOMMENDED_POLICIES[*]}"
  else
    info "no recommended ClusterAdmissionPolicies found from kubewarden-defaults (this is fine if they were not enabled)"
  fi
}

#------------------------------------------------------------------------------
# Phase 2: Snapshot resource UIDs
#------------------------------------------------------------------------------
phase_snapshot() {
  step "Phase 2: Snapshot critical resource UIDs"

  # These are used in phase 5 to verify adoption was in-place (no recreate).
  SNAPSHOT_SA_UID="$(kctl -n "$KW_NAMESPACE" get sa policy-server -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  SNAPSHOT_CROLE_UID="$(kctl get clusterrole kubewarden-context-watcher -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  SNAPSHOT_CRB_UID="$(kctl get clusterrolebinding kubewarden-context-watcher -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  SNAPSHOT_DEP_UID="$(kctl -n "$KW_NAMESPACE" get deployment policy-server-default -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"

  [[ -n "$SNAPSHOT_SA_UID" ]] && info "ServiceAccount/policy-server UID: $SNAPSHOT_SA_UID" || warn "ServiceAccount/policy-server not found (may not exist if defaults chart did not create it)"
  [[ -n "$SNAPSHOT_CROLE_UID" ]] && info "ClusterRole/kubewarden-context-watcher UID: $SNAPSHOT_CROLE_UID" || warn "ClusterRole/kubewarden-context-watcher not found"
  [[ -n "$SNAPSHOT_CRB_UID" ]] && info "ClusterRoleBinding/kubewarden-context-watcher UID: $SNAPSHOT_CRB_UID" || warn "ClusterRoleBinding/kubewarden-context-watcher not found"
  [[ -n "$SNAPSHOT_DEP_UID" ]] && info "Deployment/policy-server-default UID: $SNAPSHOT_DEP_UID" || warn "Deployment/policy-server-default not found"
}

#------------------------------------------------------------------------------
# Phase 3: Inject keep annotations via post-renderer plugin
#------------------------------------------------------------------------------
phase_inject_keep_annotations() {
  step "Phase 3: Inject helm.sh/resource-policy=keep annotations"

  # Create the post-renderer plugin.
  mkdir -p "$POSTRENDER_PLUGIN_DIR"
  cat > "$POSTRENDER_PLUGIN_DIR/plugin.yaml" <<'EOF'
apiVersion: v1
type: postrenderer/v1
name: kw-keep-postrenderer
version: 0.1.0
runtime: subprocess
runtimeConfig:
  platformCommand:
    - command: yq
EOF
  info "post-renderer plugin created at: $POSTRENDER_PLUGIN_DIR/"
  info "plugin.yaml:"
  sed 's/^/      /' "$POSTRENDER_PLUGIN_DIR/plugin.yaml"

  info "installing post-renderer plugin"
  hctl plugin install "$POSTRENDER_PLUGIN_DIR" 2>/dev/null \
    || hctl plugin update kw-keep-postrenderer 2>/dev/null \
    || true
  ok "plugin kw-keep-postrenderer installed"

  confirm "About to run 'helm upgrade --reuse-values --post-renderer' on all 3 legacy releases to inject keep annotations."

  local entry release chart version
  for entry in "${LEGACY_RELEASES[@]}"; do
    release="${entry%%=*}"
    chart="${entry##*=}"

    version="$(hctl get metadata "$release" -n "$KW_NAMESPACE" -o json | jq -r '.version')"
    [[ -n "$version" && "$version" != "null" ]] \
      || fail "could not determine installed chart version for $release"

    # Per-release keep policy:
    #  - crds / defaults: keep ALL rendered docs (data + identity).
    #  - controller: keep ONLY the kubewarden-ca Secret (the root of trust).
    #    The unified chart's webhooks.yaml lookup reuses it, so the running
    #    policy-server pods and the policy webhook caBundles stay valid (no CA
    #    churn). The leaf certs (kubewarden-webhook-server-cert,
    #    kubewarden-audit-scanner-client-cert) are intentionally NOT kept: the
    #    webhook server cert's SAN is bound to the webhook Service DNS name,
    #    which changes when the controller plumbing is recreated under the
    #    chart's natural names. Keeping the stale leaf cert would make the
    #    controller serve a cert valid for the OLD service name and every kb.io
    #    webhook call would fail TLS verification. Deleting them lets the unified
    #    install regenerate them from the preserved CA with the correct SAN. The
    #    Deployment, Services, ConfigMap, SA, RBAC and webhook configs are also
    #    NOT kept, so `helm uninstall` deletes them and the unified install
    #    recreates them with the chart's natural names.
    local keep_expr
    if [[ "$chart" == "kubewarden-controller" ]]; then
      keep_expr='select(. != null) | (select(.kind == "Secret" and .metadata.name == "kubewarden-ca") | .metadata.annotations."helm.sh/resource-policy") = "keep"'
    else
      keep_expr='select(. != null) | .metadata.annotations."helm.sh/resource-policy" = "keep"'
    fi

    info "$release: upgrading with post-renderer (chart version $version, --reuse-values)"

    if dry_run_guard "helm upgrade $release $HELM_REPO_NAME/$chart --version $version --reuse-values --post-renderer kw-keep-postrenderer"; then
      hctl upgrade "$release" "$HELM_REPO_NAME/$chart" \
        --version "$version" \
        --namespace "$KW_NAMESPACE" \
        --reuse-values \
        --post-renderer kw-keep-postrenderer \
        --post-renderer-args "eval" \
        --post-renderer-args "$keep_expr" \
        --post-renderer-args "-" \
        --wait --timeout "$WAIT_TIMEOUT"
      ok "$release: keep annotation baked into stored manifest"
    fi
  done

  if (( DRY_RUN == 0 )); then
    step "Verifying keep annotations on critical resources"
    local v

    for crd in "${KW_CRDS[@]}"; do
      v="$(kctl get crd "$crd" -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}' 2>/dev/null || true)"
      [[ "$v" == "keep" ]] \
        || fail "CRD $crd is missing helm.sh/resource-policy=keep after upgrade"
      ok "$crd has keep annotation"
    done

    # Only kubewarden-ca is kept from the controller release; the leaf certs are
    # intentionally left unkept so the unified install regenerates them.
    v="$(kctl -n "$KW_NAMESPACE" get secret kubewarden-ca -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}' 2>/dev/null || true)"
    [[ "$v" == "keep" ]] \
      || fail "Secret/kubewarden-ca is missing keep annotation after upgrade"
    ok "Secret/kubewarden-ca has keep annotation"

    v="$(kctl get policyserver default -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}' 2>/dev/null || true)"
    [[ "$v" == "keep" ]] \
      || warn "PolicyServer/default does not have keep annotation (may not exist if defaults were not enabled)"

    v="$(kctl -n "$KW_NAMESPACE" get sa policy-server -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}' 2>/dev/null || true)"
    [[ "$v" == "keep" ]] \
      || warn "ServiceAccount/policy-server does not have keep annotation"

    v="$(kctl get clusterrole kubewarden-context-watcher -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}' 2>/dev/null || true)"
    [[ "$v" == "keep" ]] \
      || warn "ClusterRole/kubewarden-context-watcher does not have keep annotation"

    v="$(kctl get clusterrolebinding kubewarden-context-watcher -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}' 2>/dev/null || true)"
    [[ "$v" == "keep" ]] \
      || warn "ClusterRoleBinding/kubewarden-context-watcher does not have keep annotation"
  fi
}

#------------------------------------------------------------------------------
# Phase 4: Uninstall legacy releases
#------------------------------------------------------------------------------
phase_uninstall_legacy() {
  step "Phase 4: Uninstall legacy releases (--no-hooks)"

  confirm "About to uninstall the three legacy Helm releases with --no-hooks. CRDs, CA cert Secret, RBAC, PolicyServers, and policies are kept. The controller goes down for a short time. Existing policy enforcement keeps running, but new/updated PolicyServers and policies will not be reconciled until the unified chart is installed."

  # Reverse order: defaults, controller, crds.
  local releases_reversed=("kubewarden-defaults" "kubewarden-controller" "kubewarden-crds")
  local rel
  for rel in "${releases_reversed[@]}"; do
    info "uninstalling $rel"
    if dry_run_guard "helm uninstall $rel --namespace $KW_NAMESPACE --no-hooks --wait"; then
      hctl uninstall "$rel" --namespace "$KW_NAMESPACE" --no-hooks --wait || true
      ok "$rel uninstalled"
    fi
  done

  if (( DRY_RUN == 1 )); then return; fi

  step "Verifying resources survived uninstall"

  info "checking CRDs"
  for crd in "${KW_CRDS[@]}"; do
    kctl get crd "$crd" >/dev/null 2>&1 \
      || fail "CRD $crd was deleted during uninstall"
    ok "$crd survived"
  done

  info "checking kubewarden-ca Secret (root of trust; leaf certs are regenerated by the unified install)"
  kctl -n "$KW_NAMESPACE" get secret kubewarden-ca >/dev/null 2>&1 \
    || fail "Secret/kubewarden-ca was deleted during uninstall"
  ok "Secret/kubewarden-ca survived"

  info "checking default PolicyServer"
  if kctl get policyserver default >/dev/null 2>&1; then
    ok "PolicyServer/default survived"
  else
    warn "PolicyServer/default not found (may not have been deployed)"
  fi

  info "checking ServiceAccount/policy-server"
  if kctl -n "$KW_NAMESPACE" get sa policy-server >/dev/null 2>&1; then
    ok "ServiceAccount/policy-server survived"
  else
    warn "ServiceAccount/policy-server not found"
  fi

  info "checking kubewarden-context-watcher RBAC"
  if kctl get clusterrole kubewarden-context-watcher >/dev/null 2>&1; then
    ok "ClusterRole/kubewarden-context-watcher survived"
  else
    warn "ClusterRole/kubewarden-context-watcher not found"
  fi
  if kctl get clusterrolebinding kubewarden-context-watcher >/dev/null 2>&1; then
    ok "ClusterRoleBinding/kubewarden-context-watcher survived"
  else
    warn "ClusterRoleBinding/kubewarden-context-watcher not found"
  fi

  for p in "${RECOMMENDED_POLICIES[@]}"; do
    if kctl get clusteradmissionpolicy "$p" >/dev/null 2>&1; then
      ok "ClusterAdmissionPolicy/$p survived"
    else
      warn "ClusterAdmissionPolicy/$p not found after uninstall"
    fi
  done

  info "checking legacy controller plumbing was removed (expected)"
  if kctl -n "$KW_NAMESPACE" get deployment \
      -l "app.kubernetes.io/instance=$LEGACY_CONTROLLER_RELEASE_NAME,app.kubernetes.io/component=controller" \
      --no-headers 2>/dev/null | grep -q .; then
    warn "legacy controller Deployment still present (expected to be deleted; will be reconciled by the unified install)"
  else
    ok "legacy controller Deployment removed (will be recreated by the unified install)"
  fi
}

#------------------------------------------------------------------------------
# Build the merged values file from the three legacy releases.
#
# The unified chart's values are the concatenation of the three legacy charts'
# values (their key namespaces are effectively disjoint), so we simply append
# each release's user-supplied values into one file — no deep merge. Runs before
# the legacy uninstall because `helm get values` needs the releases to exist.
#
# name/fullname overrides and the three component image tags (image.tag,
# auditScanner.image.tag, policyServer.image.tag) are stripped from each
# release's values so the unified chart's appVersion images are used (a stale
# tag would crashloop the controller — see issue #1852).
#------------------------------------------------------------------------------
phase_build_merged_values() {
  step "Phase: build merged values from legacy releases"

  # Refuse to clobber an existing file (it may be a previous run's output or a
  # user-managed file holding real config). Prompt in interactive mode, fail
  # fast otherwise.
  if [[ -e "$MERGED_VALUES_FILE" ]]; then
    if (( INTERACTIVE == 1 )); then
      confirm "merged values file $MERGED_VALUES_FILE already exists and will be overwritten."
    else
      fail "merged values file $MERGED_VALUES_FILE already exists; remove it or set MERGED_VALUES_FILE to a new path"
    fi
  fi

  : > "$MERGED_VALUES_FILE"

  local entry release vals
  for entry in "${LEGACY_RELEASES[@]}"; do
    release="${entry%%=*}"
    vals="$(hctl get values "$release" -n "$KW_NAMESPACE" -o yaml 2>/dev/null || true)"
    # `helm get values -o yaml` prints "null" (or nothing) when the release has
    # no user-supplied values; skip those.
    if [[ -z "$vals" || "$vals" == "null" ]]; then
      info "$release: no user-supplied values"
      continue
    fi
    # Drop name/fullname overrides AND the three component image tags:
    #  - name/fullname overrides: the unified chart recreates the controller
    #    plumbing with its own natural names, so no override is needed. (Also
    #    resolves the case where a legacy release set fullnameOverride.)
    #  - component image tags: the unified chart's bundled tags must match its
    #    appVersion. A stale tag (e.g. image.tag=v1.36.0 from a --set/-f on the
    #    legacy stack) would pin an old image that lacks the new
    #    --defaults-configmap-name flag and crashloop, failing `helm install
    #    --wait`. Custom repositories (private mirrors) are preserved.
    # Deleting an absent path is a no-op in yq, so one expression covers all
    # three legacy releases.
    vals="$(printf '%s\n' "$vals" | yq '
      del(.nameOverride) | del(.fullnameOverride) |
      del(.image.tag) | del(.auditScanner.image.tag) | del(.policyServer.image.tag)
    ')"

    # If the override(s) were the ONLY user-supplied values, stripping them
    # leaves an empty map, which yq renders as "{}". Appending that to the
    # merged file produces invalid YAML (a flow mapping followed by block keys),
    # and Helm then silently reads only the "{}" document and DROPS every later
    # release's values — e.g. a kubewarden-defaults `recommendedPolicies.enabled=true`
    # would be lost, leaving recommended policies unmanaged after the migration.
    # Skip releases that have nothing left after stripping.
    local trimmed
    trimmed="$(printf '%s' "$vals" | tr -d '[:space:]')"
    if [[ -z "$trimmed" || "$trimmed" == "{}" || "$trimmed" == "null" ]]; then
      info "$release: no user-supplied values after stripping name/fullname overrides"
      continue
    fi

    info "$release: appending user-supplied values"
    {
      printf '# values from legacy release: %s\n' "$release"
      printf '%s\n' "$vals"
    } >> "$MERGED_VALUES_FILE"
  done

  ok "merged values written to $MERGED_VALUES_FILE"
  # The merged values come from `helm get values` and may carry sensitive
  # user-supplied config, so only dump the full contents under --verbose;
  # otherwise just point at the file.
  if (( VERBOSE == 1 )); then
    info "merged values:"
    sed 's/^/      /' "$MERGED_VALUES_FILE"
  else
    info "re-run with --verbose to print the merged values"
  fi
}

#------------------------------------------------------------------------------
# Phase 5: Install unified chart
#------------------------------------------------------------------------------
phase_install_unified() {
  step "Phase 5: Install unified chart with --take-ownership"

  local release_name="$UNIFIED_RELEASE_NAME"
  info "release name: $release_name (fresh unified release; adopts kept resources via --take-ownership)"
  info "chart source: $UNIFIED_CHART"
  info "namespace: $KW_NAMESPACE"

  confirm "About to run 'helm install $release_name --take-ownership' to adopt the kept resources and recreate the controller plumbing with the chart's natural names."

  # Install from the merged legacy values (see phase_build_merged_values) instead
  # of hardcoded --set flags, so the migrated release keeps the configuration the
  # user actually had on the legacy stack. The merged file carries NO name/fullname
  # overrides — the release is vanilla.
  #
  # Adoption model:
  #  - Constant-named data/identity resources (the 5 CRDs, the kubewarden-ca Secret,
  #    the policy-server SA, the kubewarden-context-watcher RBAC, the default
  #    PolicyServer and recommended policies) were kept through the legacy
  #    uninstall and are adopted into this release via Helm 4 Server-Side Apply
  #    (--take-ownership). Their names are literal/constant, so they render
  #    identically under the unified chart regardless of release name.
  #  - The controller's stateless plumbing (Deployment, Services, ConfigMap, SA,
  #    RBAC, webhook configs) is created FRESH under the chart's natural names.
  #    The legacy copies were deleted in phase 4, so there is nothing to adopt and
  #    no immutable-selector conflict — the hardened controller selector
  #    (component+instance, independent of the chart name) renders cleanly.
  #  - Only the kubewarden-ca Secret was kept, so the chart's webhooks.yaml
  #    `lookup` reuses the existing CA (no CA churn → running policy-server pods
  #    and policy webhook caBundles stay valid). The leaf certs
  #    (webhook-server-cert, audit-scanner-client-cert) were NOT kept and are
  #    regenerated from the preserved CA with SANs that match the recreated
  #    webhook Service name (keeping the old leaf cert would serve a SAN valid
  #    only for the old service name and break every kb.io webhook call).
  #
  # User-passed --set/--values (HELM_INSTALL_EXTRA_ARGS) come last so they
  # override the merged values. These values are stored in the release, so future
  # `helm upgrade --reuse-values` keeps them.
  # When the chart source is a repo reference (not a local file or directory),
  # resolve the chart version whose appVersion is v1.37.0 and pin the install
  # to that exact chart version.
  local helm_version_flag=()
  if [[ ! -f "$UNIFIED_CHART" && ! -d "$UNIFIED_CHART" ]]; then
    info "repo chart detected: resolving chart version for appVersion v1.37.0"
    local chart_version_for_app
    chart_version_for_app="$(hctl search repo "$UNIFIED_CHART" --versions -o json \
      | jq -r '[.[] | select(.app_version == "v1.37.0")] | first | .version // empty' 2>/dev/null || true)"
    [[ -n "$chart_version_for_app" ]] \
      || fail "could not find a chart in '$UNIFIED_CHART' with appVersion v1.37.0; ensure the repo is up to date"
    helm_version_flag=(--version "$chart_version_for_app")
    info "resolved chart version $chart_version_for_app for appVersion v1.37.0"
  fi

  if dry_run_guard "helm install $release_name $UNIFIED_CHART --namespace $KW_NAMESPACE --take-ownership --values $MERGED_VALUES_FILE ${helm_version_flag[*]:-} ${HELM_INSTALL_EXTRA_ARGS[*]:-}"; then
    hctl install "$release_name" "$UNIFIED_CHART" \
      --namespace "$KW_NAMESPACE" \
      --take-ownership \
      --values "$MERGED_VALUES_FILE" \
      "${helm_version_flag[@]}" \
      "${HELM_INSTALL_EXTRA_ARGS[@]}" \
      --wait --timeout "$WAIT_TIMEOUT"
    ok "unified chart installed"
  fi

  if (( DRY_RUN == 1 )); then return; fi

  step "Post-migration verification"

  # Verify CRD ownership.
  info "verifying CRDs are owned by release '$release_name'"
  local rel
  for crd in "${KW_CRDS[@]}"; do
    rel="$(kctl get crd "$crd" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null || true)"
    [[ "$rel" == "$release_name" ]] \
      || fail "CRD $crd is not owned by '$release_name' (got: '$rel')"
    ok "$crd owned by $release_name"
  done

  # Verify RBAC adoption.
  info "verifying RBAC adoption"
  rel="$(kctl -n "$KW_NAMESPACE" get sa policy-server -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null || true)"
  if [[ "$rel" == "$release_name" ]]; then
    ok "ServiceAccount/policy-server owned by $release_name"
    if [[ -n "$SNAPSHOT_SA_UID" ]]; then
      local uid_after
      uid_after="$(kctl -n "$KW_NAMESPACE" get sa policy-server -o jsonpath='{.metadata.uid}')"
      [[ "$SNAPSHOT_SA_UID" == "$uid_after" ]] \
        && ok "ServiceAccount/policy-server UID unchanged (in-place adoption)" \
        || warn "ServiceAccount/policy-server UID changed (before=$SNAPSHOT_SA_UID after=$uid_after)"
    fi
  else
    warn "ServiceAccount/policy-server not adopted by '$release_name' (got: '$rel')"
  fi

  rel="$(kctl get clusterrole kubewarden-context-watcher -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null || true)"
  if [[ "$rel" == "$release_name" ]]; then
    ok "ClusterRole/kubewarden-context-watcher owned by $release_name"
  else
    warn "ClusterRole/kubewarden-context-watcher not adopted (got: '$rel')"
  fi

  rel="$(kctl get clusterrolebinding kubewarden-context-watcher -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null || true)"
  if [[ "$rel" == "$release_name" ]]; then
    ok "ClusterRoleBinding/kubewarden-context-watcher owned by $release_name"
  else
    warn "ClusterRoleBinding/kubewarden-context-watcher not adopted (got: '$rel')"
  fi

  # Verify keep annotations were stripped by SSA.
  info "verifying legacy keep annotations were stripped from chart-rendered resources"
  local keep_val
  for res_desc in \
    "-n $KW_NAMESPACE sa policy-server" \
    "clusterrole kubewarden-context-watcher" \
    "clusterrolebinding kubewarden-context-watcher" \
    "-n $KW_NAMESPACE secret kubewarden-ca"; do
    keep_val="$(kctl get $res_desc -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}' 2>/dev/null || true)"
    if [[ -z "$keep_val" ]]; then
      ok "$res_desc: legacy keep annotation stripped"
    else
      warn "$res_desc still carries helm.sh/resource-policy='$keep_val'"
    fi
  done

  # Verify controller pod is running. Locate the freshly-created Deployment by
  # the hardened selector labels rather than by a computed name.
  info "checking controller Deployment (by label selector)"
  local sel="app.kubernetes.io/component=controller,app.kubernetes.io/instance=${release_name}"
  local controller_dep
  controller_dep="$(kctl -n "$KW_NAMESPACE" get deployment -l "$sel" \
                     -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [[ -n "$controller_dep" ]]; then
    local ready
    ready="$(kctl -n "$KW_NAMESPACE" get deployment "$controller_dep" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    if [[ -n "$ready" && "$ready" -ge 1 ]]; then
      ok "controller Deployment '$controller_dep' is running ($ready ready replicas)"
    else
      warn "controller Deployment '$controller_dep' has ${ready:-0} ready replicas"
    fi
  else
    warn "controller Deployment not found via selector '$sel'"
  fi

  # Verify default PolicyServer Deployment UID unchanged.
  if [[ -n "$SNAPSHOT_DEP_UID" ]]; then
    local dep_uid_after
    dep_uid_after="$(kctl -n "$KW_NAMESPACE" get deployment policy-server-default -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
    if [[ "$SNAPSHOT_DEP_UID" == "$dep_uid_after" ]]; then
      ok "Deployment/policy-server-default UID unchanged (no restart)"
    else
      warn "Deployment/policy-server-default UID changed (before=$SNAPSHOT_DEP_UID after=$dep_uid_after)"
    fi
  fi

  # Wait for DefaultsApplier to label resources.
  if kctl get policyserver default >/dev/null 2>&1; then
    wait_managed_by_label clusterscoped policyserver default
  fi
  for p in "${RECOMMENDED_POLICIES[@]}"; do
    if kctl get clusteradmissionpolicy "$p" >/dev/null 2>&1; then
      wait_managed_by_label clusterscoped clusteradmissionpolicy "$p"
    fi
  done
}

#------------------------------------------------------------------------------
# Run
#------------------------------------------------------------------------------
phase_preflight
phase_snapshot
phase_build_merged_values
phase_inject_keep_annotations
phase_uninstall_legacy
phase_install_unified

step "Migration completed successfully"
info "The unified chart is installed as release '$UNIFIED_RELEASE_NAME' in namespace '$KW_NAMESPACE'."
info "The post-renderer plugin directory is at: $POSTRENDER_PLUGIN_DIR/ (kept for inspection)"
info "Log saved to: $LOG_FILE"
info ""
info "Next steps:"
info "  - Verify your PolicyServers and policies are working as expected."
info "  - The release '$UNIFIED_RELEASE_NAME' is now VANILLA: it stores NO name/fullname"
info "    overrides, so future upgrades need no special values, e.g.:"
info "      helm upgrade $UNIFIED_RELEASE_NAME $UNIFIED_CHART -n $KW_NAMESPACE --reuse-values"
info "  - The merged values file ($MERGED_VALUES_FILE) only carried your prior user"
info "    config; you can discard it once 'helm get values $UNIFIED_RELEASE_NAME -n $KW_NAMESPACE'"
info "    looks correct."
info "  - Any component image tag you had pinned (controller/audit-scanner/policy-server)"
info "    was intentionally dropped so the chart's appVersion images are used. To pin a"
info "    specific patch tag on the new version, set it afterward, e.g.:"
info "      helm upgrade $UNIFIED_RELEASE_NAME $UNIFIED_CHART -n $KW_NAMESPACE --reuse-values \\\n        --set image.tag=<tag> --set auditScanner.image.tag=<tag> --set policyServer.image.tag=<tag>"
