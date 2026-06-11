# Kubewarden Admission Controller

Helm chart for deploying the Kubewarden admission control stack.

> **Note:** This chart replaces the three separate charts that were used
> previously: `kubewarden-crds`, `kubewarden-controller`, and
> `kubewarden-defaults`.

## Installation

```sh
helm install kubewarden kubewarden/kubewarden-controller -n kubewarden --create-namespace
```

## Migration from three-chart setup

If you are running the legacy three-chart setup (`kubewarden-crds`,
`kubewarden-controller`, `kubewarden-defaults`), use the migration
script (`kubewarden-unified-adm-controller-chart-migration.sh`) to
move to this chart without admission downtime.

The migration requires several Helm operations in a specific order:
adding resource-preservation annotations to the stored release
manifests, uninstalling the legacy releases without running cleanup
hooks, then installing the unified chart so it adopts the existing
resources. The ordering matters because annotations must be in the
stored manifest (not just on live objects) for Helm to honor them,
hooks must be skipped or they delete PolicyServers, and the release
name must match the old one or Kubernetes rejects the update due to
immutable selectors. Getting any step wrong can delete resources or
break admission, so the script handles it all and verifies each step.

What survives the migration:

- The five Kubewarden CRDs.
- Your custom `PolicyServer` instances and policy CRs, along with
  their `Validating`/`MutatingWebhookConfiguration` objects.
- The `default` `PolicyServer` and recommended policy CRs (if
  enabled). The unified chart's DefaultsApplier adopts them in
  place. The `policy-server-default` Deployment is owned by the
  `PolicyServer` CR through an owner reference, so as long as the
  CR survives, the Deployment stays up and admission continues.
- The `kubewarden-ca` Secret. Already-running policy-server pods
  stay trusted by the new webhook CA bundles. No TLS rotation
  happens.

### Prerequisites

- Helm v4+ (needed for Server-Side Apply and post-renderer plugins)
- kubectl with access to your cluster
- yq v4 (github.com/mikefarah/yq) for the post-renderer
- jq for detecting installed chart versions
- The three legacy releases must be installed in your cluster

### What the script does

The script runs five phases:

1. Preflight: checks that the required tools are available, connects
   to the cluster, finds the three legacy releases, and warns if
   they are not at the latest chart version.
2. Annotation injection: creates a Helm 4 post-renderer plugin that
   wraps `yq`, then runs `helm upgrade --reuse-values --post-renderer`
   on each legacy release to write `helm.sh/resource-policy: keep`
   into the stored release manifests.
3. Legacy uninstall: runs `helm uninstall --no-hooks` in reverse
   order. The `--no-hooks` flag skips the controller chart's
   pre-delete hook, which would otherwise delete all PolicyServers.
   Resources stay live in the cluster.
4. Unified chart install: runs `helm install --take-ownership`.
   Helm 4's Server-Side Apply adopts existing resources, updates
   their ownership metadata, and removes the legacy keep annotations
   from chart-rendered resources.
5. Verification: checks that CRDs are owned by the new release,
   RBAC resources were adopted in place (UIDs did not change), the
   controller pod is running, and the DefaultsApplier has labeled
   the default PolicyServer and recommended policies.

### Running the migration

```sh
./kubewarden-unified-adm-controller-chart-migration.sh \
  --unified-chart kubewarden/kubewarden-controller
```

Using a local tarball:

```sh
./kubewarden-unified-adm-controller-chart-migration.sh \
  --unified-chart ./kubewarden-controller-6.0.0.tgz
```

Dry run (no changes applied):

```sh
./kubewarden-unified-adm-controller-chart-migration.sh \
  --unified-chart kubewarden/kubewarden-controller --dry-run
```

Interactive mode (pauses before each destructive step):

```sh
./kubewarden-unified-adm-controller-chart-migration.sh \
  --unified-chart kubewarden/kubewarden-controller --interactive
```

Passing custom values to the unified chart:

```sh
./kubewarden-unified-adm-controller-chart-migration.sh \
  --unified-chart kubewarden/kubewarden-controller \
  --set "image.tag=v2.0.0" \
  --values ./my-custom-values.yaml
```

#### Available flags

| Flag | Description |
|------|-------------|
| `--unified-chart PATH_OR_NAME` | Required. Local tarball or Helm repo chart name |
| `--namespace NS` | Namespace of the Kubewarden installation (default: `kubewarden`) |
| `--kube-context CTX` | Kubernetes context to use (default: current context) |
| `--repo-name NAME` | Helm repo name (default: `kubewarden`) |
| `--repo-url URL` | Helm repo URL (default: `https://charts.kubewarden.io`) |
| `--timeout DURATION` | Timeout for Helm operations (default: `5m`) |
| `--set KEY=VALUE` | Set a value for the unified chart install (repeatable) |
| `--values FILE` / `-f FILE` | Values file for the unified chart install (repeatable) |
| `--interactive` | Pause for confirmation before destructive steps |
| `--dry-run` | Show what would be done without making changes |
| `--help` | Print usage information |

### Caveats

**Release name.** The unified chart must be installed with the same
release name as the legacy `kubewarden-controller` release (usually
`kubewarden-controller`). Kubernetes Deployments have an immutable
`spec.selector.matchLabels` that includes
`app.kubernetes.io/instance: <release-name>`. If the name does not
match, the install fails with an immutable-field error. The script
detects the legacy release name automatically.

**Controller gap.** Between the legacy uninstall and the unified
chart becoming ready, no controller is running. Existing webhook
configurations are still served by the surviving policy-server pods,
so admission for active policies keeps working. New policy CRs
created during this window are not reconciled into webhooks until
the new controller starts.

**Settings drift.** The DefaultsApplier rewrites each recommended
policy's spec to match the values you pass to the unified chart. If
you had changed `mode`, `settings`, or other fields on the
recommended policies, pass those same values with `--set` or
`--values` to preserve your configuration.

**Renamed policies.** The unified chart's default policy names match
the legacy defaults. If you renamed any through legacy values, pass
the same `name` overrides with `--set` or `--values`. Otherwise the
applier removes the old-named CRs after install.

**Custom RBAC.** If you added extra permissions to
`kubewarden-context-watcher` by hand, the unified chart overwrites
them on install. Include those permissions in a values file or pass
them with `--set "policyServer.permissions[0].apiGroup=..."`.

## Configuration

### Defaults

The chart can deploy a default Policy Server and recommended policies:

```yaml
policyServer:
  enabled: true
  replicaCount: 1
  # ... (see values.yaml for full options)

recommendedPolicies:
  enabled: false # disabled by default
  defaultPolicyMode: "monitor"
  allowPrivilegeEscalationPolicy:
    # ... (see values.yaml)
```

These resources are owned and reconciled by the controller. Manual
changes are reverted on the next reconciliation. Setting `enabled`
to `false` removes all managed resources.

### CRDs

CRDs are installed with the `helm.sh/resource-policy: keep` annotation:

- `helm upgrade` updates CRDs normally
- `helm uninstall` does not delete CRDs, which prevents cascade-deletion of all PolicyServers and policies in the cluster

## Uninstall

```sh
helm uninstall kubewarden-controller -n kubewarden
```

This removes:

- The controller Deployment
- Managed defaults (resources labeled `kubewarden.io/managed-by=kubewarden-controller-defaults`)
- ConfigMaps, Secrets, Services

It does not remove:

- CRDs (kept by `helm.sh/resource-policy: keep`)
- User-managed PolicyServers and policies

To remove CRDs after uninstall:

```sh
kubectl delete crd policyservers.policies.kubewarden.io
kubectl delete crd clusteradmissionpolicies.policies.kubewarden.io
kubectl delete crd admissionpolicies.policies.kubewarden.io
kubectl delete crd clusteradmissionpolicygroups.policies.kubewarden.io
kubectl delete crd admissionpolicygroups.policies.kubewarden.io
```

## References

- [Kubewarden documentation](https://docs.kubewarden.io/)
- [Releases](https://github.com/kubewarden/adm-controller/releases)
