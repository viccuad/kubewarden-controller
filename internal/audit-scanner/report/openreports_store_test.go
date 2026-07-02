package report

import (
	"fmt"
	"log/slog"
	"testing"

	policiesv1 "github.com/kubewarden/adm-controller/api/policies/v1"
	auditConstants "github.com/kubewarden/adm-controller/internal/audit-scanner/constants"
	openreports "github.com/openreports/reports-api/apis/openreports.io/v1alpha1"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubewarden/adm-controller/internal/audit-scanner/testutils"
)

func TestCreateReport(t *testing.T) {
	fakeClient, err := testutils.NewFakeClient()
	require.NoError(t, err)
	logger := slog.Default()
	store := NewOpenReportStore(fakeClient, logger)

	resource := unstructured.Unstructured{}
	resource.SetUID("uid")
	resource.SetName("test-pod")
	resource.SetNamespace("namespace")
	resource.SetAPIVersion("v1")
	resource.SetKind("Pod")
	resource.SetResourceVersion("12345")

	policyReport := NewOpenReport("runUID", resource)
	err = store.CreateOrPatchReport(t.Context(), policyReport)
	require.NoError(t, err)

	storedPolicyReport := &openreports.Report{}
	err = fakeClient.Get(t.Context(), types.NamespacedName{Name: policyReport.report.GetName(), Namespace: policyReport.report.GetNamespace()}, storedPolicyReport)
	require.NoError(t, err)

	require.Equal(t, policyReport.report.ObjectMeta.Labels, storedPolicyReport.ObjectMeta.Labels)
	require.Equal(t, policyReport.report.ObjectMeta.OwnerReferences, storedPolicyReport.ObjectMeta.OwnerReferences)
	require.Equal(t, policyReport.report.Scope, storedPolicyReport.Scope)
	require.Equal(t, policyReport.report.Summary, storedPolicyReport.Summary)
	require.Equal(t, policyReport.report.Results, storedPolicyReport.Results)
}

func TestPatchReport(t *testing.T) {
	fakeClient, err := testutils.NewFakeClient()
	require.NoError(t, err)
	logger := slog.Default()
	store := NewOpenReportStore(fakeClient, logger)

	resource := unstructured.Unstructured{}
	resource.SetUID("uid")
	resource.SetName("test-pod")
	resource.SetNamespace("test-namespace")
	resource.SetAPIVersion("v1")
	resource.SetKind("Pod")
	resource.SetResourceVersion("12345")

	policyReport := NewOpenReport("runUID", resource)
	err = store.CreateOrPatchReport(t.Context(), policyReport)
	require.NoError(t, err)

	// The resource version is updated to simulate a change in the resource.
	resource.SetResourceVersion("45678")
	newPolicyReport := NewOpenReport("runUID", resource)
	// Results are added to the policy report
	policy := &policiesv1.AdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{
			UID:             "policy-uid",
			ResourceVersion: "1",
			Name:            "policy-name",
			Namespace:       "test-namespace",
		},
	}
	admissionReview := &admissionv1.AdmissionReview{
		Response: &admissionv1.AdmissionResponse{
			Allowed: true,
			Result:  &metav1.Status{Message: "The request was allowed"},
		},
	}
	newPolicyReport.AddResult(policy, admissionReview, false)
	err = store.CreateOrPatchReport(t.Context(), newPolicyReport)
	require.NoError(t, err)

	storedPolicyReport := &openreports.Report{}
	err = fakeClient.Get(t.Context(), types.NamespacedName{Name: policyReport.report.GetName(), Namespace: policyReport.report.GetNamespace()}, storedPolicyReport)
	require.NoError(t, err)

	require.Equal(t, newPolicyReport.report.ObjectMeta.Labels, storedPolicyReport.ObjectMeta.Labels)
	require.Equal(t, newPolicyReport.report.ObjectMeta.OwnerReferences, storedPolicyReport.ObjectMeta.OwnerReferences)
	require.Equal(t, newPolicyReport.report.Scope, storedPolicyReport.Scope)
	require.Equal(t, newPolicyReport.report.Summary, storedPolicyReport.Summary)
	require.Equal(t, newPolicyReport.report.Results, storedPolicyReport.Results)
}

func TestCreateClusterReport(t *testing.T) {
	fakeClient, err := testutils.NewFakeClient()
	require.NoError(t, err)
	logger := slog.Default()
	store := NewOpenReportStore(fakeClient, logger)

	resource := unstructured.Unstructured{}
	resource.SetUID("uid")
	resource.SetName("test-namespace")
	resource.SetAPIVersion("v1")
	resource.SetKind("Namespace")
	resource.SetResourceVersion("12345")

	clusterPolicyReport := NewClusterOpenReport("runUID", resource)
	err = store.CreateOrPatchClusterReport(t.Context(), clusterPolicyReport)
	require.NoError(t, err)

	storedClusterPolicyReport := &openreports.ClusterReport{}
	err = fakeClient.Get(t.Context(), types.NamespacedName{Name: clusterPolicyReport.report.GetName()}, storedClusterPolicyReport)
	require.NoError(t, err)

	require.Equal(t, clusterPolicyReport.report.ObjectMeta.Labels, storedClusterPolicyReport.ObjectMeta.Labels)
	require.Equal(t, clusterPolicyReport.report.ObjectMeta.OwnerReferences, storedClusterPolicyReport.ObjectMeta.OwnerReferences)
	require.Equal(t, clusterPolicyReport.report.Scope, storedClusterPolicyReport.Scope)
	require.Equal(t, clusterPolicyReport.report.Summary, storedClusterPolicyReport.Summary)
	require.Equal(t, clusterPolicyReport.report.Results, storedClusterPolicyReport.Results)
}

func TestPatchClusterReport(t *testing.T) {
	fakeClient, err := testutils.NewFakeClient()
	require.NoError(t, err)
	logger := slog.Default()
	store := NewOpenReportStore(fakeClient, logger)

	resource := unstructured.Unstructured{}
	resource.SetUID("uid")
	resource.SetAPIVersion("v1")
	resource.SetKind("Namespace")
	resource.SetName("test-namespace")
	resource.SetResourceVersion("12345")

	clusterPolicyReport := NewClusterOpenReport("runUID", resource)
	err = store.CreateOrPatchClusterReport(t.Context(), clusterPolicyReport)
	require.NoError(t, err)

	// The resource version is updated to simulate a change in the resource.
	resource.SetResourceVersion("45678")
	newClusterPolicyReport := NewClusterOpenReport("runUID", resource)
	// Results are added to the policy report
	policy := &policiesv1.ClusterAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{
			UID:             "policy-uid",
			ResourceVersion: "1",
			Name:            "policy-name",
		},
	}
	admissionReview := &admissionv1.AdmissionReview{
		Response: &admissionv1.AdmissionResponse{
			Allowed: true,
			Result:  &metav1.Status{Message: "The request was allowed"},
		},
	}
	newClusterPolicyReport.AddResult(policy, admissionReview, false)
	err = store.CreateOrPatchClusterReport(t.Context(), newClusterPolicyReport)
	require.NoError(t, err)

	storedClusterPolicyReport := &openreports.ClusterReport{}
	err = fakeClient.Get(t.Context(), types.NamespacedName{Name: clusterPolicyReport.report.GetName()}, storedClusterPolicyReport)
	require.NoError(t, err)

	require.Equal(t, newClusterPolicyReport.report.ObjectMeta.Labels, storedClusterPolicyReport.ObjectMeta.Labels)
	require.Equal(t, newClusterPolicyReport.report.ObjectMeta.OwnerReferences, storedClusterPolicyReport.ObjectMeta.OwnerReferences)
	require.Equal(t, newClusterPolicyReport.report.Scope, storedClusterPolicyReport.Scope)
	require.Equal(t, newClusterPolicyReport.report.Summary, storedClusterPolicyReport.Summary)
	require.Equal(t, newClusterPolicyReport.report.Results, storedClusterPolicyReport.Results)
}

func TestDeleteReport(t *testing.T) {
	oldPolicyReport := testutils.NewPolicyReportFactory().
		Name("old-report").Namespace("default").RunUID("old-uid").WithAppLabel().BuildOpenReports()
	otherOldPolicyReport := testutils.NewPolicyReportFactory().
		Name("other-old-report").Namespace("default").RunUID("old-uid").BuildOpenReports()
	newPolicyReport := testutils.NewPolicyReportFactory().
		Name("new-report").Namespace("default").RunUID("new-uid").WithAppLabel().BuildOpenReports()
	oldPolicyReportOtheNamespace := testutils.NewPolicyReportFactory().
		Name("old-report-other-namespace").Namespace("other").RunUID("old-uid").WithAppLabel().BuildOpenReports()

	fakeClient, err := testutils.NewFakeClient(oldPolicyReport, otherOldPolicyReport, newPolicyReport, oldPolicyReportOtheNamespace)
	require.NoError(t, err)
	logger := slog.Default()
	store := NewOpenReportStore(fakeClient, logger)

	err = store.DeleteOldReports(t.Context(), sets.New("new-report"), "default")
	require.NoError(t, err)

	storedPolicyReportList := &openreports.ReportList{}

	err = fakeClient.List(t.Context(), storedPolicyReportList, &client.ListOptions{Namespace: "other"})
	require.NoError(t, err)
	require.Len(t, storedPolicyReportList.Items, 1)

	labelSelector, err := labels.Parse(fmt.Sprintf("%s=%s", auditConstants.AuditScannerRunUIDLabel, "old-uid"))
	require.NoError(t, err)
	err = fakeClient.List(t.Context(), storedPolicyReportList, &client.ListOptions{LabelSelector: labelSelector, Namespace: "default"})
	require.NoError(t, err)
	require.Len(t, storedPolicyReportList.Items, 1)
	require.Equal(t, "other-old-report", storedPolicyReportList.Items[0].Name)

	labelSelector, err = labels.Parse(fmt.Sprintf("%s!=%s", auditConstants.AuditScannerRunUIDLabel, "old-uid"))
	require.NoError(t, err)
	err = fakeClient.List(t.Context(), storedPolicyReportList, &client.ListOptions{LabelSelector: labelSelector, Namespace: "default"})
	require.NoError(t, err)
	require.Len(t, storedPolicyReportList.Items, 1)
}

func TestDeleteClusterReport(t *testing.T) {
	oldPolicyReport := testutils.NewClusterPolicyReportFactory().
		Name("old-report-with-app-label").WithAppLabel().RunUID("old-uid").BuildOpenReports()
	otherOldPolicyReport := testutils.NewClusterPolicyReportFactory().
		Name("old-report-with-no-app-label").RunUID("old-uid").BuildOpenReports()
	newPolicyReport := testutils.NewClusterPolicyReportFactory().
		Name("new-report").WithAppLabel().RunUID("new-uid").BuildOpenReports()
	fakeClient, err := testutils.NewFakeClient(oldPolicyReport, otherOldPolicyReport, newPolicyReport)
	require.NoError(t, err)
	logger := slog.Default()
	store := NewOpenReportStore(fakeClient, logger)

	err = store.DeleteOldClusterReports(t.Context(), sets.New("new-report"))
	require.NoError(t, err)

	storedPolicyReportList := &openreports.ClusterReportList{}

	labelSelector, err := labels.Parse(fmt.Sprintf("%s=%s", auditConstants.AuditScannerRunUIDLabel, "old-uid"))
	require.NoError(t, err)
	err = fakeClient.List(t.Context(), storedPolicyReportList, &client.ListOptions{LabelSelector: labelSelector})
	require.NoError(t, err)
	require.Len(t, storedPolicyReportList.Items, 1)
	require.Equal(t, "old-report-with-no-app-label", storedPolicyReportList.Items[0].Name)

	storedPolicyReportList = &openreports.ClusterReportList{}

	labelSelector, err = labels.Parse(fmt.Sprintf("%s!=%s", auditConstants.AuditScannerRunUIDLabel, "old-uid"))
	require.NoError(t, err)
	err = fakeClient.List(t.Context(), storedPolicyReportList, &client.ListOptions{LabelSelector: labelSelector})
	require.NoError(t, err)
	require.Len(t, storedPolicyReportList.Items, 1)
}

// TestDeleteOldReportsKeepsReportWrittenThisRunWithStaleLabel is a regression
// test for the read-after-write race where a Report that was patched during the
// current run could still be deleted because the garbage collection relied on
// the run-uid label, whose new value may not yet be visible when the delete
// runs. Deletion is now driven by the set of report names written this run, so a
// kept report survives even if its run-uid label still shows a previous run.
func TestDeleteOldReportsKeepsReportWrittenThisRunWithStaleLabel(t *testing.T) {
	// "kept-report" is in the write-set but still carries a stale run-uid label,
	// simulating a patch whose new label is not yet observable by the delete.
	keptWithStaleLabel := testutils.NewPolicyReportFactory().
		Name("kept-report").Namespace("default").RunUID("stale-uid").WithAppLabel().BuildOpenReports()
	// "stale-report" is managed but was NOT written this run, so it must be deleted.
	staleReport := testutils.NewPolicyReportFactory().
		Name("stale-report").Namespace("default").RunUID("stale-uid").WithAppLabel().BuildOpenReports()

	fakeClient, err := testutils.NewFakeClient(keptWithStaleLabel, staleReport)
	require.NoError(t, err)
	store := NewOpenReportStore(fakeClient, slog.Default())

	err = store.DeleteOldReports(t.Context(), sets.New("kept-report"), "default")
	require.NoError(t, err)

	// kept-report must survive despite its stale run-uid label.
	err = fakeClient.Get(t.Context(), types.NamespacedName{Name: "kept-report", Namespace: "default"}, &openreports.Report{})
	require.NoError(t, err)

	// stale-report must be deleted.
	err = fakeClient.Get(t.Context(), types.NamespacedName{Name: "stale-report", Namespace: "default"}, &openreports.Report{})
	require.True(t, apierrors.IsNotFound(err))
}

// TestDeleteOldClusterReportsKeepsReportWrittenThisRunWithStaleLabel is the
// cluster-scoped counterpart of the regression test above.
func TestDeleteOldClusterReportsKeepsReportWrittenThisRunWithStaleLabel(t *testing.T) {
	keptWithStaleLabel := testutils.NewClusterPolicyReportFactory().
		Name("kept-report").RunUID("stale-uid").WithAppLabel().BuildOpenReports()
	staleReport := testutils.NewClusterPolicyReportFactory().
		Name("stale-report").RunUID("stale-uid").WithAppLabel().BuildOpenReports()

	fakeClient, err := testutils.NewFakeClient(keptWithStaleLabel, staleReport)
	require.NoError(t, err)
	store := NewOpenReportStore(fakeClient, slog.Default())

	err = store.DeleteOldClusterReports(t.Context(), sets.New("kept-report"))
	require.NoError(t, err)

	err = fakeClient.Get(t.Context(), types.NamespacedName{Name: "kept-report"}, &openreports.ClusterReport{})
	require.NoError(t, err)

	err = fakeClient.Get(t.Context(), types.NamespacedName{Name: "stale-report"}, &openreports.ClusterReport{})
	require.True(t, apierrors.IsNotFound(err))
}
