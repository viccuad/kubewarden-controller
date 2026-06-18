#!/usr/bin/env bash
# kubewarden-unified-adm-controller-chart-migration.sh
#
# Migrates an existing Kubewarden installation from the legacy three-chart
# stack (kubewarden-crds, kubewarden-controller, kubewarden-defaults) to
# the unified single kubewarden-controller chart — with zero downtime.
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
#      Runs `helm install --take-ownership` with the unified chart. The
#      release name must match the legacy kubewarden-controller release
#      name to avoid immutable selector conflicts. Helm 4 uses
#      Server-Side Apply to adopt existing resources and strip the legacy
#      keep annotations from chart-rendered resources.
#
#   5. Post-migration verification
#      Confirms CRDs are owned by the new release, the controller
#      Deployment is running, RBAC was adopted in place (UIDs unchanged),
#      and the DefaultsApplier has labeled the default PolicyServer and
#      recommended policies.
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
#       [--repo-url URL]
#       [--timeout DURATION]
#       [--set KEY=VALUE]
#       [--values FILE]
#       [--interactive]
#       [--dry-run]
#       [--help]
#
# Examples:
#   # Migrate using a local chart tarball:
#   ./kubewarden-unified-adm-controller-chart-migration.sh \
#       --unified-chart ./kubewarden-controller-6.0.0.tgz
#
#   # Migrate using the chart from the Helm repo:
#   ./kubewarden-unified-adm-controller-chart-migration.sh \
#       --unified-chart kubewarden/kubewarden-controller
#
#   # Dry run (no changes):
#   ./kubewarden-unified-adm-controller-chart-migration.sh \
#       --unified-chart ./kubewarden-controller-6.0.0.tgz --dry-run

set -euo pipefail

#------------------------------------------------------------------------------
# Defaults
#------------------------------------------------------------------------------
KW_NAMESPACE="${KW_NAMESPACE:-kubewarden}"
KUBE_CONTEXT=""
UNIFIED_CHART=""
HELM_REPO_NAME="${HELM_REPO_NAME:-kubewarden}"
HELM_REPO_URL="${HELM_REPO_URL:-https://charts.kubewarden.io}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-5m}"
INTERACTIVE=0
DRY_RUN=0

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
    --repo-url)
      require_flag_value "$1" "${2:-}"; HELM_REPO_URL="$2"; shift 2 ;;
    --repo-url=*)
      HELM_REPO_URL="${1#--repo-url=}"; shift ;;
    --timeout)
      require_flag_value "$1" "${2:-}"; WAIT_TIMEOUT="$2"; shift 2 ;;
    --timeout=*)
      WAIT_TIMEOUT="${1#--timeout=}"; shift ;;
    --interactive)
      INTERACTIVE=1; shift ;;
    --dry-run)
      DRY_RUN=1; shift ;;
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

  # Ensure the Helm repo is configured and up to date.
  info "configuring Helm repo $HELM_REPO_NAME ($HELM_REPO_URL)"
  hctl repo add "$HELM_REPO_NAME" "$HELM_REPO_URL" >/dev/null 2>&1 || true
  hctl repo update "$HELM_REPO_NAME" >/dev/null
  ok "Helm repo $HELM_REPO_NAME is up to date"

  # Detect legacy releases.
  info "detecting legacy Kubewarden releases in namespace $KW_NAMESPACE"
  local entry release chart version latest_version
  for entry in "${LEGACY_RELEASES[@]}"; do
    release="${entry%%=*}"
    chart="${entry##*=}"

    if ! hctl status "$release" -n "$KW_NAMESPACE" >/dev/null 2>&1; then
      fail "legacy release '$release' not found in namespace '$KW_NAMESPACE'"
    fi

    version="$(hctl get metadata "$release" -n "$KW_NAMESPACE" -o json | jq -r '.version')"
    [[ -n "$version" && "$version" != "null" ]] \
      || fail "could not determine installed chart version for $release"

    latest_version="$(hctl search repo "$HELM_REPO_NAME/$chart" -o json | jq -r '.[0].version' 2>/dev/null || true)"

    if [[ -n "$latest_version" && "$latest_version" != "null" && "$version" != "$latest_version" ]]; then
      warn "$release is at chart version $version but latest in repo is $latest_version"
      warn "it is recommended to upgrade to the latest version before migrating"
    else
      ok "$release is at chart version $version (latest)"
    fi

    # Remember the kubewarden-controller release name for unified chart install.
    if [[ "$chart" == "kubewarden-controller" ]]; then
      LEGACY_CONTROLLER_RELEASE_NAME="$release"
    fi
  done

  [[ -n "$LEGACY_CONTROLLER_RELEASE_NAME" ]] \
    || fail "could not determine the legacy kubewarden-controller release name"
  info "unified chart will be installed with release name: $LEGACY_CONTROLLER_RELEASE_NAME"

  # Validate unified chart source.
  if [[ -f "$UNIFIED_CHART" ]]; then
    info "unified chart source: local tarball ($UNIFIED_CHART)"
  else
    info "unified chart source: repo chart ($UNIFIED_CHART)"
  fi

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

    info "$release: upgrading with post-renderer (chart version $version, --reuse-values)"

    if dry_run_guard "helm upgrade $release $HELM_REPO_NAME/$chart --version $version --reuse-values --post-renderer kw-keep-postrenderer"; then
      hctl upgrade "$release" "$HELM_REPO_NAME/$chart" \
        --version "$version" \
        --namespace "$KW_NAMESPACE" \
        --reuse-values \
        --post-renderer kw-keep-postrenderer \
        --post-renderer-args "eval" \
        --post-renderer-args 'select(. != null) | .metadata.annotations."helm.sh/resource-policy" = "keep"' \
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

    v="$(kctl -n "$KW_NAMESPACE" get secret kubewarden-ca -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}' 2>/dev/null || true)"
    [[ "$v" == "keep" ]] \
      || fail "Secret/kubewarden-ca is missing keep annotation"
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

  confirm "About to uninstall the three legacy Helm releases with --no-hooks. Resources will be preserved by keep annotations."

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

  info "checking kubewarden-ca Secret"
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
}

#------------------------------------------------------------------------------
# Phase 5: Install unified chart
#------------------------------------------------------------------------------
phase_install_unified() {
  step "Phase 5: Install unified chart with --take-ownership"

  local release_name="$LEGACY_CONTROLLER_RELEASE_NAME"
  info "release name: $release_name (matches legacy kubewarden-controller release)"
  info "chart source: $UNIFIED_CHART"
  info "namespace: $KW_NAMESPACE"

  confirm "About to run 'helm install $release_name --take-ownership' to adopt existing resources."

  if dry_run_guard "helm install $release_name $UNIFIED_CHART --namespace $KW_NAMESPACE --take-ownership --set policyServer.enabled=true --set recommendedPolicies.enabled=true ${HELM_INSTALL_EXTRA_ARGS[*]:-}"; then
    hctl install "$release_name" "$UNIFIED_CHART" \
      --namespace "$KW_NAMESPACE" \
      --take-ownership \
      --set "policyServer.enabled=true" \
      --set "recommendedPolicies.enabled=true" \
      --set "recommendedPolicies.defaultPolicyMode=protect" \
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

  # Verify controller pod is running.
  info "checking controller Deployment"
  local controller_dep="${release_name}"
  if kctl -n "$KW_NAMESPACE" get deployment "$controller_dep" >/dev/null 2>&1; then
    local ready
    ready="$(kctl -n "$KW_NAMESPACE" get deployment "$controller_dep" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    if [[ -n "$ready" && "$ready" -ge 1 ]]; then
      ok "controller Deployment '$controller_dep' is running ($ready ready replicas)"
    else
      warn "controller Deployment '$controller_dep' has $ready ready replicas"
    fi
  else
    warn "controller Deployment '$controller_dep' not found"
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
phase_inject_keep_annotations
phase_uninstall_legacy
phase_install_unified

step "Migration completed successfully"
info "The unified chart is installed as release '$LEGACY_CONTROLLER_RELEASE_NAME' in namespace '$KW_NAMESPACE'."
info "The post-renderer plugin directory is at: $POSTRENDER_PLUGIN_DIR/ (kept for inspection)"
info "Log saved to: $LOG_FILE"
info ""
info "Next steps:"
info "  - Verify your PolicyServers and policies are working as expected"
