package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	sigsyaml "sigs.k8s.io/yaml"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kubewarden/adm-controller/internal/constants"
)

// DefaultsApplierReconciler watches a ConfigMap containing default Kubewarden
// resources (PolicyServer, ClusterAdmissionPolicy, etc.) and applies them to
// the cluster. It injects ownership labels and cleans up stale managed resources.

//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch,namespace=kubewarden
//+kubebuilder:rbac:groups=policies.kubewarden.io,resources=policyservers;clusteradmissionpolicies;admissionpolicies;clusteradmissionpolicygroups;admissionpolicygroups,verbs=get;list;watch;create;update;patch;delete

type DefaultsApplierReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	Log                  logr.Logger
	DeploymentsNamespace string
	ConfigMapName        string
}

// Reconcile watches the defaults ConfigMap and applies the resources it contains.
func (r *DefaultsApplierReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("configmap", req.NamespacedName)

	var cm corev1.ConfigMap
	if err := r.Get(ctx, req.NamespacedName, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("ConfigMap not found, cleaning up all managed resources")
			if cleanupErr := r.cleanupAll(ctx); cleanupErr != nil {
				return ctrl.Result{}, fmt.Errorf("failed to cleanup all managed resources: %w", cleanupErr)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ConfigMap: %w", err)
	}

	desired := sets.New[resourceKey]()

	for key, yamlData := range cm.Data {
		obj := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal([]byte(yamlData), obj); err != nil {
			log.Error(err, "failed to decode resource from ConfigMap", "key", key)
			// Don't fail the whole reconciliation for one bad entry
			continue
		}

		gvk := obj.GroupVersionKind()
		if !isSupportedGVK(gvk) {
			log.Info("Unsupported resource type, skipping", "key", key, "gvk", gvk.String())
			continue
		}

		// Track this resource as desired
		rk := resourceKey{
			gvk:       gvk.String(),
			name:      obj.GetName(),
			namespace: obj.GetNamespace(),
		}
		desired.Insert(rk)

		// Apply the resource with ownership label injected
		if applyErr := r.applyResource(ctx, obj); applyErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to apply resource %s: %w", rk, applyErr)
		}
	}

	if err := r.cleanupStale(ctx, desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to cleanup stale resources: %w", err)
	}

	log.Info("Reconciliation complete", "appliedResources", len(desired))
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *DefaultsApplierReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			return object.GetName() == r.ConfigMapName &&
				object.GetNamespace() == r.DeploymentsNamespace
		})).
		Complete(r); err != nil {
		return fmt.Errorf("failed to create DefaultsApplier controller: %w", err)
	}
	return nil
}

// applyResource applies the desired resource using Server-Side Apply. Only the fields present
// in the ConfigMap YAML are sent, so the API server applies CRD schema defaults (e.g.
// spec.backgroundAudit=true) for any field the template omits. SSA also manages field ownership:
// keys removed from the YAML are pruned, and re-applying identical content is a no-op.
func (r *DefaultsApplierReconciler) applyResource(ctx context.Context, desired *unstructured.Unstructured) error {
	log := r.Log.WithValues("resource", client.ObjectKeyFromObject(desired), "kind", desired.GetKind())

	// The status subresource cannot be set via an apply to the main resource; drop it so it
	// does not end up in our managed field set.
	unstructured.RemoveNestedField(desired.Object, "status")

	// Stamp the ownership label used by cleanupStale to identify managed resources.
	labels := desired.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[constants.DefaultsManagedByLabelKey] = constants.DefaultsManagedByLabelValue
	desired.SetLabels(labels)

	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(desired),
		client.FieldOwner(constants.DefaultsManagedByLabelValue),
		client.ForceOwnership,
	); err != nil {
		return fmt.Errorf("failed to server-side apply resource: %w", err)
	}

	log.V(1).Info("Resource applied successfully")
	return nil
}

// isSupportedGVK returns true if the GroupVersionKind is a known Kubewarden resource type.
func isSupportedGVK(gvk schema.GroupVersionKind) bool {
	if gvk.Group != constants.KubewardenPoliciesGroup {
		return false
	}
	switch gvk.Kind {
	case "PolicyServer",
		"ClusterAdmissionPolicy",
		"AdmissionPolicy",
		"ClusterAdmissionPolicyGroup",
		"AdmissionPolicyGroup":
		return true
	default:
		return false
	}
}

// cleanupStale removes managed resources that are not in the desired set.
func (r *DefaultsApplierReconciler) cleanupStale(ctx context.Context, desired sets.Set[resourceKey]) error {
	managedSelector := client.MatchingLabels{
		constants.DefaultsManagedByLabelKey: constants.DefaultsManagedByLabelValue,
	}

	gvks := []schema.GroupVersionKind{
		{Group: constants.KubewardenPoliciesGroup, Version: "v1", Kind: "PolicyServerList"},
		{Group: constants.KubewardenPoliciesGroup, Version: "v1", Kind: "ClusterAdmissionPolicyList"},
		{Group: constants.KubewardenPoliciesGroup, Version: "v1", Kind: "AdmissionPolicyList"},
		{Group: constants.KubewardenPoliciesGroup, Version: "v1", Kind: "ClusterAdmissionPolicyGroupList"},
		{Group: constants.KubewardenPoliciesGroup, Version: "v1", Kind: "AdmissionPolicyGroupList"},
	}

	for _, gvk := range gvks {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)

		if err := r.List(ctx, list, managedSelector); err != nil {
			return fmt.Errorf("failed to list managed resources: %w", err)
		}

		for i := range list.Items {
			item := &list.Items[i]
			rk := resourceKey{
				gvk:       item.GetObjectKind().GroupVersionKind().String(),
				name:      item.GetName(),
				namespace: item.GetNamespace(),
			}

			if !desired.Has(rk) {
				r.Log.Info("Deleting stale managed resource", "resource", rk)
				if deleteErr := r.Delete(ctx, item); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
					return fmt.Errorf("failed to delete stale resource %s: %w", rk, deleteErr)
				}
			}
		}
	}

	return nil
}

// cleanupAll removes all managed resources (called when ConfigMap is absent).
func (r *DefaultsApplierReconciler) cleanupAll(ctx context.Context) error {
	return r.cleanupStale(ctx, sets.New[resourceKey]())
}

// resourceKey uniquely identifies a Kubernetes resource.
type resourceKey struct {
	gvk       string
	name      string
	namespace string
}

func (rk resourceKey) String() string {
	if rk.namespace == "" {
		return fmt.Sprintf("%s/%s", rk.gvk, rk.name)
	}
	return fmt.Sprintf("%s/%s/%s", rk.gvk, rk.namespace, rk.name)
}
