package cachemanager

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

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

// ReasonPlacementConflict is a placement status reason: an object the cache
// manager did not create holds the placement's name.
const ReasonPlacementConflict = "PlacementConflict"

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

// ownPlacement reports whether p carries the labels of a placement of
// record.
func ownPlacement(p *sandboxv1alpha1.SwiftArtifactPlacement, record string) bool {
	for k, v := range placementLabels(record) {
		if p.Labels[k] != v {
			return false
		}
	}
	return true
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
// replica mounts: the same reference, layout, claims and pull Secret.
func (r *RecordReconciler) place(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache) error {
	artifact := compiledcache.TrustedArtifact(rec)
	var pools []string
	if artifact != "" {
		pools = rec.Status.Pools
	}
	ns := rec.Spec.Namespace
	if err := r.deletePlacements(ctx, ns, rec.Name, func(p *sandboxv1alpha1.SwiftArtifactPlacement) bool {
		return slices.Contains(pools, p.Spec.PoolRef.Name) && p.Name == compiledcache.PlacementName(rec.Name, p.Spec.PoolRef.Name)
	}); err != nil {
		return err
	}
	id := compiledcache.IdentityOf(rec)
	statuses := make([]cachev1alpha1.PlacementStatus, 0, len(pools))
	for _, pool := range pools {
		st, err := r.ensurePlacement(ctx, rec, pool, r.Delivery.PlacementArtifact(id, artifact))
		if err != nil {
			return err
		}
		statuses = append(statuses, st)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Pool < statuses[j].Pool })
	return r.patchPlacements(ctx, rec, statuses)
}

// ensurePlacement creates or updates the placement of rec on pool and
// returns its state.
func (r *RecordReconciler) ensurePlacement(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache, pool string, a sandboxv1alpha1.PlacementArtifact) (cachev1alpha1.PlacementStatus, error) {
	name := compiledcache.PlacementName(rec.Name, pool)
	st := cachev1alpha1.PlacementStatus{Pool: pool, Name: name}
	want := sandboxv1alpha1.SwiftArtifactPlacementSpec{
		PoolRef:   corev1.LocalObjectReference{Name: pool},
		Artifacts: []sandboxv1alpha1.PlacementArtifact{a},
	}
	var cur sandboxv1alpha1.SwiftArtifactPlacement
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: rec.Spec.Namespace, Name: name}, &cur)
	switch {
	case apierrors.IsNotFound(err):
		p := &sandboxv1alpha1.SwiftArtifactPlacement{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: rec.Spec.Namespace, Labels: placementLabels(rec.Name)},
			Spec:       want,
		}
		if err := r.Client.Create(ctx, p); err != nil && !apierrors.IsAlreadyExists(err) {
			return st, fmt.Errorf("create SwiftArtifactPlacement %s/%s: %w", p.Namespace, name, err)
		}
		log.FromContext(ctx).Info("created placement", "placement", name, "pool", pool)
		st.Reason = "Preparing"
		return st, nil
	case err != nil:
		return st, fmt.Errorf("read SwiftArtifactPlacement %s/%s: %w", rec.Spec.Namespace, name, err)
	}
	if !ownPlacement(&cur, rec.Name) {
		// Never take over an object the cache manager did not create.
		st.Reason = ReasonPlacementConflict
		st.Message = fmt.Sprintf("SwiftArtifactPlacement %s in namespace %s was not created by the cache manager; remove it to let the cache manager place the cache on pool %s", name, rec.Spec.Namespace, pool)
		return st, nil
	}
	if !cur.DeletionTimestamp.IsZero() {
		st.Reason = "Preparing"
		return st, nil
	}
	if !apiequality.Semantic.DeepEqual(cur.Spec, want) {
		cur.Spec = want
		if err := r.Client.Update(ctx, &cur); err != nil {
			return st, fmt.Errorf("update SwiftArtifactPlacement %s/%s: %w", cur.Namespace, name, err)
		}
		log.FromContext(ctx).Info("updated placement", "placement", name, "pool", pool)
		st.Reason = "Preparing"
		return st, nil
	}
	st.PreparedNodes, st.TargetNodes = cur.Status.PreparedNodes, cur.Status.TargetNodes
	c := apimeta.FindStatusCondition(cur.Status.Conditions, sandboxv1alpha1.PlacementConditionPrepared)
	switch {
	case c == nil || cur.Status.ObservedGeneration < cur.Generation || c.ObservedGeneration < cur.Generation:
		// KubeSwift has not acted on this generation yet.
		st.Reason = "Preparing"
	default:
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
