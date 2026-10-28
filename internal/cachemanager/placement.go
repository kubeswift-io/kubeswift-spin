package cachemanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

// Placement status reasons of the cache manager's own: an object it did not
// create holds the placement's name, or the API server refused to create
// or update the placement (a quota or an admission policy).
const (
	ReasonPlacementConflict = "PlacementConflict"
	ReasonPlacementRejected = "PlacementRejected"
)

// placementRecheck is when a placement blocked by another object or a
// refusal is looked at again; nothing the cache manager watches says when
// that changes.
const placementRecheck = 2 * time.Minute

// maxPlacementMessage bounds the placement message copied into the record.
const maxPlacementMessage = 512

// placementLabels mark a placement of record.
func placementLabels(record string) map[string]string {
	return map[string]string{
		translate.LabelManagedBy:     translate.ManagedByValue,
		compiledcache.LabelComponent: compiledcache.ComponentPlacement,
		compiledcache.LabelRecord:    record,
	}
}

// ownPlacement reports whether p is a placement of rec: it carries the
// record's labels and its UID.
func ownPlacement(p *sandboxv1alpha1.SwiftArtifactPlacement, rec *cachev1alpha1.SpinCompiledCache) bool {
	for k, v := range placementLabels(rec.Name) {
		if p.Labels[k] != v {
			return false
		}
	}
	return p.Annotations[compiledcache.AnnRecordUID] == string(rec.UID)
}

// recordsForPlacement enqueues the record of a placement. A placement whose
// record is gone is deleted by that record's reconcile.
func (r *RecordReconciler) recordsForPlacement(_ context.Context, o client.Object) []reconcile.Request {
	if name := o.GetLabels()[compiledcache.LabelRecord]; name != "" {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Settings.Namespace, Name: name}}}
	}
	return nil
}

// placements lists the placements of record (in every namespace when ns is
// empty).
func (r *RecordReconciler) placements(ctx context.Context, ns, record string) ([]sandboxv1alpha1.SwiftArtifactPlacement, error) {
	var l sandboxv1alpha1.SwiftArtifactPlacementList
	opts := []client.ListOption{client.MatchingLabels(placementLabels(record))}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	if err := r.Client.List(ctx, &l, opts...); err != nil {
		return nil, fmt.Errorf("list SwiftArtifactPlacements: %w", err)
	}
	return l.Items, nil
}

// deletePlacements deletes the placements of record whose pool keep does not
// want, each by UID precondition.
func (r *RecordReconciler) deletePlacements(ctx context.Context, ns, record string, keep func(*sandboxv1alpha1.SwiftArtifactPlacement) bool) error {
	list, err := r.placements(ctx, ns, record)
	if err != nil {
		return err
	}
	for i := range list {
		p := &list[i]
		if !p.DeletionTimestamp.IsZero() || (keep != nil && keep(p)) {
			continue
		}
		uid := p.UID
		if err := r.Client.Delete(ctx, p, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return fmt.Errorf("delete SwiftArtifactPlacement %s/%s: %w", p.Namespace, p.Name, err)
		}
		log.FromContext(ctx).Info("deleted placement", "placement", p.Name, "placementNamespace", p.Namespace, "pool", p.Spec.PoolRef.Name)
	}
	return nil
}

// place keeps one SwiftArtifactPlacement per warm pool the record's current
// consumers check out from, while the record is trusted, and copies their
// state into status.placements. A placement prepares exactly what a
// replica mounts: the same reference, layout, claims and pull Secret. One
// pool's failure never holds back the others or the record's conditions.
// It returns when to look again if no watch event will say so.
func (r *RecordReconciler) place(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache) (time.Duration, error) {
	artifact := compiledcache.TrustedArtifact(rec)
	var pools []string
	if artifact != "" {
		pools = rec.Status.Pools
	}
	ns := rec.Spec.Namespace
	if err := r.deletePlacements(ctx, ns, rec.Name, func(p *sandboxv1alpha1.SwiftArtifactPlacement) bool {
		// Another record's object with forged labels is never deleted here;
		// one of this record's is kept only for a pool it still needs.
		return !ownPlacement(p, rec) ||
			(slices.Contains(pools, p.Spec.PoolRef.Name) && p.Name == compiledcache.PlacementName(rec.Name, p.Spec.PoolRef.Name))
	}); err != nil {
		return 0, err
	}
	id := compiledcache.IdentityOf(rec)
	statuses := make([]cachev1alpha1.PlacementStatus, 0, len(pools))
	var errs []error
	var requeue time.Duration
	for _, pool := range pools {
		st, err := r.ensurePlacement(ctx, rec, pool, r.Delivery.PlacementArtifact(id, artifact))
		if err != nil {
			errs = append(errs, err)
			st.Reason, st.Message = "Preparing", ""
		}
		if st.Reason == ReasonPlacementConflict || st.Reason == ReasonPlacementRejected {
			requeue = placementRecheck
		}
		statuses = append(statuses, st)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Pool < statuses[j].Pool })
	if err := r.patchPlacements(ctx, rec, statuses); err != nil {
		return 0, err
	}
	return requeue, errors.Join(errs...)
}

// ensurePlacement creates or updates the placement of rec on pool and
// returns its state. A refusal by the API server is reported in the state;
// other errors are returned.
func (r *RecordReconciler) ensurePlacement(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache, pool string, a sandboxv1alpha1.PlacementArtifact) (cachev1alpha1.PlacementStatus, error) {
	name := compiledcache.PlacementName(rec.Name, pool)
	st := cachev1alpha1.PlacementStatus{Pool: pool, Name: name}
	want := sandboxv1alpha1.SwiftArtifactPlacementSpec{
		PoolRef:   corev1.LocalObjectReference{Name: pool},
		Artifacts: []sandboxv1alpha1.PlacementArtifact{a},
	}
	rejected := func(verb string, err error) (cachev1alpha1.PlacementStatus, error) {
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			// A quota or an admission policy: the API message can echo
			// values, so only the log carries it.
			log.FromContext(ctx).Error(err, "SwiftArtifactPlacement "+verb+" refused", "placement", name, "pool", pool)
			st.Reason = ReasonPlacementRejected
			st.Message = fmt.Sprintf("the API server refused to %s SwiftArtifactPlacement %s in namespace %s; check ResourceQuota and admission policies there, and the cache manager log", verb, name, rec.Spec.Namespace)
			return st, nil
		}
		return st, fmt.Errorf("%s SwiftArtifactPlacement %s/%s: %w", verb, rec.Spec.Namespace, name, err)
	}
	conflict := func() (cachev1alpha1.PlacementStatus, error) {
		st.Reason = ReasonPlacementConflict
		st.Message = fmt.Sprintf("SwiftArtifactPlacement %s in namespace %s was not created by the cache manager for pool %s; remove it to let the cache manager place the cache there", name, rec.Spec.Namespace, pool)
		return st, nil
	}
	var cur sandboxv1alpha1.SwiftArtifactPlacement
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: rec.Spec.Namespace, Name: name}, &cur)
	switch {
	case apierrors.IsNotFound(err):
		p := &sandboxv1alpha1.SwiftArtifactPlacement{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: rec.Spec.Namespace, Labels: placementLabels(rec.Name),
				Annotations: map[string]string{compiledcache.AnnRecordUID: string(rec.UID)}},
			Spec: want,
		}
		if err := r.Client.Create(ctx, p); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Created in between, by this process or not: look again.
				st.Reason = "Preparing"
				return st, nil
			}
			return rejected("create", err)
		}
		log.FromContext(ctx).Info("created placement", "placement", name, "pool", pool)
		st.Reason = "Preparing"
		return st, nil
	case err != nil:
		return st, fmt.Errorf("read SwiftArtifactPlacement %s/%s: %w", rec.Spec.Namespace, name, err)
	}
	// Never take over an object the cache manager did not create for this
	// record and pool (two pools whose names share a hash prefix included).
	if !ownPlacement(&cur, rec) || cur.Spec.PoolRef.Name != pool {
		return conflict()
	}
	if !cur.DeletionTimestamp.IsZero() {
		st.Reason = "Preparing"
		return st, nil
	}
	if !apiequality.Semantic.DeepEqual(cur.Spec, want) {
		cur.Spec = want
		if err := r.Client.Update(ctx, &cur); err != nil {
			if apierrors.IsConflict(err) {
				st.Reason = "Preparing"
				return st, err
			}
			return rejected("update", err)
		}
		log.FromContext(ctx).Info("updated placement", "placement", name, "pool", pool)
		st.Reason = "Preparing"
		return st, nil
	}
	c := apimeta.FindStatusCondition(cur.Status.Conditions, sandboxv1alpha1.PlacementConditionPrepared)
	switch {
	case c == nil || cur.Status.ObservedGeneration < cur.Generation || c.ObservedGeneration < cur.Generation:
		// KubeSwift has not acted on this generation yet.
		st.Reason = "Preparing"
	default:
		st.PreparedNodes, st.TargetNodes = cur.Status.PreparedNodes, cur.Status.TargetNodes
		st.Prepared = c.Status == metav1.ConditionTrue
		st.Reason = c.Reason
		st.Message = truncate(c.Message, maxPlacementMessage)
	}
	return st, nil
}

// patchPlacements writes status.placements when it changed. Only that
// field and the resourceVersion are sent.
func (r *RecordReconciler) patchPlacements(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache, statuses []cachev1alpha1.PlacementStatus) error {
	if len(statuses) == 0 {
		statuses = nil
	}
	if apiequality.Semantic.DeepEqual(statuses, rec.Status.Placements) {
		return nil
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": rec.ResourceVersion},
		"status":   map[string]any{"placements": statuses},
	})
	if err != nil {
		return err
	}
	if err := r.Client.Status().Patch(ctx, rec, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("patch SpinCompiledCache %s placements: %w", rec.Name, err)
	}
	rec.Status.Placements = statuses
	return nil
}
