package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	policiesv1 "github.com/kubewarden/adm-controller/api/policies/v1"
)

// This file contains temporary migration code that takes care of the rename
// of the policies' unique names, which happened to fix GHSA advisory about
// ambiguous policy identities: the legacy `namespaced-<ns>-<name>` encoding
// was ambiguous (`-` is both the delimiter and a legal character inside of
// namespace and policy names), allowing two distinct policies to collide on
// the same webhook configuration, admission path and PolicyServer
// configuration entry.
//
// The Kubewarden upgrade path does not allow jumping versions, hence this
// code can be safely removed after a few releases, together with the
// legacy entries written by `buildPoliciesMap`.

// legacyPolicyUniqueName returns the unique name a policy had before the
// unambiguous `kw.<kind-token>.` encoding was introduced. It returns an empty
// string for unknown policy types.
func legacyPolicyUniqueName(policy policiesv1.Policy) string {
	switch policy.(type) {
	case *policiesv1.AdmissionPolicy:
		return "namespaced-" + policy.GetNamespace() + "-" + policy.GetName()
	case *policiesv1.AdmissionPolicyGroup:
		return "namespaced-group-" + policy.GetNamespace() + "-" + policy.GetName()
	case *policiesv1.ClusterAdmissionPolicy:
		return "clusterwide-" + policy.GetName()
	case *policiesv1.ClusterAdmissionPolicyGroup:
		return "clusterwide-group-" + policy.GetName()
	default:
		return ""
	}
}

// reconcileLegacyWebhookConfigurationCleanup deletes the webhook
// configurations that were created for the given policy using the legacy
// unique name. Both the validating and the mutating configurations are
// checked, since the policy's `mutating` flag could have changed since they
// were created. Only objects carrying the Kubewarden labels are removed.
func (r *policySubReconciler) reconcileLegacyWebhookConfigurationCleanup(ctx context.Context, policy policiesv1.Policy) error {
	legacyName := legacyPolicyUniqueName(policy)
	if legacyName == "" {
		return nil
	}

	validatingWebhook := admissionregistrationv1.ValidatingWebhookConfiguration{}
	err := r.Get(ctx, types.NamespacedName{Name: legacyName}, &validatingWebhook)
	if err == nil && hasKubewardenLabel(validatingWebhook.GetLabels()) {
		deleteErr := r.Delete(ctx, &validatingWebhook, client.Preconditions{UID: &validatingWebhook.UID})
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) && !apierrors.IsConflict(deleteErr) {
			return fmt.Errorf("cannot delete legacy validating webhook: %w", deleteErr)
		}
	} else if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("cannot retrieve legacy validating webhook: %w", err)
	}

	mutatingWebhook := admissionregistrationv1.MutatingWebhookConfiguration{}
	err = r.Get(ctx, types.NamespacedName{Name: legacyName}, &mutatingWebhook)
	if err == nil && hasKubewardenLabel(mutatingWebhook.GetLabels()) {
		deleteErr := r.Delete(ctx, &mutatingWebhook, client.Preconditions{UID: &mutatingWebhook.UID})
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) && !apierrors.IsConflict(deleteErr) {
			return fmt.Errorf("cannot delete legacy mutating webhook: %w", deleteErr)
		}
	} else if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("cannot retrieve legacy mutating webhook: %w", err)
	}

	return nil
}

// addLegacyPolicyEntries adds, for every policy already present in the map
// under its new unique name, the same configuration entry under the legacy
// unique name. This keeps the legacy admission paths served by the
// policy-server during the migration window, i.e. while webhook
// configurations created with the legacy names still exist.
//
// The legacy encoding is ambiguous: distinct policies can map to the same
// legacy name. When that happens, no entry is written for the contended
// legacy name. Serving one of the colliding policies under it would re-open
// the vulnerability this migration is part of fixing: the winning policy
// would be evaluated in place of the others. At worst, the omission turns the
// collision into a short-lived, self-healing dead admission path.
func addLegacyPolicyEntries(policies policyConfigEntryMap, admissionPolicies []policiesv1.Policy, log logr.Logger) {
	legacyNameOwners := make(map[string][]policiesv1.Policy, len(admissionPolicies))
	for _, admissionPolicy := range admissionPolicies {
		legacyName := legacyPolicyUniqueName(admissionPolicy)
		if legacyName == "" {
			continue
		}
		legacyNameOwners[legacyName] = append(legacyNameOwners[legacyName], admissionPolicy)
	}

	for legacyName, owners := range legacyNameOwners {
		if len(owners) > 1 {
			colliding := make([]string, 0, len(owners))
			for _, owner := range owners {
				colliding = append(colliding, owner.GetUniqueName())
			}
			log.Info(
				"skipping legacy configuration entry: multiple policies map to the same legacy unique name",
				"legacyName", legacyName,
				"policies", colliding,
			)
			continue
		}

		if entry, ok := policies[owners[0].GetUniqueName()]; ok {
			policies[legacyName] = entry
		}
	}
}
