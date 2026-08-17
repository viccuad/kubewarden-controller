package controller

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	policiesv1 "github.com/kubewarden/adm-controller/api/policies/v1"
	"github.com/kubewarden/adm-controller/internal/constants"
)

func TestLegacyPolicyUniqueName(t *testing.T) {
	tests := []struct {
		name     string
		policy   policiesv1.Policy
		expected string
	}{
		{
			name: "AdmissionPolicy",
			policy: &policiesv1.AdmissionPolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-prod", Name: "baseline"},
			},
			expected: "namespaced-tenant-prod-baseline",
		},
		{
			name: "AdmissionPolicyGroup",
			policy: &policiesv1.AdmissionPolicyGroup{
				ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-prod", Name: "baseline"},
			},
			expected: "namespaced-group-tenant-prod-baseline",
		},
		{
			name: "ClusterAdmissionPolicy",
			policy: &policiesv1.ClusterAdmissionPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "baseline"},
			},
			expected: "clusterwide-baseline",
		},
		{
			name: "ClusterAdmissionPolicyGroup",
			policy: &policiesv1.ClusterAdmissionPolicyGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "baseline"},
			},
			expected: "clusterwide-group-baseline",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, legacyPolicyUniqueName(test.policy))
		})
	}
}

func TestAddLegacyPolicyEntries(t *testing.T) {
	policyServer := &policiesv1.PolicyServer{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	policy := &policiesv1.AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-prod", Name: "baseline"},
		Spec: policiesv1.AdmissionPolicySpec{
			PolicySpec: policiesv1.PolicySpec{Module: "registry://legit/module:v1"},
		},
	}

	policies := buildPoliciesMap([]policiesv1.Policy{policy}, policyServer)
	addLegacyPolicyEntries(policies, []policiesv1.Policy{policy}, logr.Discard())

	require.Contains(t, policies, "kw.ap.tenant-prod.baseline")
	require.Contains(t, policies, "namespaced-tenant-prod-baseline")
	assert.Equal(t, policies["kw.ap.tenant-prod.baseline"], policies["namespaced-tenant-prod-baseline"])
}

// When distinct policies collide on the same legacy unique name, no legacy
// entry must be written: serving one of them under the contended identity
// would allow its module to evaluate admission requests meant for the other
// (the vulnerability this migration code is part of fixing).
func TestAddLegacyPolicyEntriesSkipsCollidingLegacyNames(t *testing.T) {
	policyServer := &policiesv1.PolicyServer{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	victim := &policiesv1.AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-prod", Name: "baseline"},
		Spec: policiesv1.AdmissionPolicySpec{
			PolicySpec: policiesv1.PolicySpec{Module: "registry://legit/module:v1"},
		},
	}
	attacker := &policiesv1.AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "prod-baseline"},
		Spec: policiesv1.AdmissionPolicySpec{
			PolicySpec: policiesv1.PolicySpec{Module: "registry://attacker/module:v1"},
		},
	}
	allPolicies := []policiesv1.Policy{victim, attacker}

	policies := buildPoliciesMap(allPolicies, policyServer)
	addLegacyPolicyEntries(policies, allPolicies, logr.Discard())

	// Both policies are served under their new, unambiguous names.
	require.Contains(t, policies, "kw.ap.tenant-prod.baseline")
	require.Contains(t, policies, "kw.ap.tenant.prod-baseline")
	// The contended legacy name is not served at all.
	assert.NotContains(t, policies, "namespaced-tenant-prod-baseline")
}

// Cross-kind collisions on the legacy encoding must be skipped as well.
func TestAddLegacyPolicyEntriesSkipsCrossKindCollisions(t *testing.T) {
	policyServer := &policiesv1.PolicyServer{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	policy := &policiesv1.AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "group-x", Name: "y"},
		Spec: policiesv1.AdmissionPolicySpec{
			PolicySpec: policiesv1.PolicySpec{Module: "registry://legit/module:v1"},
		},
	}
	group := policiesv1.NewAdmissionPolicyGroupFactory().
		WithNamespace("x").
		WithName("y").
		Build()
	allPolicies := []policiesv1.Policy{policy, group}

	policies := buildPoliciesMap(allPolicies, policyServer)
	addLegacyPolicyEntries(policies, allPolicies, logr.Discard())

	require.Contains(t, policies, "kw.ap.group-x.y")
	require.Contains(t, policies, "kw.apg.x.y")
	assert.NotContains(t, policies, "namespaced-group-x-y")
}

func TestReconcileLegacyWebhookConfigurationCleanup(t *testing.T) {
	policy := &policiesv1.AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-prod", Name: "baseline"},
	}
	legacyName := legacyPolicyUniqueName(policy)

	tests := []struct {
		name           string
		labels         map[string]string
		expectDeletion bool
	}{
		{
			name:           "deletes legacy webhook configurations with the part-of label",
			labels:         map[string]string{constants.PartOfLabelKey: constants.PartOfLabelValue},
			expectDeletion: true,
		},
		{
			name:           "deletes legacy webhook configurations with the pre v1.16.0 label",
			labels:         map[string]string{"kubewarden": "true"},
			expectDeletion: true,
		},
		{
			name:           "does not delete webhook configurations not managed by Kubewarden",
			labels:         map[string]string{},
			expectDeletion: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			validatingWebhook := &admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: legacyName, Labels: test.labels},
			}
			mutatingWebhook := &admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: legacyName, Labels: test.labels},
			}
			k8sClient := fake.NewClientBuilder().WithObjects(validatingWebhook, mutatingWebhook).Build()
			reconciler := &policySubReconciler{Client: k8sClient, Log: logr.Discard()}

			err := reconciler.reconcileLegacyWebhookConfigurationCleanup(t.Context(), policy)
			require.NoError(t, err)

			validatingErr := k8sClient.Get(t.Context(), types.NamespacedName{Name: legacyName}, &admissionregistrationv1.ValidatingWebhookConfiguration{})
			mutatingErr := k8sClient.Get(t.Context(), types.NamespacedName{Name: legacyName}, &admissionregistrationv1.MutatingWebhookConfiguration{})
			if test.expectDeletion {
				assert.True(t, apierrors.IsNotFound(validatingErr))
				assert.True(t, apierrors.IsNotFound(mutatingErr))
			} else {
				assert.NoError(t, validatingErr)
				assert.NoError(t, mutatingErr)
			}
		})
	}
}

func TestReconcileLegacyWebhookConfigurationCleanupNoLegacyWebhooks(t *testing.T) {
	policy := &policiesv1.AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-prod", Name: "baseline"},
	}
	k8sClient := fake.NewClientBuilder().Build()
	reconciler := &policySubReconciler{Client: k8sClient, Log: logr.Discard()}

	err := reconciler.reconcileLegacyWebhookConfigurationCleanup(t.Context(), policy)
	require.NoError(t, err)
}
