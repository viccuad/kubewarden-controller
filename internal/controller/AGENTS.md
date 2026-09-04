# AGENTS.md — the controller

- This file applies to all the files in `internal/controller/`. The root
  [`AGENTS.md`](../../AGENTS.md) holds the rules for the full monorepo.

## Structure

- This package holds the reconcilers for the CRDs of the group
  `policies.kubewarden.io`. They are built on controller-runtime.
- `admissionpolicy_controller.go`, `clusteradmissionpolicy_controller.go` and
  the two `*group` files reconcile the policies into webhook configurations.
- `policyserver_controller.go` and the `policy_server_*.go` files handle the
  deployment, the service, the configmap, the PDB and the certificate secret
  of a `PolicyServer`.
- `cert_controller.go` rotates the CA and the serving certificates.
- `policy_subreconciler.go` and `policy_subreconciler_webhook.go` hold the
  logic that the four policy kinds share.
- `legacy_policy_migration.go` migrates away from the deprecated package
  `api/policies/v1alpha2`.
- `suite_test.go` starts envtest for the full package.

## Testing strategy

- The tests of this package run against a real API server with envtest.
- They are the integration level of the project.
- Use them for the behavior of a reconciler.
- For logic that needs no API server, write a unit test instead.

- Run `make test-go` from the root of the repository.
- The Makefile downloads the envtest binaries into `.envtest/` and sets
  `KUBEBUILDER_ASSETS`.

These facts apply to the suite:

- The suite uses Ginkgo v2, Gomega and `SynchronizedBeforeSuite`. All the specs
  are in one package.
- The suite loads the CRDs from the Helm chart. The function
  `kubewardenCRDPaths()` in `suite_test.go` lists the five files
  `policies.kubewarden.io_*.yaml` in
  `charts/admission-controller/templates/crds/`. It excludes the CRDs of the
  policy reports, because their Helm conditionals are not plain YAML.
- If you add a CRD kind, add its path in the chart to `kubewardenCRDPaths()`.
- The constants are the namespace `kubewarden-integration-tests`,
  `timeout = 180s`, `pollInterval = 250ms` and `consistencyTimeout = 5s`. Use
  `Eventually` with these values. Do not invent new ones. Use `Consistently` to
  assert that nothing happens.
- To work on one spec, use the focus helpers of Ginkgo: `FIt`, `FDescribe` and
  `FContext`. Never commit them. A focused suite gives a false pass.

## Development principles

- A reconciler must be idempotent. The same input gives the same result, and a
  second call changes nothing.
- A reconciler must converge from any state. Read the state of the cluster.
  Do not assume the result of the previous call.
- Keep the RBAC markers as narrow as the reconciler needs.
- Logic that the four policy kinds share belongs in `policy_subreconciler.go`.

## Pitfalls

- The `+kubebuilder:rbac:` markers in this package generate
  `charts/admission-controller/templates/controller/controller-rbac-roles.yaml`.
  After you add a permission or make one wider, run `make generate` and
  `make check-generate`.
- After a change to a CRD, run `make manifests` before the tests. If you do not,
  the suite runs against an old schema.
- Keep the markers `//+kubebuilder:scaffold:imports` and
  `//+kubebuilder:scaffold:scheme`. The Kubebuilder scaffolding inserts code at
  these positions.
- The file `.golangci.yml` is strict for this package. The test files have
  exclusions. The production code has none.
