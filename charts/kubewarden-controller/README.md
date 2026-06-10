# Kubewarden Admission Controller

Unified Helm chart for deploying the complete Kubewarden admission control stack.

> **Note:** This chart combines what were previously three separate charts:
> `kubewarden-crds` (CRDs), `kubewarden-controller` (controller), and
> `kubewarden-defaults` (default PolicyServer and recommended policies).

## Installation

```sh
helm install kubewarden kubewarden/kubewarden-controller -n kubewarden --create-namespace
```

## Migration from Three-Chart Setup

If you're currently running the legacy three-chart setup (`kubewarden-crds`,
`kubewarden-controller`, `kubewarden-defaults`) released until Kubewarden
admission controller version 1.36, follow these steps to migrate to the unified
chart released on version 1.37.

**⚠️ Important**: There will be a brief window during migration where no admission control is active. Plan accordingly.

### Prerequisites

- Access to your cluster with `kubectl` and `helm`
- [`yq`](https://github.com/mikefarah/yq) installed locally (used to sanitize the backup output)

### Migration Steps

#### 1. Backup All Policies and PolicyServers

Uninstalling `kubewarden-crds` cascade-deletes **all** custom resources, so every policy and PolicyServer must be backed up. The `yq` filter strips cluster-specific metadata (`uid`, `resourceVersion`, `managedFields`, `status`, …) so that the manifests can be re-applied cleanly later:

```sh
FILTER='del(.items[].metadata.uid, .items[].metadata.resourceVersion, .items[].metadata.creationTimestamp, .items[].metadata.generation, .items[].metadata.managedFields, .items[].status)'

kubectl get clusteradmissionpolicies -A -o yaml | yq "$FILTER" > clusteradmissionpolicies-backup.yaml
kubectl get admissionpolicies -A -o yaml | yq "$FILTER" > admissionpolicies-backup.yaml
kubectl get clusteradmissionpolicygroups -A -o yaml | yq "$FILTER" > clusteradmissionpolicygroups-backup.yaml
kubectl get admissionpolicygroups -A -o yaml | yq "$FILTER" > admissionpolicygroups-backup.yaml
kubectl get policyservers -A -o yaml | yq "$FILTER" > policyservers-backup.yaml
```

#### 2. Uninstall Old Charts

Uninstall in reverse order:

```sh
helm uninstall kubewarden-defaults -n kubewarden
helm uninstall kubewarden-controller -n kubewarden
helm uninstall kubewarden-crds -n kubewarden
```

This removes all CRDs and cascades deletion of all CRs (PolicyServers and policies).

#### 3. Install the Unified Chart

```sh
helm install kubewarden kubewarden/kubewarden-controller -n kubewarden
```

This defines:

- CRDs. These are created with the `helm.sh/resource-policy: keep` to prevent deletion on uninstall.
- The actual adm-controller
- If enabled by `values.yaml`, the default Policy Server and the recommended policies

#### 4. Restore User Policies

Once the default PolicyServer is ready, re-apply all backed-up resources:

```sh
kubectl apply -f policyservers-backup.yaml
kubectl apply -f clusteradmissionpolicies-backup.yaml
kubectl apply -f admissionpolicies-backup.yaml
kubectl apply -f clusteradmissionpolicygroups-backup.yaml
kubectl apply -f admissionpolicygroups-backup.yaml
```

## Configuration

### Defaults

The chart can deploy a default Policy Server and a series of recommended policies:

```yaml
policyServer:
  enabled: true
  replicaCount: 1
  # ... (see values.yaml for full options)

recommendedPolicies:
  enabled: false # Disabled by default
  defaultPolicyMode: "monitor"
  allowPrivilegeEscalationPolicy:
    # ... (see values.yaml)
```

**Note:** these resources are owned and reconciled by the adm-controller.
Manual changes are going to be reverted. Also, changing this value to `false` leads to a cleanup of all these managed resources.

### CRDs

CRDs are installed with the `helm.sh/resource-policy: keep` annotation. This means:

- `helm upgrade` will update CRDs
- `helm uninstall` will **not** delete CRDs (preventing catastrophic cascade-deletion of all cluster resources)

## Uninstall

```sh
helm uninstall kubewarden -n kubewarden
```

This removes:

- The controller deployment
- Managed defaults (resources with `kubewarden.io/managed-by=kubewarden-controller-defaults` label)
- ConfigMaps, Secrets, Services

It does **not** remove:

- CRDs (due to `helm.sh/resource-policy: keep`)
- User-managed PolicyServers and policies

To fully remove CRDs after uninstall:

```sh
kubectl delete crd policyservers.policies.kubewarden.io
kubectl delete crd clusteradmissionpolicies.policies.kubewarden.io
kubectl delete crd admissionpolicies.policies.kubewarden.io
kubectl delete crd clusteradmissionpolicygroups.policies.kubewarden.io
kubectl delete crd admissionpolicygroups.policies.kubewarden.io
```

## References

- [Kubewarden Documentation](https://docs.kubewarden.io/)
- [Releases](https://github.com/kubewarden/adm-controller/releases)
