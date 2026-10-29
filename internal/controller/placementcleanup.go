package controller

import (
	"context"
	"fmt"
	"time"

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
// compiledcache.IsPlacement), once it is MinAge old, by UID and resource
// version precondition. The marks are labels, annotations and names in the
// placement's own namespace, so an object a tenant labels can only ever be
// the object deleted.
type PlacementCleanup struct {
	// LocalApplications is set when local applications are disabled;
	// their application placements are deleted (ADR 0015).
	LocalApplications bool
	// CompiledCaches is set when compiled caches are disabled; their
	// placements are deleted (ADR 0014).
	CompiledCaches bool
}

// Any reports whether any feature is disabled.
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

// DefaultPlacementCleanupMinAge is how old a placement must be before
// PlacementCleanup deletes it. Turning a feature on or off rolls both the
// controller and the cache manager, each with its own lease: for a while an
// old controller that cleans up after the feature may run beside a new cache
// manager that wants its placements, or the reverse. Waiting keeps them from
// deleting and recreating a placement in a loop, which would start
// KubeSwift's fetches again each time, even if a rollout stalls; it bounds
// that to one placement per key and period. Placements left behind by an
// earlier release are older and go at once.
const DefaultPlacementCleanupMinAge = 5 * time.Minute

// PlacementCleanupReconciler deletes the placements of disabled features.
type PlacementCleanupReconciler struct {
	Client  client.Client
	Cleanup PlacementCleanup
	// MinAge is how old a placement must be before it is deleted.
	MinAge time.Duration
	Now    func() time.Time
}

// SetupWithManager registers the reconciler. The informer lists every
// matching placement at start, so placements left behind by v0.3.0 and
// earlier, or while the controller was not running, are deleted too.
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
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	if age := now().Sub(pl.CreationTimestamp.Time); age < r.MinAge {
		return ctrl.Result{RequeueAfter: r.MinAge - age + time.Second}, nil
	}
	// Only the object that was checked: not a replacement, and not a version
	// changed since the cache read.
	uid, rv := pl.UID, pl.ResourceVersion
	if err := r.Client.Delete(ctx, &pl, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		if apierrors.IsConflict(err) {
			// Changed or replaced since the read: look again.
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("delete SwiftArtifactPlacement %s/%s: %w", pl.Namespace, pl.Name, err)
	}
	// The reconcile's logger already names the placement and its namespace.
	log.FromContext(ctx).Info("deleted a placement of a disabled feature", "feature", feature, "pool", pl.Spec.PoolRef.Name)
	return ctrl.Result{}, nil
}
