package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/metrics"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
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
	// preparedPools are the warm pools whose placement of the artifact is
	// Prepared on every node.
	preparedPools map[string]bool
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
			Message: "the installed KubeSwift has no optional, platform-verified artifacts or sandbox outputs; compiled caches need KubeSwift v0.19.3 or later"}
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
		if skip == nil && !r.CacheDelivery.Serves(p) {
			id, skip = nil, compiledcache.NoWarmPool(p.ExecutorName)
		}
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
	artifact := compiledcache.TrustedArtifact(&rec)
	if h, held := r.cacheHold(client.ObjectKeyFromObject(app).String(), artifact); held {
		// A replica that carried this cache failed because of it: new
		// replicas start without it for a while, so the cache cannot keep a
		// SpinApp from scaling or recovering.
		msg := fmt.Sprintf("replica %s failed while KubeSwift delivered the compiled cache (%s); new replicas start without it until %s. "+
			"Check the KubeSwift sandbox's status and Events: a failure here is a platform problem, such as a sandbox-materialize image of another KubeSwift version or a kernel that cannot mount artifacts",
			h.sandbox, h.reason, h.until.UTC().Format(time.RFC3339))
		c = metav1.Condition{Type: compiledcache.TypeCompiledCache, Status: metav1.ConditionFalse, Reason: compiledcache.ReasonDeliveryFailed, Message: msg}
		return cacheView{known: true, cond: &c, id: id}
	}
	prepared := map[string]bool{}
	for _, p := range rec.Status.Placements {
		if p.Prepared {
			prepared[p.Pool] = true
		}
	}
	return cacheView{known: true, cond: &c, id: id, artifact: artifact, preparedPools: prepared}
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
	case compiledcache.ReasonCompileFailed, compiledcache.ReasonSignFailed, compiledcache.ReasonDeliveryFailed:
		r.event(app, corev1.EventTypeWarning, EventCompiledCacheFailed, "CompiledCache", "%s", cond.Message)
	case compiledcache.ReasonPlacementFailed:
		r.event(app, corev1.EventTypeWarning, EventCompiledCachePlacementFailed, "CompiledCache", "%s", cond.Message)
	case compiledcache.ReasonNotConfigured, compiledcache.ReasonImageNotPinned, compiledcache.ReasonRuntimeNotPinned, compiledcache.ReasonUnsupported,
		compiledcache.ReasonRuntimeNotAllowed, compiledcache.ReasonNamespaceNotEnabled, compiledcache.ReasonNoWarmPool:
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

// decorateCache adds the SpinApp's signed compiled cache to a new replica
// and returns the lookup result to count once the replica is created ("" for
// none). It runs after the revision fingerprint is computed: the cache is
// never part of a replica's revision.
func (r *SpinAppReconciler) decorateCache(sb *sandboxv1alpha1.SwiftSandbox, cv cacheView) string {
	switch {
	case cv.cond == nil && cv.id == nil:
		// The SpinApp does not ask for a cache, or the reconcile could not
		// tell; nothing to count.
		return ""
	case cv.id == nil:
		return "skipped"
	case cv.artifact == "":
		return "miss"
	case !r.CacheDelivery.Delivers(sb):
		return "cold"
	case sb.Spec.PoolRef != nil && !cv.preparedPools[sb.Spec.PoolRef.Name]:
		// An optimization, not a correctness rule: a warm replica gets the
		// cache once its pool's placement has prepared every node, where a
		// checkout finds the entry. KubeSwift skips an optional artifact a
		// node does not hold, and the replica starts and becomes ready
		// without it, but it would pay for a fetch it cannot use.
		return "unplaced"
	}
	r.CacheDelivery.Decorate(sb, *cv.id, cv.artifact)
	return "hit"
}

// cacheHoldPeriod is how long a SpinApp's new replicas go without a cache
// after a replica failed because of it. KubeSwift skips an optional
// artifact it cannot deliver, so a replica that merely ran without its cache
// never starts a hold; one that failed did so because the platform could not
// deliver artifacts at all (an artifact init container that failed, a kernel
// without artifact support), and every new replica with the cache would
// fail the same way.
const cacheHoldPeriod = 30 * time.Minute

// holdPeriod is cacheHoldPeriod, or CacheHoldPeriod when set (tests).
func (r *SpinAppReconciler) holdPeriod() time.Duration {
	if r.CacheHoldPeriod > 0 {
		return r.CacheHoldPeriod
	}
	return cacheHoldPeriod
}

type cacheHold struct {
	artifact, sandbox, reason string
	until                     time.Time
}

// cacheFailure reports why a replica that carried the compiled cache failed
// because of an artifact, or "". Only a failed replica qualifies: one whose
// cache KubeSwift skipped is a healthy replica without a cache. KubeSwift
// fails a sandbox for an optional artifact only when its delivery itself
// fails (an Artifact* reason) or when the kernel cannot mount artifacts;
// KernelUnsupported for anything else (secret files, outputs) is not the
// cache's.
func cacheFailure(sb *sandboxv1alpha1.SwiftSandbox) string {
	carried := false
	for _, a := range sb.Spec.Artifacts {
		if a.Name == compiledcache.ArtifactName {
			carried = true
		}
	}
	if !carried || sb.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed {
		return ""
	}
	c := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	switch {
	case c == nil:
		return ""
	case strings.HasPrefix(c.Reason, "Artifact"):
		return metrics.Reason(c.Reason)
	case c.Reason == "KernelUnsupported" && strings.Contains(c.Message, "artifact"):
		return metrics.Reason(c.Reason)
	}
	return ""
}

// holdRecheck returns how long until a SpinApp's active hold ends, plus a
// second, or zero without one: the reconcile looks again then, whichever
// path it takes.
func (r *SpinAppReconciler) holdRecheck(key string) time.Duration {
	r.attachMu.Lock()
	defer r.attachMu.Unlock()
	h, ok := r.holds[key]
	if !ok || !r.Now().Before(h.until) {
		return 0
	}
	return h.until.Sub(r.Now()) + time.Second
}

// cacheHold returns the hold on artifact for a SpinApp, if one is active.
func (r *SpinAppReconciler) cacheHold(key, artifact string) (cacheHold, bool) {
	r.attachMu.Lock()
	defer r.attachMu.Unlock()
	h, ok := r.holds[key]
	if !ok || artifact == "" || h.artifact != artifact || !r.Now().Before(h.until) {
		return cacheHold{}, false
	}
	return h, true
}

// countAttachments counts, once per replica, what KubeSwift did with the
// compiled cache of a replica that carried one: mounted (reason Cached or
// Fetched) or skipped (and why). Replicas created before this process
// started are not counted, so a restart does not count them again. A
// replica that failed because of the cache starts a hold.
func (r *SpinAppReconciler) countAttachments(key string, owned []sandboxv1alpha1.SwiftSandbox) {
	r.attachMu.Lock()
	defer r.attachMu.Unlock()
	if r.attached == nil {
		r.attached = map[types.UID]string{}
		r.holds = map[string]cacheHold{}
	}
	if r.startedAt.IsZero() {
		// Only the leader reconciles: count replicas from the moment this
		// process started reconciling.
		r.startedAt = r.Now()
	}
	for i := range owned {
		sb := &owned[i]
		if reason := cacheFailure(sb); reason != "" {
			for _, a := range sb.Spec.Artifacts {
				if a.Name != compiledcache.ArtifactName {
					continue
				}
				if h, ok := r.holds[key]; !ok || h.artifact != a.Ref || !r.Now().Before(h.until) {
					r.holds[key] = cacheHold{artifact: a.Ref, sandbox: sb.Name, reason: reason, until: r.Now().Add(r.holdPeriod())}
				}
			}
		}
	}
	live := make(map[types.UID]bool, len(owned))
	for i := range owned {
		sb := &owned[i]
		live[sb.UID] = true
		if _, done := r.attached[sb.UID]; done || sb.CreationTimestamp.Time.Before(r.startedAt) {
			continue
		}
		for _, a := range sb.Status.Artifacts {
			if a.Name != compiledcache.ArtifactName {
				continue
			}
			switch a.State {
			case sandboxv1alpha1.SandboxArtifactMounted:
				metrics.CompiledCacheAttachments.WithLabelValues("mounted", metrics.Reason(a.Reason)).Inc()
			case sandboxv1alpha1.SandboxArtifactSkipped, sandboxv1alpha1.SandboxArtifactFailed:
				metrics.CompiledCacheAttachments.WithLabelValues("skipped", metrics.Reason(a.Reason)).Inc()
			default:
				continue
			}
			r.attached[sb.UID] = key
		}
	}
	for uid, k := range r.attached {
		if k == key && !live[uid] {
			delete(r.attached, uid)
		}
	}
}

// forgetAttachments drops what countAttachments remembers of a SpinApp.
func (r *SpinAppReconciler) forgetAttachments(key string) {
	r.attachMu.Lock()
	defer r.attachMu.Unlock()
	for uid, k := range r.attached {
		if k == key {
			delete(r.attached, uid)
		}
	}
	delete(r.holds, key)
}

// recordViewChanged lets a record update through only when something a
// SpinApp reads changed: the spec, the conditions or the trusted artifact.
// Consumer lists and per-node placement progress change often and would
// otherwise reconcile every SpinApp of the namespace.
var recordViewChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, ok1 := e.ObjectOld.(*cachev1alpha1.SpinCompiledCache)
		n, ok2 := e.ObjectNew.(*cachev1alpha1.SpinCompiledCache)
		if !ok1 || !ok2 {
			return true
		}
		return o.Spec != n.Spec || compiledcache.TrustedArtifact(o) != compiledcache.TrustedArtifact(n) ||
			!conditionsEqual(o.Status.Conditions, n.Status.Conditions)
	},
}

func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x := apimeta.FindStatusCondition(b, a[i].Type)
		if x == nil || x.Status != a[i].Status || x.Reason != a[i].Reason || x.Message != a[i].Message {
			return false
		}
	}
	return true
}
