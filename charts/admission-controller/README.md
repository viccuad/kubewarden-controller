# Kubewarden Admission Controller

Helm chart for deploying the Kubewarden admission control stack.

> **Note:** This chart replaces the three separate charts that were used
> previously: `kubewarden-crds`, `kubewarden-controller`, and
> `kubewarden-defaults`.

## Installation

```sh
helm install kubewarden kubewarden/admission-controller -n kubewarden --create-namespace
```

## Migration from three-chart setup

If you are running the legacy three-chart setup (`kubewarden-crds`,
`kubewarden-controller`, `kubewarden-defaults`), there are two ways to migrate to
the new single helm chart. Users can use the migration script
(`kubewarden-unified-adm-controller-chart-migration.sh`) to move to this chart
without admission downtime. Or uninstall the old Helm charts and reinstall the
new single Helm chart acknowledging that this procedure will cause down time in the
policy evaluations.

### Manual reinstallation

This is the simplest migration path. But it will cause the policies to stop
being evaluated during the process. In the procedure users must backup all the
policies and policy servers, uninstall the previous Kubewarden stack and
reinstall the new single Helm chart. Once the stack is installed again, the
backup resources can be reapplied.

The reinstallation can follow the below steps:

1. Back your policies and policy servers:

```sh
FILTER='del(.items[].metadata.uid, .items[].metadata.resourceVersion, .items[].metadata.creationTimestamp, .items[].metadata.generation, .items[].metadata.managedFields, .items[].status)'

kubectl get clusteradmissionpolicies -A -o yaml | yq "$FILTER" > clusteradmissionpolicies-backup.yaml
kubectl get admissionpolicies -A -o yaml | yq "$FILTER" > admissionpolicies-backup.yaml
kubectl get clusteradmissionpolicygroups -A -o yaml | yq "$FILTER" > clusteradmissionpolicygroups-backup.yaml
kubectl get admissionpolicygroups -A -o yaml | yq "$FILTER" > admissionpolicygroups-backup.yaml
kubectl get policyservers -A -o yaml | yq "$FILTER" > policyservers-backup.yaml
```

2. Uninstall old Helm charts:

```sh
helm uninstall kubewarden-defaults -n kubewarden
helm uninstall kubewarden-controller -n kubewarden
helm uninstall kubewarden-crds -n kubewarden
```

3. Install the unified Helm chart:

```sh
helm install kubewarden kubewarden/admission-controller -n kubewarden
```

It's important to note that the new single Helm chart also unified the values
files. Thus, your previous values files should work still. Therefore, to have
the same stack again you can merge all the values files used in the old 3 helm
charts into one and use in the new Helm chart installation as well:

```sh
helm install kubewarden kubewarden/admission-controller -n kubewarden --values all-values.yaml
```

4. Restore policies and policy servers:

```sh
kubectl apply -f policyservers-backup.yaml
kubectl apply -f clusteradmissionpolicies-backup.yaml
kubectl apply -f admissionpolicies-backup.yaml
kubectl apply -f clusteradmissionpolicygroups-backup.yaml
kubectl apply -f admissionpolicygroups-backup.yaml
```

After the reconciliation loop run, the Kubewarden stack should be up and
running as before.

### Migration script

The migration requires several Helm operations in a specific order:
adding resource-preservation annotations to the stored release
manifests, uninstalling the legacy releases without running cleanup
hooks, then installing the unified chart so it adopts the existing
resources. The ordering matters because annotations must be in the
stored manifest (not just on live objects) for Helm to honor them,
hooks must be skipped or they delete PolicyServers, and both the
release name and the chart's `nameOverride` must match the legacy ones
(see the "Chart rename / resource naming" caveat) or Kubernetes
rejects the update due to immutable selectors. Getting any step wrong
can delete resources or break admission, so the script handles it all
and verifies each step.

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

To facilitate the migration process the Kubewarden team provided a script that
perform all the required operation to allow a migration to the unified Helm
chart with no downtime. To be able to use the script users must be aware of the
prerequisites listed below.

If for some reason some of the prerequisites is not possible user should backup
all the resources they currently have which is the policy servers and policies
and reinstall them after the reinstallation of the stack. This means that the
migration will cause down time in the policy evaluation.

> [!WARNING]
> The Kubewarden team test the migration path as much as possible. But it is
> still recommended to backup policies and policy server definitions just in
> case something unexpected happens. Therefore, it will be easier to restore to
> the previous state if necessary.

### Prerequisites

- Helm v4+ (needed for Server-Side Apply and post-renderer plugins)
- kubectl with access to your cluster
- yq v4 (github.com/mikefarah/yq) for the post-renderer
- jq for detecting installed chart versions
- The three legacy releases must be installed in your cluster

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
  --unified-chart kubewarden/admission-controller
```

Using a local tarball:

```sh
./kubewarden-unified-adm-controller-chart-migration.sh \
  --unified-chart ./admission-controller-6.0.0.tgz
```

Dry run (no changes applied):

```sh
./kubewarden-unified-adm-controller-chart-migration.sh \
  --unified-chart kubewarden/admission-controller --dry-run
```

Interactive mode (pauses before each destructive step):

```sh
./kubewarden-unified-adm-controller-chart-migration.sh \
  --unified-chart kubewarden/admission-controller --interactive
```

Passing custom values to the unified chart:

```sh
./kubewarden-unified-adm-controller-chart-migration.sh \
  --unified-chart kubewarden/admission-controller \
  --set "image.tag=v2.0.0" \
  --values ./my-custom-values.yaml
```

#### Available flags

| Flag                           | Description                                                      |
| ------------------------------ | ---------------------------------------------------------------- |
| `--unified-chart PATH_OR_NAME` | Required. Local tarball or Helm repo chart name                  |
| `--namespace NS`               | Namespace of the Kubewarden installation (default: `kubewarden`) |
| `--kube-context CTX`           | Kubernetes context to use (default: current context)             |
| `--repo-name NAME`             | Helm repo name (default: `kubewarden`)                           |
| `--repo-url URL`               | Helm repo URL (default: `https://charts.kubewarden.io`)          |
| `--timeout DURATION`           | Timeout for Helm operations (default: `5m`)                      |
| `--set KEY=VALUE`              | Set a value for the unified chart install (repeatable)           |
| `--values FILE` / `-f FILE`    | Values file for the unified chart install (repeatable)           |
| `--interactive`                | Pause for confirmation before destructive steps                  |
| `--dry-run`                    | Show what would be done without making changes                   |
| `--help`                       | Print usage information                                          |

### Caveats

**Release name.** The unified chart must be installed with the same
release name as the legacy `kubewarden-controller` release (usually
`kubewarden-controller`). Kubernetes Deployments have an immutable
`spec.selector.matchLabels` that includes
`app.kubernetes.io/instance: <release-name>`. If the name does not
match, the install fails with an immutable-field error. The script
detects the legacy release name automatically.

**Chart rename / resource naming.** This chart is named
`admission-controller`, so a fresh install names its resources after it
(`app.kubernetes.io/name: admission-controller`, Deployment
`<release>-admission-controller`). The legacy resources being adopted were
created by the old `kubewarden-controller` chart and carry
`app.kubernetes.io/name: kubewarden-controller` in the same immutable
selector. The script therefore installs with
`--set nameOverride=<legacy name>` (read from the legacy release's Helm
values, defaulting to `kubewarden-controller`) so the chart reproduces
the old names/labels and Server-Side Apply adopts the objects in place.
This value is stored in the release, so **future upgrades must keep
it** — run `helm upgrade ... --reuse-values`, or add
`nameOverride: kubewarden-controller` to your values file. Omitting it
re-renders `admission-controller`-named selectors and fails on the immutable
field. (A cluster migrated this way keeps `kubewarden-controller`
resource names; a brand-new install uses `admission-controller`.)

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

#### Reinstalling under a different release name or namespace

Because the CRDs are kept on uninstall, they survive with the Helm
ownership metadata of the release that created them
(`meta.helm.sh/release-name` and `meta.helm.sh/release-namespace`). Helm
checks this metadata on the next install:

- Same release name **and** namespace: Helm adopts the existing CRDs and
  the install succeeds.
- Different release name **or** namespace: Helm refuses to take over the
  CRDs and the install fails with:

  ```
  Error: ... invalid ownership metadata; annotation
  meta.helm.sh/release-name must equal "<new>": current value is "<old>"
  ```

This is expected: the CRDs still belong to the previous release. To adopt
them into the new release, install with `--take-ownership` (Helm 3.18+ or
Helm 4), which re-stamps the ownership metadata:

```sh
helm install <release> <chart> -n <namespace> --take-ownership
```

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
