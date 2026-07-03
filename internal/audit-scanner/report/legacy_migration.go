package report

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	wgpolicy "sigs.k8s.io/wg-policy-prototypes/policy-report/pkg/api/wgpolicyk8s.io/v1alpha2"
)

// DeleteAllLegacyPolicyReports deletes all wgpolicyk8s.io PolicyReports and
// ClusterPolicyReports labelled app.kubernetes.io/managed-by=kubewarden.
//
// This is called once per scan when scans save openreports.
//
// ClusterPolicyReport is cluster-scoped, so it is deleted with a single
// deletecollection call, free of read-after-write races: every
// kubewarden-managed legacy cluster report is deleted unconditionally, so
// there is no risk of removing a report that was just written by the current
// scan (openreports are a different kind and are never matched here).
//
// PolicyReport is namespaced, and the Kubernetes API only exposes
// deletecollection on the namespaced route: there is no way to delete a
// namespaced kind across every namespace with a single call. So namespaces
// that actually hold legacy reports are discovered first with a metadata-only
// List across all namespaces, and one deletecollection call is issued per
// affected namespace.
//
// For reducing cost in subsequent runs, it checks for the existence of legacy
// wgpolicyk8s.io reports before attempting deletion: in clusters with many
// namespaces the cost after the first migration is just two cheap List calls.
func DeleteAllLegacyPolicyReports(ctx context.Context, c client.Client, logger *slog.Logger) error {
	labelSelector, err := managedByKubewardenSelector()
	if err != nil {
		return err
	}
	// After first migration, we will perform 2 list calls, one for
	// ClusterPolicyReport, another for PolicyReport, both with limit=1.
	probeOpts := &client.ListOptions{LabelSelector: labelSelector, Limit: 1}

	clusterReportList := &wgpolicy.ClusterPolicyReportList{}
	err = c.List(ctx, clusterReportList, probeOpts)
	switch {
	case meta.IsNoMatchError(err):
		logger.DebugContext(ctx, "wgpolicyk8s.io CRDs not installed, skipping legacy clusterreport cleanup")
	case err != nil:
		return fmt.Errorf("failed to list legacy ClusterPolicyReports: %w", err)
	case len(clusterReportList.Items) > 0:
		logger.InfoContext(ctx, "Deleting legacy wgpolicyk8s.io ClusterPolicyReports")
		if deleteErr := c.DeleteAllOf(ctx, &wgpolicy.ClusterPolicyReport{}, &client.DeleteAllOfOptions{
			ListOptions: client.ListOptions{LabelSelector: labelSelector},
		}); deleteErr != nil {
			return fmt.Errorf("failed to delete legacy ClusterPolicyReports: %w", deleteErr)
		}
	}

	policyReportProbe := &wgpolicy.PolicyReportList{}
	err = c.List(ctx, policyReportProbe, probeOpts)
	switch {
	case meta.IsNoMatchError(err):
		logger.DebugContext(ctx, "wgpolicyk8s.io CRDs not installed, skipping legacy report cleanup")
		return nil
	case err != nil:
		return fmt.Errorf("failed to list legacy PolicyReports: %w", err)
	case len(policyReportProbe.Items) == 0:
		return nil
	}

	namespaces, err := legacyPolicyReportNamespaces(ctx, c, labelSelector)
	if err != nil {
		return fmt.Errorf("failed to list legacy PolicyReport namespaces: %w", err)
	}

	for ns := range namespaces {
		logger.InfoContext(ctx, "Deleting legacy wgpolicyk8s.io PolicyReports", slog.String("namespace", ns))
		if deleteErr := c.DeleteAllOf(ctx, &wgpolicy.PolicyReport{}, &client.DeleteAllOfOptions{
			ListOptions: client.ListOptions{LabelSelector: labelSelector, Namespace: ns},
		}); deleteErr != nil {
			return fmt.Errorf("failed to delete legacy PolicyReports in namespace %s: %w", ns, deleteErr)
		}
	}

	return nil
}

// legacyPolicyReportNamespaces returns the set of namespaces holding at least
// one kubewarden-managed legacy PolicyReport. The list only fetches object
// metadata (name/namespace), since that's all that's needed to determine which
// namespaces to target with deletecollection.
func legacyPolicyReportNamespaces(ctx context.Context, c client.Client, labelSelector labels.Selector) (sets.Set[string], error) {
	gvk, err := apiutil.GVKForObject(&wgpolicy.PolicyReport{}, c.Scheme())
	if err != nil {
		return nil, fmt.Errorf("failed to get GroupVersionKind for PolicyReport: %w", err)
	}
	listGVK := gvk
	listGVK.Kind += "List"

	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(listGVK)
	if listErr := c.List(ctx, list, &client.ListOptions{LabelSelector: labelSelector}); listErr != nil {
		return nil, fmt.Errorf("failed to list PolicyReports: %w", listErr)
	}

	namespaces := sets.New[string]()
	for i := range list.Items {
		namespaces.Insert(list.Items[i].GetNamespace())
	}
	return namespaces, nil
}
