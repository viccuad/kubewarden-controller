package report

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// Store is an interface to abstract the storage of reports. It's agnostic to the
// kind of report used (PolicyReport or OpenReport).
type Store interface {
	CreateOrPatchReport(ctx context.Context, report any) error
	// DeleteOldReports deletes all the kubewarden-managed Reports in the given
	// namespace whose name is not present in keptReports. keptReports holds the
	// names of the reports created or patched during the current scan run.
	//
	// Deleting by an explicit set of names (instead of a "run-uid != current"
	// label selector) avoids a read-after-write race: a collection delete
	// evaluates its selector against a possibly stale API server cache, which
	// can wrongly match — and therefore delete — reports that were just patched
	// with the current run UID.
	DeleteOldReports(ctx context.Context, keptReports sets.Set[string], namespace string) error
	CreateOrPatchClusterReport(ctx context.Context, report any) error
	// DeleteOldClusterReports deletes all the kubewarden-managed ClusterReports
	// whose name is not present in keptReports. See DeleteOldReports for the
	// rationale behind deleting by name instead of by label selector.
	DeleteOldClusterReports(ctx context.Context, keptReports sets.Set[string]) error
}

func NewReportStoreOfKind(kind CrdKind, client client.Client, logger *slog.Logger) Store {
	if kind == ReportKindPolicyReport {
		return NewPolicyReportStore(client, logger)
	}
	return NewOpenReportStore(client, logger)
}

// managedByKubewardenSelector returns a label selector matching all the reports
// managed by the audit scanner.
func managedByKubewardenSelector() (labels.Selector, error) {
	selector, err := labels.Parse(fmt.Sprintf("%s=%s", labelAppManagedBy, labelApp))
	if err != nil {
		return nil, fmt.Errorf("failed to parse label selector: %w", err)
	}
	return selector, nil
}

// deleteReportsNotInSet lists the kubewarden-managed reports of the kind
// represented by sample (matching listOpts) and deletes those whose name is
// not present in keptReports, which holds the names of the reports created or
// patched during the current scan run.
//
// The list only fetches object metadata (a PartialObjectMetadataList), not the
// full report body (labels, scope and results): GC only needs name, UID,
// ResourceVersion and namespace, so this avoids transferring and decoding the
// full report payload for every managed report on every scan.
//
// Each report is deleted with UID and ResourceVersion preconditions (optimistic
// concurrency), so a report that was concurrently re-created or updated (for
// example by an overlapping scan) is left untouched: such conflicts, as well as
// not-found errors, are expected and ignored.
func deleteReportsNotInSet(
	ctx context.Context,
	c client.Client,
	logger *slog.Logger,
	sample client.Object,
	listOpts *client.ListOptions,
	keptReports sets.Set[string],
) error {
	gvk, err := apiutil.GVKForObject(sample, c.Scheme())
	if err != nil {
		return fmt.Errorf("failed to get GroupVersionKind for %T: %w", sample, err)
	}
	listGVK := gvk
	listGVK.Kind += "List"

	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(listGVK)

	if listErr := c.List(ctx, list, listOpts); listErr != nil {
		return fmt.Errorf("failed to list reports: %w", listErr)
	}

	var errs []error
	for i := range list.Items {
		report := &list.Items[i]
		// The API server sets Kind/APIVersion on each returned item, but set it
		// explicitly too so Delete knows which resource to target regardless.
		report.SetGroupVersionKind(gvk)

		if keptReports.Has(report.GetName()) {
			continue
		}
		if derr := deleteReport(ctx, c, logger, report); derr != nil {
			errs = append(errs, derr)
		}
	}
	return errors.Join(errs...)
}

// deleteReport deletes a single report guarding the operation with UID and
// ResourceVersion preconditions. Not-found and conflict errors are treated as
// success: they mean the report is already gone or was modified concurrently.
func deleteReport(ctx context.Context, c client.Client, logger *slog.Logger, report client.Object) error {
	err := c.Delete(ctx, report, client.Preconditions{
		UID:             ptr.To(report.GetUID()),
		ResourceVersion: ptr.To(report.GetResourceVersion()),
	})
	switch {
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		logger.DebugContext(ctx, "skipping deletion of report modified concurrently",
			slog.String("name", report.GetName()),
			slog.String("namespace", report.GetNamespace()))
		return nil
	case err != nil:
		return fmt.Errorf("failed to delete report %s: %w", report.GetName(), err)
	default:
		return nil
	}
}
