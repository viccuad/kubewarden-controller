package e2e

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	policiesv1 "github.com/kubewarden/adm-controller/api/policies/v1"
	"github.com/kubewarden/adm-controller/internal/constants"
)

const (
	victimPolicyKey   contextKey = "victimPolicy"
	attackerPolicyKey contextKey = "attackerPolicy"
)

// legacyUniqueName returns the ambiguous unique name used by previous
// versions of the controller. It is used to verify the migration away from
// it: the cleanup of leftover webhook configurations and the transitional
// dual-key policy-server configuration.
func legacyUniqueName(policy *policiesv1.AdmissionPolicy) string {
	return "namespaced-" + policy.GetNamespace() + "-" + policy.GetName()
}

// getPolicyServerPoliciesEntry returns the parsed `policies.yml` entry of the
// given PolicyServer's ConfigMap.
func getPolicyServerPoliciesEntry(ctx context.Context, cfg *envconf.Config, policyServerName string) (map[string]json.RawMessage, error) {
	configMap := &corev1.ConfigMap{}
	err := cfg.Client().Resources(namespace).Get(ctx, "policy-server-"+policyServerName, namespace, configMap)
	if err != nil {
		return nil, err
	}

	policies := map[string]json.RawMessage{}
	err = json.Unmarshal([]byte(configMap.Data[constants.PolicyServerConfigPoliciesEntry]), &policies)
	if err != nil {
		return nil, err
	}
	return policies, nil
}

// TestPolicyIdentity covers the security fix for the ambiguous policy unique
// names (`namespaced-<namespace>-<name>`): distinct policies used to collide
// on the same webhook configuration, admission path and policy-server
// configuration entry, allowing a tenant to have its module evaluate the
// admission traffic of another namespace.
func TestPolicyIdentity(t *testing.T) {
	victimNamespace := "tenant-prod"
	attackerNamespace := "tenant"

	// Regression test for the identity collision: `tenant-prod/baseline` and
	// `tenant/prod-baseline` used to collide on the same identity,
	// `namespaced-tenant-prod-baseline`.
	collisionFeature := features.New("Colliding policy identities").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			// Create namespaces
			err := createNamespaceWithRetry(ctx, cfg, victimNamespace)
			require.NoError(t, err)
			err = createNamespaceWithRetry(ctx, cfg, attackerNamespace)
			require.NoError(t, err)

			// Add scheme
			err = policiesv1.AddToScheme(cfg.Client().Resources().GetScheme())
			require.NoError(t, err)

			// Create PolicyServer and wait for it to be ready
			policyServerName := policiesv1.NewPolicyServerFactory().Build().Name
			policyServer := policiesv1.NewPolicyServerFactory().
				WithName(policyServerName).
				Build()
			err = createPolicyServerAndWaitForItsService(ctx, cfg, policyServer)
			require.NoError(t, err)

			ctx = context.WithValue(ctx, policyServerNameKey, policyServerName)

			// Create two AdmissionPolicies bound to the same PolicyServer,
			// whose legacy unique names collide.
			victimPolicy := policiesv1.NewAdmissionPolicyFactory().
				WithName("baseline").
				WithNamespace(victimNamespace).
				WithPolicyServer(policyServerName).
				Build()
			err = cfg.Client().Resources().Create(ctx, victimPolicy)
			require.NoError(t, err)

			attackerPolicy := policiesv1.NewAdmissionPolicyFactory().
				WithName("prod-baseline").
				WithNamespace(attackerNamespace).
				WithPolicyServer(policyServerName).
				Build()
			err = cfg.Client().Resources().Create(ctx, attackerPolicy)
			require.NoError(t, err)

			require.Equal(t, legacyUniqueName(victimPolicy), legacyUniqueName(attackerPolicy),
				"test setup error: the two policies are expected to collide on their legacy unique names")

			ctx = context.WithValue(ctx, victimPolicyKey, victimPolicy)
			ctx = context.WithValue(ctx, attackerPolicyKey, attackerPolicy)

			return ctx
		}).
		Assess("both policies should become active", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			victimPolicy := ctx.Value(victimPolicyKey).(*policiesv1.AdmissionPolicy)
			attackerPolicy := ctx.Value(attackerPolicyKey).(*policiesv1.AdmissionPolicy)

			err := waitForAdmissionPolicyActive(cfg, victimPolicy.GetName(), victimPolicy.GetNamespace())
			require.NoError(t, err, "the victim policy should transition to active status")

			err = waitForAdmissionPolicyActive(cfg, attackerPolicy.GetName(), attackerPolicy.GetNamespace())
			require.NoError(t, err, "the attacker policy should transition to active status")

			return ctx
		}).
		Assess("each policy should get its own ValidatingWebhookConfiguration and admission path", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			victimPolicy := ctx.Value(victimPolicyKey).(*policiesv1.AdmissionPolicy)
			attackerPolicy := ctx.Value(attackerPolicyKey).(*policiesv1.AdmissionPolicy)

			require.NotEqual(t, victimPolicy.GetUniqueName(), attackerPolicy.GetUniqueName(),
				"the two policies must have distinct unique names")

			for _, policy := range []*policiesv1.AdmissionPolicy{victimPolicy, attackerPolicy} {
				webhookName := policy.GetUniqueName()
				expectedPath := "/validate/" + webhookName

				webhook := &admissionregistrationv1.ValidatingWebhookConfiguration{
					ObjectMeta: metav1.ObjectMeta{Name: webhookName},
				}
				err := wait.For(conditions.New(cfg.Client().Resources()).ResourceMatch(webhook, func(object k8s.Object) bool {
					w := object.(*admissionregistrationv1.ValidatingWebhookConfiguration)

					if !verifyWebhookMetadata(w.Labels, w.Annotations, policy.GetName(), policy.GetNamespace()) {
						return false
					}
					if len(w.Webhooks) != 1 {
						return false
					}
					if w.Webhooks[0].ClientConfig.Service.Path == nil {
						return false
					}

					return *w.Webhooks[0].ClientConfig.Service.Path == expectedPath
				}), wait.WithTimeout(testTimeout), wait.WithInterval(testPollInterval))
				require.NoErrorf(t, err,
					"policy %s/%s should get its own ValidatingWebhookConfiguration %q routing to %q",
					policy.GetNamespace(), policy.GetName(), webhookName, expectedPath)
			}

			return ctx
		}).
		Assess("the PolicyServer configuration should serve both policies but not the contended legacy identity", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			policyServerName := ctx.Value(policyServerNameKey).(string)
			victimPolicy := ctx.Value(victimPolicyKey).(*policiesv1.AdmissionPolicy)
			attackerPolicy := ctx.Value(attackerPolicyKey).(*policiesv1.AdmissionPolicy)

			policies, err := getPolicyServerPoliciesEntry(ctx, cfg, policyServerName)
			require.NoError(t, err)

			require.Contains(t, policies, victimPolicy.GetUniqueName())
			require.Contains(t, policies, attackerPolicy.GetUniqueName())
			// The legacy identity is contended between the two policies:
			// serving one of them under it would allow its module to evaluate
			// admission requests meant for the other.
			require.NotContains(t, policies, legacyUniqueName(victimPolicy),
				"the contended legacy identity should not be served")

			return ctx
		}).Feature()

	// Migration test: webhook configurations created by previous versions of
	// the controller with the legacy unique names must be cleaned up, and the
	// policy-server configuration must serve the legacy admission paths until
	// then (transitional dual keys).
	migrationNamespace := "legacy-migration-test"
	migrationFeature := features.New("Legacy unique name migration").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			// Create namespace
			err := createNamespaceWithRetry(ctx, cfg, migrationNamespace)
			require.NoError(t, err)

			// Add scheme
			err = policiesv1.AddToScheme(cfg.Client().Resources().GetScheme())
			require.NoError(t, err)

			// Create PolicyServer and wait for it to be ready
			policyServerName := policiesv1.NewPolicyServerFactory().Build().Name
			policyServer := policiesv1.NewPolicyServerFactory().
				WithName(policyServerName).
				Build()
			err = createPolicyServerAndWaitForItsService(ctx, cfg, policyServer)
			require.NoError(t, err)

			ctx = context.WithValue(ctx, policyServerNameKey, policyServerName)

			// Create AdmissionPolicy
			policyName := policiesv1.NewAdmissionPolicyFactory().Build().Name
			policy := policiesv1.NewAdmissionPolicyFactory().
				WithName(policyName).
				WithNamespace(migrationNamespace).
				WithPolicyServer(policyServerName).
				Build()
			err = cfg.Client().Resources().Create(ctx, policy)
			require.NoError(t, err)

			ctx = context.WithValue(ctx, policyKey, policy)

			return ctx
		}).
		Assess("the PolicyServer configuration should serve the policy under both the new and the legacy identity", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			policyServerName := ctx.Value(policyServerNameKey).(string)
			policy := ctx.Value(policyKey).(*policiesv1.AdmissionPolicy)

			err := waitForAdmissionPolicyActive(cfg, policy.GetName(), policy.GetNamespace())
			require.NoError(t, err, "policy should transition to active status")

			policies, err := getPolicyServerPoliciesEntry(ctx, cfg, policyServerName)
			require.NoError(t, err)

			require.Contains(t, policies, policy.GetUniqueName())
			require.Contains(t, policies, legacyUniqueName(policy),
				"the legacy admission path should be served during the migration window")
			require.JSONEq(t,
				string(policies[policy.GetUniqueName()]),
				string(policies[legacyUniqueName(policy)]),
				"the legacy identity should serve the very same policy configuration")

			return ctx
		}).
		Assess("a webhook configuration left behind by a previous controller version should be deleted", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			policy := ctx.Value(policyKey).(*policiesv1.AdmissionPolicy)
			legacyWebhookName := legacyUniqueName(policy)

			// Simulate the leftover webhook configuration created by a
			// previous version of the controller. Its annotations point back
			// to the policy: the controller's webhook configuration watch
			// will enqueue the policy reconciliation, no manual trigger is
			// needed.
			legacyWebhook := &admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: legacyWebhookName,
					Labels: map[string]string{
						constants.PartOfLabelKey: constants.PartOfLabelValue,
					},
					Annotations: map[string]string{
						constants.WebhookConfigurationPolicyNameAnnotationKey:      policy.GetName(),
						constants.WebhookConfigurationPolicyNamespaceAnnotationKey: policy.GetNamespace(),
					},
				},
			}
			err := cfg.Client().Resources().Create(ctx, legacyWebhook)
			require.NoError(t, err)

			// Wait for the controller to clean it up
			err = wait.For(conditions.New(cfg.Client().Resources()).ResourceDeleted(
				&admissionregistrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: legacyWebhookName}},
			), wait.WithTimeout(testTimeout), wait.WithInterval(testPollInterval))
			require.NoError(t, err, "the legacy ValidatingWebhookConfiguration should be deleted by the controller")

			// The webhook configuration with the new name must still be there
			webhook := &admissionregistrationv1.ValidatingWebhookConfiguration{}
			err = cfg.Client().Resources().Get(ctx, policy.GetUniqueName(), "", webhook)
			require.NoError(t, err, "the webhook configuration with the new name should still exist")

			return ctx
		}).Feature()

	testenv.Test(t, collisionFeature, migrationFeature)
}
