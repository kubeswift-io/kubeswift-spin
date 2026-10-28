package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/localapp"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

// PlacementCleanup removes the SwiftArtifactPlacements of a disabled feature.
// While a feature is enabled, the cache manager keeps exactly the
// placements it needs and deletes the others; once the feature is disabled
// the chart stops running that part of the cache manager (or all of it),
// so nothing else would delete them. The controller always runs, so it
// deletes them: every placement of a disabled feature is unwanted.
//
// A placement is deleted only when it carries every mark the cache
// manager puts on that feature's placements (localapp.IsPlacement,
// compiledcache.IsPlacement), by UID precondition. The marks are labels,
// annotations and names in the placement's own namespace, so an object a
// tenant labels can only ever be the object deleted.
type PlacementCleanup struct {
	// LocalApplications deletes application placements (ADR 0015):
	// local applications are disabled.
	LocalApplications bool
	// CompiledCaches deletes compiled-cache placements (ADR 0014):
	// compiled caches are disabled.
	CompiledCaches bool
}

// Any reports whether a feature is disabled.
func (c PlacementCleanup) Any() bool { return c.LocalApplications || c.CompiledCaches }

// Components are the component label values of the placements to delete.
func (c PlacementCleanup) Components() []string {
	var out []string
	if c.LocalApplications {
		out = append(out, localapp.ComponentPlacement)
	}
	if c.CompiledCaches {
		out = append(out, compiledcache.ComponentPlacement)
	}
	return out
}

// feature names the disabled feature pl belongs to, or "" when pl is not
// kubeswift-spin's placement of a disabled feature.
func (c PlacementCleanup) feature(pl *sandboxv1alpha1.SwiftArtifactPlacement) string {
	switch {
	case c.LocalApplications && localapp.IsPlacement(pl):
		return "local applications"
	case c.CompiledCaches && compiledcache.IsPlacement(pl):
		return "compiled caches"
	}
	return ""
}

// PlacementCleanupReconciler deletes the placements of disabled features.
type PlacementCleanupReconciler struct {
	Client  client.Client
	Cleanup PlacementCleanup
}

// SetupWithManager registers the reconciler. The informer lists every
// matching placement at start, so placements left behind while no
// component deleted them (by v0.3.0, or before a restart) are deleted too.
func (r *PlacementCleanupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	components := r.Cleanup.Components()
	return ctrl.NewControllerManagedBy(mgr).
		Named("placement-cleanup").
		For(&sandboxv1alpha1.SwiftArtifactPlacement{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			l := o.GetLabels()
			if l[translate.LabelManagedBy] != translate.ManagedByValue {
				return false
			}
			for _, c := range components {
				if l[compiledcache.LabelComponent] == c {
					return true
				}
			}
			return false
		}))).
		Complete(r)
}

// Reconcile deletes one placement if it belongs to a disabled feature.
func (r *PlacementCleanupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pl sandboxv1alpha1.SwiftArtifactPlacement
	if err := r.Client.Get(ctx, req.NamespacedName, &pl); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	feature := r.Cleanup.feature(&pl)
	if feature == "" || !pl.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	uid := pl.UID
	if err := r.Client.Delete(ctx, &pl, client.Preconditions{UID: &uid}); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// Gone, or replaced by another object, which has its own event.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("delete SwiftArtifactPlacement %s/%s: %w", pl.Namespace, pl.Name, err)
	}
	// The reconcile's logger already names the placement and its namespace.
	log.FromContext(ctx).Info("deleted a placement of a disabled feature", "feature", feature, "pool", pl.Spec.PoolRef.Name)
	return ctrl.Result{}, nil
}
