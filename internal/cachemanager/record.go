package cachemanager

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
)

// RecordReconciler drives one SpinCompiledCache record and keeps its
// conditions.
type RecordReconciler struct {
	Client   client.Client
	Settings compiledcache.Settings
	Now      func() time.Time
}

// SetupWithManager registers the reconciler.
func (r *RecordReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("compiledcache-record").
		For(&cachev1alpha1.SpinCompiledCache{}).
		Complete(r)
}

// Reconcile updates one record.
func (r *RecordReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rec cachev1alpha1.SpinCompiledCache
	if err := r.Client.Get(ctx, req.NamespacedName, &rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rec.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx).WithValues("record", rec.Name, "namespace", rec.Spec.Namespace)
	ctx = log.IntoContext(ctx, logger)
	err := r.writeConditions(ctx, &rec)
	if apierrors.IsConflict(err) {
		// Another writer (the namespace reconciler or the signer) changed
		// the record; retry on fresh data.
		return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	}
	return ctrl.Result{}, err
}

// writeConditions recomputes the record's conditions and patches them when
// they changed. Only the conditions and observedGeneration are sent, so the
// fields other writers own (consumers, signature) are untouched.
func (r *RecordReconciler) writeConditions(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache) error {
	conds := append([]metav1.Condition(nil), rec.Status.Conditions...)
	for _, c := range compiledcache.RecordConditions(rec, r.Now()) {
		apimeta.SetStatusCondition(&conds, c)
	}
	if apiequality.Semantic.DeepEqual(conds, rec.Status.Conditions) && rec.Status.ObservedGeneration == rec.Generation {
		return nil
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": rec.ResourceVersion},
		"status":   map[string]any{"conditions": conds, "observedGeneration": rec.Generation},
	})
	if err != nil {
		return err
	}
	if err := r.Client.Status().Patch(ctx, rec, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("patch SpinCompiledCache %s conditions: %w", rec.Name, err)
	}
	rec.Status.Conditions = conds
	return nil
}
