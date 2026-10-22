package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
)

// Compiled-cache Event reasons. They are API: do not rename them.
const (
	EventCompiledCacheRequested       = "CompiledCacheRequested"
	EventCompiledCacheReady           = "CompiledCacheReady"
	EventCompiledCacheFailed          = "CompiledCacheFailed"
	EventCompiledCacheSkipped         = "CompiledCacheSkipped"
	EventCompiledCachePlacementFailed = "CompiledCachePlacementFailed"
)

// cacheView is what a reconcile decided about a SpinApp's compiled cache.
type cacheView struct {
	// known is false when the reconcile did not get far enough to decide;
	// the SpinApp's CompiledCache condition is then left as it is.
	known bool
	// cond is the CompiledCache condition, nil when the SpinApp does not ask
	// for a compiled cache (the condition is removed).
	cond *metav1.Condition
	// artifact is the signed artifact new replicas mount, "" for none.
	artifact string
	id       *compiledcache.Identity
}

// compiledCache decides the compiled cache of a SpinApp.
func (r *SpinAppReconciler) compiledCache(ctx context.Context, app *spinv1alpha1.SpinApp, p *executor.Profile, f capabilities.Sandbox) cacheView {
	if !compiledcache.Requested(app, p) {
		return cacheView{known: true}
	}
	var skip *compiledcache.Skip
	var id *compiledcache.Identity
	switch {
	case !r.CompiledCache.Configured() || !r.CompiledCachesServed:
		skip = &compiledcache.Skip{Reason: compiledcache.ReasonNotConfigured,
			Message: "executor " + p.ExecutorName + " enables " + executor.AnnCompiledCache + ", but the platform has not configured compiled caches (chart value compiledCache.enabled)"}
	case !f.CompiledCache():
		skip = &compiledcache.Skip{Reason: compiledcache.ReasonUnsupported,
			Message: "the installed KubeSwift has no optional, platform-verified artifacts or sandbox outputs; compiled caches need KubeSwift v0.19.1 or later"}
	default:
		var ns corev1.Namespace
		if err := r.Client.Get(ctx, types.NamespacedName{Name: app.Namespace}, &ns); err != nil {
			log.FromContext(ctx).Info("cannot read the SpinApp's namespace; continuing without a compiled cache", "error", err.Error())
			return cacheView{}
		}
		if !compiledcache.NamespaceEnabledFor(ns.Labels) {
			skip = &compiledcache.Skip{Reason: compiledcache.ReasonNamespaceNotEnabled,
				Message: "namespace " + app.Namespace + " is not enabled for compiled caches; an administrator sets the label " + compiledcache.NamespaceLabel + "=" + compiledcache.NamespaceEnabled + " (see docs/compiled-cache.md)"}
			break
		}
		id, skip = compiledcache.Derive(app, p, r.CompiledCache)
	}
	if skip != nil {
		c := compiledcache.AppCondition(skip, nil)
		return cacheView{known: true, cond: &c}
	}
	var rec cachev1alpha1.SpinCompiledCache
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.CompiledCache.Namespace, Name: id.RecordName()}, &rec)
	switch {
	case apierrors.IsNotFound(err):
		c := compiledcache.AppCondition(nil, nil)
		return cacheView{known: true, cond: &c, id: id}
	case err != nil:
		// The cache is an optimization: a failed read never holds up the
		// SpinApp. New replicas start without a cache and the condition
		// stays as it was.
		log.FromContext(ctx).Info("cannot read the SpinApp's compiled cache record; continuing without it", "record", id.RecordName(), "error", err.Error())
		return cacheView{id: id}
	}
	if !id.Matches(&rec) {
		// A record under the name of this identity that describes another
		// one was not written by the cache manager; never use it.
		log.FromContext(ctx).Info("ignoring a SpinCompiledCache whose spec does not match its name", "record", rec.Name)
		c := compiledcache.AppCondition(nil, nil)
		return cacheView{known: true, cond: &c, id: id}
	}
	c := compiledcache.AppCondition(nil, &rec)
	return cacheView{known: true, cond: &c, id: id, artifact: compiledcache.TrustedArtifact(&rec)}
}

// applyCache sets or removes the CompiledCache condition, and emits an Event
// when its reason changes.
func (r *SpinAppReconciler) applyCache(app *spinv1alpha1.SpinApp, prev []metav1.Condition, cv cacheView) {
	if !cv.known {
		return
	}
	if cv.cond == nil {
		apimeta.RemoveStatusCondition(&app.Status.Conditions, compiledcache.TypeCompiledCache)
		return
	}
	cond := *cv.cond
	cond.ObservedGeneration = app.Generation
	old := apimeta.FindStatusCondition(prev, compiledcache.TypeCompiledCache)
	apimeta.SetStatusCondition(&app.Status.Conditions, cond)
	if old != nil && old.Reason == cond.Reason {
		return
	}
	switch cond.Reason {
	case compiledcache.ReasonRequested:
		r.event(app, corev1.EventTypeNormal, EventCompiledCacheRequested, "CompiledCache", "%s", cond.Message)
	case compiledcache.ReasonReady:
		r.event(app, corev1.EventTypeNormal, EventCompiledCacheReady, "CompiledCache", "%s", cond.Message)
	case compiledcache.ReasonCompileFailed, compiledcache.ReasonSignFailed:
		r.event(app, corev1.EventTypeWarning, EventCompiledCacheFailed, "CompiledCache", "%s", cond.Message)
	case compiledcache.ReasonPlacementFailed:
		r.event(app, corev1.EventTypeWarning, EventCompiledCachePlacementFailed, "CompiledCache", "%s", cond.Message)
	case compiledcache.ReasonNotConfigured, compiledcache.ReasonImageNotPinned, compiledcache.ReasonRuntimeNotPinned, compiledcache.ReasonUnsupported,
		compiledcache.ReasonRuntimeNotAllowed, compiledcache.ReasonNamespaceNotEnabled:
		r.event(app, corev1.EventTypeNormal, EventCompiledCacheSkipped, "CompiledCache", "%s", cond.Message)
	}
}

// appsForNamespace enqueues every SpinApp of a namespace whose labels
// changed: the compiled-cache label decides whether they get a cache.
func (r *SpinAppReconciler) appsForNamespace(ctx context.Context, o client.Object) []reconcile.Request {
	var apps spinv1alpha1.SpinAppList
	if err := r.Client.List(ctx, &apps, client.InNamespace(o.GetName())); err != nil {
		log.FromContext(ctx).Error(err, "list SpinApps for namespace", "namespace", o.GetName())
		return nil
	}
	return requestsFor(apps.Items)
}

// appsForRecord enqueues every SpinApp of a record's namespace. The
// consumer list is bounded, so it is not used to find the SpinApps; a
// SpinApp that does not use the record reconciles without effect.
func (r *SpinAppReconciler) appsForRecord(ctx context.Context, o client.Object) []reconcile.Request {
	rec, ok := o.(*cachev1alpha1.SpinCompiledCache)
	if !ok || rec.Namespace != r.CompiledCache.Namespace || rec.Spec.Namespace == "" {
		return nil
	}
	var apps spinv1alpha1.SpinAppList
	if err := r.Client.List(ctx, &apps, client.InNamespace(rec.Spec.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "list SpinApps for compiled cache record", "record", rec.Name)
		return nil
	}
	return requestsFor(apps.Items)
}
