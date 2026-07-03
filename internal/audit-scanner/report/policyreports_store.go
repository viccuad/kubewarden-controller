package report

import (
	"context"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	wgpolicy "sigs.k8s.io/wg-policy-prototypes/policy-report/pkg/api/wgpolicyk8s.io/v1alpha2"
)

// PolicyReportStore is a store for PolicyReport and ClusterPolicyReport.
//
// Deprecated: use OpenReportStore instead. wgpolicy.PolicyReport is deprecated in favor of openreports.Report.
type PolicyReportStore struct {
	// client is a controller-runtime client that knows about PolicyReport and ClusterPolicyReport CRDs
	client client.Client
	// logger is used to log the messages
	logger *slog.Logger
}

// NewPolicyReportStore creates a new PolicyReportStore.
func NewPolicyReportStore(client client.Client, logger *slog.Logger) *PolicyReportStore {
	return &PolicyReportStore{
		client: client,
		logger: logger.With("component", "policyreportstore"),
	}
}

// CreateOrPatchReport creates or patches a PolicyReport.
func (s *PolicyReportStore) CreateOrPatchReport(ctx context.Context, obj any) error {
	report, ok := obj.(*PolicyReport)
	if !ok {
		return fmt.Errorf("expected *PolicyReport, got %T", obj)
	}
	policyReport := report.report
	oldPolicyReport := &wgpolicy.PolicyReport{ObjectMeta: metav1.ObjectMeta{
		Name:      policyReport.GetName(),
		Namespace: policyReport.GetNamespace(),
	}}

	operation, err := controllerutil.CreateOrPatch(ctx, s.client, oldPolicyReport, func() error {
		oldPolicyReport.ObjectMeta.Labels = policyReport.ObjectMeta.Labels
		oldPolicyReport.ObjectMeta.OwnerReferences = policyReport.ObjectMeta.OwnerReferences
		oldPolicyReport.Scope = policyReport.Scope
		oldPolicyReport.Summary = policyReport.Summary
		oldPolicyReport.Results = policyReport.Results

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create or patch policy report %s: %w", policyReport.GetName(), err)
	}

	s.logger.DebugContext(ctx, fmt.Sprintf("PolicyReport %s", operation),
		slog.String("report-name", policyReport.GetName()),
		slog.String("report-version", policyReport.GetResourceVersion()),
		slog.String("resource-name", policyReport.Scope.Name),
		slog.String("resource-namespace", policyReport.Scope.Namespace),
		slog.String("resource-version", policyReport.Scope.ResourceVersion))

	return nil
}

// DeleteOldReports deletes all the kubewarden-managed PolicyReports in the given
// namespace whose name is not present in keptReports (the reports created or
// patched during the current scan run).
func (s *PolicyReportStore) DeleteOldReports(ctx context.Context, keptReports sets.Set[string], namespace string) error {
	labelSelector, err := managedByKubewardenSelector()
	if err != nil {
		return err
	}
	s.logger.DebugContext(ctx, "Deleting old PolicyReports",
		slog.String("namespace", namespace),
		slog.Int("kept-reports", keptReports.Len()))

	return deleteReportsNotInSet(ctx, s.client, s.logger, &wgpolicy.PolicyReport{}, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     namespace,
	}, keptReports)
}

// CreateOrPatchClusterReport creates or patches a ClusterPolicyReport.
//
//nolint:dupl // Temporary duplicated code with openreports_store.go, it's planned to be removed in the near future.
func (s *PolicyReportStore) CreateOrPatchClusterReport(ctx context.Context, obj any) error {
	report, ok := obj.(*ClusterPolicyReport)
	if !ok {
		return fmt.Errorf("expected *PolicyReport, got %T", obj)
	}
	clusterPolicyReport := report.report
	oldClusterPolicyReport := &wgpolicy.ClusterPolicyReport{ObjectMeta: metav1.ObjectMeta{
		Name: clusterPolicyReport.GetName(),
	}}

	operation, err := controllerutil.CreateOrPatch(ctx, s.client, oldClusterPolicyReport, func() error {
		oldClusterPolicyReport.ObjectMeta.Labels = clusterPolicyReport.ObjectMeta.Labels
		oldClusterPolicyReport.ObjectMeta.OwnerReferences = clusterPolicyReport.ObjectMeta.OwnerReferences
		oldClusterPolicyReport.Scope = clusterPolicyReport.Scope
		oldClusterPolicyReport.Summary = clusterPolicyReport.Summary
		oldClusterPolicyReport.Results = clusterPolicyReport.Results

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create or patch cluster policy report %s: %w", clusterPolicyReport.GetName(), err)
	}

	s.logger.DebugContext(ctx, fmt.Sprintf("ClusterPolicyReport %s", operation),
		slog.String("report-name", clusterPolicyReport.GetName()),
		slog.String("report-version", clusterPolicyReport.GetResourceVersion()),
		slog.String("resource-name", clusterPolicyReport.Scope.Name),
		slog.String("resource-namespace", clusterPolicyReport.Scope.Namespace),
		slog.String("resource-version", clusterPolicyReport.Scope.ResourceVersion))

	return nil
}

// DeleteOldClusterReports deletes all the kubewarden-managed ClusterPolicyReports
// whose name is not present in keptReports (the reports created or patched during
// the current scan run).
func (s *PolicyReportStore) DeleteOldClusterReports(ctx context.Context, keptReports sets.Set[string]) error {
	labelSelector, err := managedByKubewardenSelector()
	if err != nil {
		return err
	}
	s.logger.DebugContext(ctx, "Deleting old ClusterPolicyReports",
		slog.Int("kept-reports", keptReports.Len()))

	return deleteReportsNotInSet(ctx, s.client, s.logger, &wgpolicy.ClusterPolicyReport{}, &client.ListOptions{
		LabelSelector: labelSelector,
	}, keptReports)
}
