package report

import (
	"context"
	"fmt"
	"log/slog"

	openreports "github.com/openreports/reports-api/apis/openreports.io/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// OpenReportStore is a store for OpenReports Report and ClusterReport.
type OpenReportStore struct {
	// client is a controller-runtime client that knows about PolicyReport and ClusterPolicyReport CRDs
	client client.Client
	// logger is used to log the messages
	logger *slog.Logger
}

// NewOpenReportStore creates a new PolicyReportStore.
func NewOpenReportStore(client client.Client, logger *slog.Logger) Store {
	return &OpenReportStore{
		client: client,
		logger: logger.With("component", "policyreportstore"),
	}
}

// CreateOrPatchReport creates or patches a OpenReports Report.
func (s *OpenReportStore) CreateOrPatchReport(ctx context.Context, obj any) error {
	openReport, ok := obj.(*OpenReport)
	if !ok {
		return fmt.Errorf("expected *OpenReport, got %T", obj)
	}
	policyReport := openReport.report
	oldPolicyReport := &openreports.Report{ObjectMeta: metav1.ObjectMeta{
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

// DeleteOldReports deletes all the kubewarden-managed OpenReports Reports in the
// given namespace whose name is not present in keptReports (the reports created
// or patched during the current scan run).
func (s *OpenReportStore) DeleteOldReports(ctx context.Context, keptReports sets.Set[string], namespace string) error {
	labelSelector, err := managedByKubewardenSelector()
	if err != nil {
		return err
	}
	s.logger.DebugContext(ctx, "Deleting old Reports",
		slog.String("namespace", namespace),
		slog.Int("kept-reports", keptReports.Len()))

	return deleteReportsNotInSet(ctx, s.client, s.logger, &openreports.Report{}, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     namespace,
	}, keptReports)
}

// CreateOrPatchClusterReport creates or patches a OpenReports ClusterReport.
//
//nolint:dupl // Temporary duplicated code with policyreports_store.go, it's planned to be the only implementation in the future.
func (s *OpenReportStore) CreateOrPatchClusterReport(ctx context.Context, obj any) error {
	openReport, ok := obj.(*OpenClusterReport)
	if !ok {
		return fmt.Errorf("expected *OpenReport, got %T", obj)
	}
	clusterPolicyReport := openReport.report
	oldClusterPolicyReport := &openreports.ClusterReport{ObjectMeta: metav1.ObjectMeta{
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

// DeleteOldClusterReports deletes all the kubewarden-managed OpenReports
// ClusterReports whose name is not present in keptReports (the reports created
// or patched during the current scan run).
func (s *OpenReportStore) DeleteOldClusterReports(ctx context.Context, keptReports sets.Set[string]) error {
	labelSelector, err := managedByKubewardenSelector()
	if err != nil {
		return err
	}
	s.logger.DebugContext(ctx, "Deleting old ClusterReports",
		slog.Int("kept-reports", keptReports.Len()))

	return deleteReportsNotInSet(ctx, s.client, s.logger, &openreports.ClusterReport{}, &client.ListOptions{
		LabelSelector: labelSelector,
	}, keptReports)
}
