package cachemanager

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/compatibility"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/localapp"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

// applicationResync rechecks a namespace's application placements: retention
// ends and the hourly limit move with time, not with events.
const applicationResync = 2 * time.Minute

// Event reasons of the application placement reconciler (API).
const (
	EventApplicationPlacementConflict = "ApplicationPlacementConflict"
	EventApplicationPlacementRejected = "ApplicationPlacementRejected"
)

// ApplicationReconciler keeps the SwiftArtifactPlacements that prepare
// SpinApps' own application artifacts on their warm pools' nodes (ADR 0015).
// One reconcile converges one namespace. It decides only with
// localapp.Decide and localapp.Select, which the controller also uses, and
// it never modifies a placement it does not own.
type ApplicationReconciler struct {
	Client   client.Client
	Defaults executor.Defaults
	Options  compatibility.Options
	Settings localapp.Settings
	Detector capabilities.SandboxDetector
	// PlacementsServed reports that the SwiftArtifactPlacement API exists.
	PlacementsServed bool
	Recorder         events.EventRecorder
	Now              func() time.Time
}

// SetupWithManager registers the reconciler.
func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	toNamespace := func(_ context.Context, o client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace()}}}
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("localapp-placements").
		Watches(&spinv1alpha1.SpinApp{}, handler.EnqueueRequestsFromMapFunc(toNamespace),
			builder.WithPredicates(predicate.Or[client.Object](predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&spinv1alpha1.SpinAppExecutor{}, handler.EnqueueRequestsFromMapFunc(toNamespace),
			builder.WithPredicates(predicate.Or[client.Object](predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}, predicate.LabelChangedPredicate{}))).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetName()}}}
		}), builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Watches(&sandboxv1alpha1.SwiftArtifactPlacement{}, handler.EnqueueRequestsFromMapFunc(toNamespace)).
		Complete(r)
}

// Reconcile converges one namespace's application placements.
func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ns := req.Namespace
	logger := log.FromContext(ctx).WithValues("namespace", ns)
	now := r.Now()

	wants, err := r.wants(ctx, ns)
	if err != nil {
		return ctrl.Result{}, err
	}
	var pls sandboxv1alpha1.SwiftArtifactPlacementList
	if err := r.Client.List(ctx, &pls, client.InNamespace(ns),
		client.MatchingLabels{"app.kubernetes.io/managed-by": "kubeswift-spin", executor.Prefix + "component": localapp.ComponentPlacement}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list application placements: %w", err)
	}
	owned := map[localapp.Key]*sandboxv1alpha1.SwiftArtifactPlacement{}
	byName := map[string]*sandboxv1alpha1.SwiftArtifactPlacement{}
	var existing []localapp.Existing
	for i := range pls.Items {
		pl := &pls.Items[i]
		byName[pl.Name] = pl
		k, ok := keyOf(pl)
		if !ok || !localapp.Owned(pl, k) {
			continue // not ours: never modified
		}
		owned[k] = pl
		e := localapp.Existing{Key: k, Created: pl.CreationTimestamp.Time}
		if v, ok := pl.Annotations[localapp.AnnUnwantedSince]; ok {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				e.UnwantedSince = &t
			}
		}
		existing = append(existing, e)
	}

	sel := localapp.Select(wants, existing, now, r.Settings)
	for _, k := range sel.Keep {
		if err := r.ensure(ctx, k, owned[k], byName[k.PlacementName()]); err != nil {
			return ctrl.Result{}, err
		}
	}
	for _, k := range sel.NewlyUnwanted {
		pl := owned[k].DeepCopy()
		if pl.Annotations == nil {
			pl.Annotations = map[string]string{}
		}
		pl.Annotations[localapp.AnnUnwantedSince] = now.UTC().Format(time.RFC3339)
		if err := r.Client.Update(ctx, pl); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return ctrl.Result{}, fmt.Errorf("mark placement %s unwanted: %w", pl.Name, err)
		}
	}
	for _, k := range sel.Delete {
		pl := owned[k]
		uid := pl.UID
		if err := r.Client.Delete(ctx, pl, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return ctrl.Result{}, fmt.Errorf("delete placement %s: %w", pl.Name, err)
		}
		logger.V(1).Info("deleted application placement", "placement", pl.Name)
	}
	return ctrl.Result{RequeueAfter: applicationResync}, nil
}

// wants returns what the namespace's SpinApps ask for. A disabled feature
// or namespace wants nothing, so its placements are retained and then
// removed like any other unwanted placement.
func (r *ApplicationReconciler) wants(ctx context.Context, ns string) ([]localapp.Want, error) {
	if !r.Settings.Enabled || !r.PlacementsServed {
		return nil, nil
	}
	var nsObj corev1.Namespace
	if err := r.Client.Get(ctx, types.NamespacedName{Name: ns}, &nsObj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read namespace: %w", err)
	}
	enabled := localapp.NamespaceEnabledFor(nsObj.Labels)
	if !enabled {
		return nil, nil
	}
	features := capabilities.Sandbox{}
	if r.Detector != nil {
		features, _ = r.Detector.Sandbox(ctx)
	}
	opts := r.Options
	opts.Features = features
	var apps spinv1alpha1.SpinAppList
	if err := r.Client.List(ctx, &apps, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list SpinApps: %w", err)
	}
	profiles := map[string]*executor.Profile{}
	var wants []localapp.Want
	for i := range apps.Items {
		app := &apps.Items[i]
		if !app.DeletionTimestamp.IsZero() || app.Spec.Executor == "" || app.Spec.Replicas == 0 {
			continue
		}
		p, seen := profiles[app.Spec.Executor]
		if !seen {
			var err error
			if p, err = r.profile(ctx, ns, app.Spec.Executor); err != nil {
				return nil, err
			}
			profiles[app.Spec.Executor] = p
		}
		if p == nil || compatibility.Blocking(compatibility.Analyze(app, p, opts)) {
			continue
		}
		k, skip := localapp.Decide(localapp.Input{App: app, Profile: p, Settings: r.Settings, NamespaceEnabled: true,
			Supported: features.LocalApplication() && r.PlacementsServed})
		if k == nil || skip != nil {
			continue
		}
		wants = append(wants, localapp.Want{Key: *k, Created: app.CreationTimestamp.Time, App: app.Name})
	}
	return wants, nil
}

// profile returns the executor's profile, or nil when it is missing, not
// kubeswift-spin's or invalid (its SpinApps want nothing).
func (r *ApplicationReconciler) profile(ctx context.Context, ns, name string) (*executor.Profile, error) {
	var e spinv1alpha1.SpinAppExecutor
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &e); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read SpinAppExecutor %s: %w", name, err)
	}
	if !executor.IsManaged(&e) {
		return nil, nil
	}
	p, err := executor.Parse(&e, r.Defaults)
	if err != nil {
		return nil, nil
	}
	return p, nil
}

// ensure creates or repairs the placement of a wanted key. A foreign object
// at the name is reported and left alone.
func (r *ApplicationReconciler) ensure(ctx context.Context, k localapp.Key, owned, atName *sandboxv1alpha1.SwiftArtifactPlacement) error {
	want := localapp.Placement(k)
	switch {
	case owned == nil && atName != nil:
		r.event(atName, corev1.EventTypeWarning, EventApplicationPlacementConflict,
			"an object not owned by kubeswift-spin has the name of an application placement; it is not modified")
		return nil
	case owned == nil:
		if err := r.Client.Create(ctx, want); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil // the next reconcile sees it
			}
			if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
				log.FromContext(ctx).Error(err, "application placement rejected", "placement", want.Name)
				return nil
			}
			return fmt.Errorf("create placement %s: %w", want.Name, err)
		}
		return nil
	}
	// Bring every non-key field back and clear the retention mark.
	pl := owned.DeepCopy()
	changed := false
	if _, ok := pl.Annotations[localapp.AnnUnwantedSince]; ok {
		delete(pl.Annotations, localapp.AnnUnwantedSince)
		changed = true
	}
	if !samePlacementSpec(pl.Spec, want.Spec) {
		pl.Spec = want.Spec
		changed = true
	}
	if !changed {
		return nil
	}
	if err := r.Client.Update(ctx, pl); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("update placement %s: %w", pl.Name, err)
	}
	return nil
}

func samePlacementSpec(a, b sandboxv1alpha1.SwiftArtifactPlacementSpec) bool {
	if a.PoolRef != b.PoolRef || len(a.Artifacts) != len(b.Artifacts) {
		return false
	}
	for i := range a.Artifacts {
		x, y := a.Artifacts[i], b.Artifacts[i]
		if x.Name != y.Name || x.Ref != y.Ref || x.Layout != y.Layout || x.RequirePlatformVerification != y.RequirePlatformVerification ||
			len(x.Claims) != len(y.Claims) || (x.PullSecretRef == nil) != (y.PullSecretRef == nil) ||
			(x.PullSecretRef != nil && x.PullSecretRef.Name != y.PullSecretRef.Name) {
			return false
		}
	}
	return true
}

// keyOf reconstructs the key a placement claims to be for.
func keyOf(pl *sandboxv1alpha1.SwiftArtifactPlacement) (localapp.Key, bool) {
	if len(pl.Spec.Artifacts) != 1 {
		return localapp.Key{}, false
	}
	a := pl.Spec.Artifacts[0]
	k := localapp.Key{Namespace: pl.Namespace, Pool: pl.Spec.PoolRef.Name, Ref: a.Ref}
	if a.PullSecretRef != nil {
		k.PullSecret = a.PullSecretRef.Name
	}
	return k, true
}

func (r *ApplicationReconciler) event(obj *sandboxv1alpha1.SwiftArtifactPlacement, typ, reason, note string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(obj, nil, typ, reason, "Reconcile", "%s", note)
}
