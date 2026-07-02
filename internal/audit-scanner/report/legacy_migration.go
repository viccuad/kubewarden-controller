package report

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	wgpolicy "sigs.k8s.io/wg-policy-prototypes/policy-report/pkg/api/wgpolicyk8s.io/v1alpha2"
)

// DeleteAllLegacyPolicyReports deletes all wgpolicyk8s.io PolicyReports and
// ClusterPolicyReports labelled app.kubernetes.io/managed-by=kubewarden.
//
// This is called once per scan when scans save openreports.
//
// The deletion is performed with a single deletecollection api call per kind,
// which is cheap and free of read-after-write races: every kubewarden-managed
// legacy report is deleted unconditionally, so there is no risk of removing a
// report that was just written by the current scan (openreports are a different
// kind and are never matched here).
//
// For reducing cost in subsequent runs, it checks for the existence of legacy
// wgpolicyk8s.io reports before attempting deletion: in clusters with many
// namespaces the cost after the first migration is just two cheap List calls.
func DeleteAllLegacyPolicyReports(ctx context.Context, c client.Client, logger *slog.Logger) error {
	labelSelector, err := labels.Parse(fmt.Sprintf("%s=%s", labelAppManagedBy, labelApp))
	if err != nil {
		return fmt.Errorf("failed to parse label selector: %w", err)
	}
	// After first migration, we will perform 2 list calls, one for
	// ClusterPolicyReport, another for PolicyReport, both with limit=1.
	listOpts := &client.ListOptions{LabelSelector: labelSelector, Limit: 1}

	clusterReportList := &wgpolicy.ClusterPolicyReportList{}
	err = c.List(ctx, clusterReportList, listOpts)
	switch {
	case meta.IsNoMatchError(err):
		logger.DebugContext(ctx, "wgpolicyk8s.io CRDs not installed, skipping legacy clusterreport cleanup")
	case err != nil:
		return fmt.Errorf("failed to list legacy ClusterPolicyReports: %w", err)
	case len(clusterReportList.Items) > 0:
		logger.InfoContext(ctx, "Deleting legacy wgpolicyk8s.io ClusterPolicyReports")
		if err = c.DeleteAllOf(ctx, &wgpolicy.ClusterPolicyReport{}, &client.DeleteAllOfOptions{
			ListOptions: client.ListOptions{LabelSelector: labelSelector},
		}); err != nil {
			return fmt.Errorf("failed to delete legacy ClusterPolicyReports: %w", err)
		}
	}

	policyReportList := &wgpolicy.PolicyReportList{}
	err = c.List(ctx, policyReportList, listOpts)
	switch {
	case meta.IsNoMatchError(err):
		logger.DebugContext(ctx, "wgpolicyk8s.io CRDs not installed, skipping legacy report cleanup")
	case err != nil:
		return fmt.Errorf("failed to list legacy PolicyReports: %w", err)
	case len(policyReportList.Items) > 0:
		logger.InfoContext(ctx, "Deleting legacy wgpolicyk8s.io PolicyReports")
		if err = c.DeleteAllOf(ctx, &wgpolicy.PolicyReport{}, &client.DeleteAllOfOptions{
			ListOptions: client.ListOptions{LabelSelector: labelSelector},
		}); err != nil {
			return fmt.Errorf("failed to delete legacy PolicyReports: %w", err)
		}
	}

	return nil
}
