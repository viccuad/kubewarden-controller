package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The unique name is used as the name of cluster-scoped webhook
// configurations, as the policy-server admission path and as the key of the
// policy-server configuration entries. Hence the encoding must be injective:
// two distinct policies (of any kind) must never produce the same unique
// name. The legacy `-`-delimited encoding was ambiguous and allowed a tenant
// to hijack the admission traffic of another namespace (security fix).

func TestAdmissionPolicyUniqueNameFormat(t *testing.T) {
	policy := AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-prod", Name: "baseline"},
	}
	assert.Equal(t, "kw.ap.tenant-prod.baseline", policy.GetUniqueName())

	group := AdmissionPolicyGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-prod", Name: "baseline"},
	}
	assert.Equal(t, "kw.apg.tenant-prod.baseline", group.GetUniqueName())

	clusterPolicy := ClusterAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "baseline"},
	}
	assert.Equal(t, "kw.cap.baseline", clusterPolicy.GetUniqueName())

	clusterGroup := ClusterAdmissionPolicyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "baseline"},
	}
	assert.Equal(t, "kw.capg.baseline", clusterGroup.GetUniqueName())
}

// Regression test: `tenant-prod/baseline` and `tenant/prod-baseline` used to
// collide on `namespaced-tenant-prod-baseline`. The namespace cannot contain
// dots, so the `.` delimiter keeps the two identities distinct.
func TestAdmissionPolicyUniqueNameNoCollisionAcrossNamespaces(t *testing.T) {
	victim := AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-prod", Name: "baseline"},
	}
	attacker := AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "prod-baseline"},
	}

	assert.NotEqual(t, victim.GetUniqueName(), attacker.GetUniqueName())
}

// Regression test: an AdmissionPolicy in namespace `group-x` used to collide
// with an AdmissionPolicyGroup in namespace `x` bearing the same name
// (`namespaced-group-x-<name>`). The kind token is dot-free and unique per
// kind, so identities of different kinds can never collide.
func TestUniqueNameNoCollisionAcrossKinds(t *testing.T) {
	policy := AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "group-x", Name: "y"},
	}
	group := AdmissionPolicyGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "x", Name: "y"},
	}
	assert.NotEqual(t, policy.GetUniqueName(), group.GetUniqueName())

	clusterPolicy := ClusterAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "group-y"},
	}
	clusterGroup := ClusterAdmissionPolicyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "y"},
	}
	assert.NotEqual(t, clusterPolicy.GetUniqueName(), clusterGroup.GetUniqueName())
}

// Policy names may contain dots: the namespace and the kind token are the
// only segments used to decode the identity, so dotted policy names cannot
// craft an identity belonging to another namespace.
func TestUniqueNameNoCollisionWithDottedPolicyNames(t *testing.T) {
	dottedName := AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "prod.baseline"},
	}
	victim := AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-prod", Name: "baseline"},
	}
	otherVictim := AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "baseline"},
	}

	assert.NotEqual(t, victim.GetUniqueName(), dottedName.GetUniqueName())
	assert.NotEqual(t, otherVictim.GetUniqueName(), dottedName.GetUniqueName())
}
