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
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/compatibility"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/localapp"
	"github.com/kubeswift-io/kubeswift-spin/internal/metrics"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

// Local application Event reasons (API).
const (
	EventLocalApplicationReady   = "LocalApplicationReady"
	EventLocalApplicationSkipped = "LocalApplicationSkipped"
	EventLocalApplicationFailed  = "LocalApplicationFailed"
)

// placementBlockedAfter is how long an expected application placement may
// be missing before the SpinApp reports PlacementBlocked.
const placementBlockedAfter = 2 * time.Minute

// localView is what a reconcile decided about a SpinApp's local
// application. A nil view (paths that stop early) leaves the condition as
// it is.
type localView struct {
	// cond is the LocalApplication condition; nil removes it (the SpinApp
	// does not ask).
	cond *metav1.Condition
	// key is the SpinApp's placement key when it is eligible.
	key *localapp.Key
	// attach is true when new warm replicas get the artifact.
	attach bool
	held   bool
}

func localCond(reason, msg string, status metav1.ConditionStatus) *metav1.Condition {
	if len(msg) > 1024 {
		msg = msg[:1024]
	}
	return &metav1.Condition{Type: localapp.TypeLocalApplication, Status: status, Reason: reason, Message: msg}
}

// localApplication decides a SpinApp's local application (ADR 0015).
func (r *SpinAppReconciler) localApplication(ctx context.Context, app *spinv1alpha1.SpinApp, p *executor.Profile, f capabilities.Sandbox) *localView {
	if !localapp.Requested(app, p) {
		return &localView{}
	}
	enabled := false
	if r.LocalApplication.Enabled {
		var ns corev1.Namespace
		if err := r.Client.Get(ctx, types.NamespacedName{Name: app.Namespace}, &ns); err != nil {
			log.FromContext(ctx).Info("cannot read the SpinApp's namespace; continuing without a local application", "error", err.Error())
			return nil
		}
		enabled = localapp.NamespaceEnabledFor(ns.Labels)
	}
	k, skip := localapp.Decide(localapp.Input{App: app, Profile: p, Settings: r.LocalApplication, NamespaceEnabled: enabled,
		Supported: f.LocalApplication() && r.PlacementsServed})
	if skip != nil {
		return &localView{cond: localCond(skip.Reason, skip.Message, metav1.ConditionFalse)}
	}
	v := &localView{key: k}
	appKey := client.ObjectKeyFromObject(app).String()
	if h, held := r.localHold(appKey); held {
		v.held = true
		v.cond = localCond(localapp.ReasonDeliveryFailed, fmt.Sprintf(
			"replica %s failed while it carried the local application (%s); new replicas start from the registry until %s",
			h.sandbox, h.reason, h.until.UTC().Format(time.RFC3339)), metav1.ConditionFalse)
		return v
	}
	var pl sandboxv1alpha1.SwiftArtifactPlacement
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: k.Namespace, Name: k.PlacementName()}, &pl)
	switch {
	case err == nil && localapp.Prepared(&pl, *k):
		v.attach = true
		v.cond = localCond(localapp.ReasonAttached, "new warm replicas mount the application artifact, prepared on the pool's nodes; the replica's console says whether Spin used it",
			metav1.ConditionTrue)
	case err == nil && localapp.Owned(&pl, *k):
		if reason := placementFailure(&pl); reason != "" {
			v.cond = localCond(localapp.ReasonPlacementFailed, fmt.Sprintf(
				"KubeSwift could not prepare the application artifact on the pool's nodes (%s); new replicas start from the registry", reason), metav1.ConditionFalse)
		} else {
			v.cond = localCond(localapp.ReasonPlacing, "the application artifact is being prepared on the pool's nodes; new replicas start from the registry until then", metav1.ConditionFalse)
		}
	case err == nil:
		v.cond = localCond(localapp.ReasonPlacementBlocked, "an object not owned by kubeswift-spin has the name of the application placement; new replicas start from the registry", metav1.ConditionFalse)
	case apierrors.IsNotFound(err):
		v.cond = r.missingPlacement(ctx, app, *k)
	default:
		log.FromContext(ctx).Info("cannot read the application placement; continuing without it", "error", err.Error())
		return nil
	}
	return v
}

// missingPlacement reports a placement the cache manager has not created:
// beyond a limit, being created, or blocked after placementBlockedAfter.
func (r *SpinAppReconciler) missingPlacement(ctx context.Context, app *spinv1alpha1.SpinApp, k localapp.Key) *metav1.Condition {
	if r.limited(ctx, k) {
		return localCond(localapp.ReasonPlacementLimit, fmt.Sprintf(
			"namespace %s is at its limit of application placements (chart values localApplication.maxPlacementsPerNamespace and maxNewPerHour); new replicas start from the registry",
			app.Namespace), metav1.ConditionFalse)
	}
	if old := apimeta.FindStatusCondition(app.Status.Conditions, localapp.TypeLocalApplication); old != nil &&
		(old.Reason == localapp.ReasonPlacing || old.Reason == localapp.ReasonPlacementBlocked) && r.Now().Sub(old.LastTransitionTime.Time) > placementBlockedAfter {
		return localCond(localapp.ReasonPlacementBlocked,
			"the cache manager has not created the application placement; check that it runs and its Events (a quota or admission policy may refuse placements); new replicas start from the registry",
			metav1.ConditionFalse)
	}
	return localCond(localapp.ReasonPlacing, "the application artifact is being prepared on the pool's nodes; new replicas start from the registry until then", metav1.ConditionFalse)
}

// limited computes the namespace's selection as the cache manager does and
// reports whether k is beyond a limit.
func (r *SpinAppReconciler) limited(ctx context.Context, k localapp.Key) bool {
	var apps spinv1alpha1.SpinAppList
	if err := r.Client.List(ctx, &apps, client.InNamespace(k.Namespace)); err != nil {
		return false
	}
	features := capabilities.Sandbox{}
	if r.Detector != nil {
		features, _ = r.Detector.Sandbox(ctx)
	}
	opts := r.Options
	opts.Features = features
	profiles := map[string]*executor.Profile{}
	var wants []localapp.Want
	for i := range apps.Items {
		a := &apps.Items[i]
		if !a.DeletionTimestamp.IsZero() || a.Spec.Executor == "" || a.Spec.Replicas == 0 {
			continue
		}
		p, seen := profiles[a.Spec.Executor]
		if !seen {
			var e spinv1alpha1.SpinAppExecutor
			if err := r.Client.Get(ctx, types.NamespacedName{Namespace: k.Namespace, Name: a.Spec.Executor}, &e); err == nil && executor.IsManaged(&e) {
				p, _ = executor.Parse(&e, r.Defaults)
			}
			profiles[a.Spec.Executor] = p
		}
		if p == nil || compatibility.Blocking(compatibility.Analyze(a, p, opts)) {
			continue
		}
		if wk, skip := localapp.Decide(localapp.Input{App: a, Profile: p, Settings: r.LocalApplication, NamespaceEnabled: true,
			Supported: features.LocalApplication() && r.PlacementsServed}); wk != nil && skip == nil {
			wants = append(wants, localapp.Want{Key: *wk, Created: a.CreationTimestamp.Time, App: a.Name})
		}
	}
	var pls sandboxv1alpha1.SwiftArtifactPlacementList
	if err := r.Client.List(ctx, &pls, client.InNamespace(k.Namespace)); err != nil {
		return false
	}
	var existing []localapp.Existing
	for i := range pls.Items {
		pl := &pls.Items[i]
		if len(pl.Spec.Artifacts) != 1 {
			continue
		}
		ek := localapp.Key{Namespace: pl.Namespace, Pool: pl.Spec.PoolRef.Name, Ref: pl.Spec.Artifacts[0].Ref}
		if pl.Spec.Artifacts[0].PullSecretRef != nil {
			ek.PullSecret = pl.Spec.Artifacts[0].PullSecretRef.Name
		}
		if !localapp.Owned(pl, ek) {
			continue
		}
		e := localapp.Existing{Key: ek, Created: pl.CreationTimestamp.Time}
		if s, ok := pl.Annotations[localapp.AnnUnwantedSince]; ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				e.UnwantedSince = &t
			}
		}
		existing = append(existing, e)
	}
	for _, l := range localapp.Select(wants, existing, r.Now(), r.LocalApplication).Limited {
		if l == k {
			return true
		}
	}
	return false
}

// placementFailure returns a failure reason from a placement's Prepared
// condition, or "" while it is still preparing.
func placementFailure(pl *sandboxv1alpha1.SwiftArtifactPlacement) string {
	c := apimeta.FindStatusCondition(pl.Status.Conditions, sandboxv1alpha1.PlacementConditionPrepared)
	if c == nil || c.Status == metav1.ConditionTrue {
		return ""
	}
	switch c.Reason {
	case "", "Preparing", "Progressing", "Pending", "NoTargetNodes", "Resolving":
		return ""
	}
	return metrics.Reason(c.Reason)
}

// applyLocal sets or removes the LocalApplication condition and emits an
// Event when its reason changes. It never touches Available or Progressing.
func (r *SpinAppReconciler) applyLocal(app *spinv1alpha1.SpinApp, prev []metav1.Condition, v *localView) {
	if v == nil {
		return
	}
	if v.cond == nil {
		apimeta.RemoveStatusCondition(&app.Status.Conditions, localapp.TypeLocalApplication)
		return
	}
	cond := *v.cond
	cond.ObservedGeneration = app.Generation
	old := apimeta.FindStatusCondition(prev, localapp.TypeLocalApplication)
	apimeta.SetStatusCondition(&app.Status.Conditions, cond)
	if old != nil && old.Reason == cond.Reason {
		return
	}
	switch cond.Reason {
	case localapp.ReasonAttached:
		r.event(app, corev1.EventTypeNormal, EventLocalApplicationReady, "LocalApplication", "%s", cond.Message)
	case localapp.ReasonPlacementFailed, localapp.ReasonPlacementBlocked, localapp.ReasonDeliveryFailed:
		r.event(app, corev1.EventTypeWarning, EventLocalApplicationFailed, "LocalApplication", "%s", cond.Message)
	case localapp.ReasonPlacing:
	default:
		r.event(app, corev1.EventTypeNormal, EventLocalApplicationSkipped, "LocalApplication", "%s", cond.Message)
	}
}

// decorateLocal adds the local application to a new replica and returns the
// lookup result to count ("" when the SpinApp does not ask). It runs after
// the revision fingerprint: the local application never rolls replicas.
func (r *SpinAppReconciler) decorateLocal(sb *sandboxv1alpha1.SwiftSandbox, v *localView) string {
	switch {
	case v == nil || (v.cond == nil && v.key == nil):
		return ""
	case v.key == nil:
		return "ineligible"
	case v.held:
		return "held"
	case sb.Spec.PoolRef == nil:
		return "cold"
	case !v.attach:
		return "unplaced"
	}
	localapp.Decorate(sb, *v.key)
	return "attached"
}

// localHoldPeriod is how long a SpinApp's new replicas start from the
// registry after a replica that carried the local application failed.
const localHoldPeriod = 30 * time.Minute

type localHoldEntry struct {
	sandbox, reason string
	until           time.Time
}

func (r *SpinAppReconciler) localHold(key string) (localHoldEntry, bool) {
	r.attachMu.Lock()
	defer r.attachMu.Unlock()
	h, ok := r.localHolds[key]
	if !ok || !r.Now().Before(h.until) {
		return localHoldEntry{}, false
	}
	return h, true
}

// localHoldRecheck returns how long until a SpinApp's local hold ends, plus
// a second, or zero.
func (r *SpinAppReconciler) localHoldRecheck(key string) time.Duration {
	h, ok := r.localHold(key)
	if !ok {
		return 0
	}
	return h.until.Sub(r.Now()) + time.Second
}

// localFailure reports why a replica that carried the local application
// failed in a way the local application may have caused, or "": an
// artifact failure in KubeSwift, or a failure before the replica was ever
// seen ready (a lock Spin rejects after the entrypoint started it). A
// runtime image KubeSwift could not resolve, pull or verify (Image*) fails
// the replica before the guest runs, whatever it carries.
func (r *SpinAppReconciler) localFailure(sb *sandboxv1alpha1.SwiftSandbox) string {
	carried := false
	for _, a := range sb.Spec.Artifacts {
		if a.Name == localapp.ArtifactName {
			carried = true
		}
	}
	if !carried {
		return ""
	}
	if wr := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionWorkloadReady); wr != nil && wr.Status == metav1.ConditionTrue {
		r.everReady[sb.UID] = true
	}
	if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed {
		return ""
	}
	if c := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning); c != nil {
		switch {
		case strings.HasPrefix(c.Reason, "Artifact"):
			return metrics.Reason(c.Reason)
		case c.Reason == "KernelUnsupported" && strings.Contains(c.Message, "artifact"):
			return metrics.Reason(c.Reason)
		case strings.HasPrefix(c.Reason, "Image"):
			return ""
		}
	}
	if !r.everReady[sb.UID] {
		return "FailedBeforeReady"
	}
	return ""
}

// countLocal starts holds and counts, once per replica, what KubeSwift did
// with the application artifact. Called with the replicas of one SpinApp.
func (r *SpinAppReconciler) countLocal(key string, owned []sandboxv1alpha1.SwiftSandbox) {
	r.attachMu.Lock()
	defer r.attachMu.Unlock()
	if r.localHolds == nil {
		r.localHolds = map[string]localHoldEntry{}
		r.localCounted = map[types.UID]string{}
		r.everReady = map[types.UID]bool{}
	}
	if r.startedAt.IsZero() {
		r.startedAt = r.Now()
	}
	live := map[types.UID]bool{}
	for i := range owned {
		sb := &owned[i]
		live[sb.UID] = true
		if reason := r.localFailure(sb); reason != "" {
			if h, ok := r.localHolds[key]; !ok || !r.Now().Before(h.until) {
				r.localHolds[key] = localHoldEntry{sandbox: sb.Name, reason: reason, until: r.Now().Add(r.localHoldDuration())}
			}
		}
		if _, done := r.localCounted[sb.UID]; done || sb.CreationTimestamp.Time.Before(r.startedAt) {
			continue
		}
		for _, a := range sb.Status.Artifacts {
			if a.Name != localapp.ArtifactName {
				continue
			}
			switch a.State {
			case sandboxv1alpha1.SandboxArtifactMounted:
				metrics.LocalApplicationAttachments.WithLabelValues("mounted", metrics.Reason(a.Reason)).Inc()
			case sandboxv1alpha1.SandboxArtifactSkipped, sandboxv1alpha1.SandboxArtifactFailed:
				metrics.LocalApplicationAttachments.WithLabelValues("skipped", metrics.Reason(a.Reason)).Inc()
			default:
				continue
			}
			r.localCounted[sb.UID] = key
		}
	}
	for uid, k := range r.localCounted {
		if k == key && !live[uid] {
			delete(r.localCounted, uid)
			delete(r.everReady, uid)
		}
	}
}

func (r *SpinAppReconciler) localHoldDuration() time.Duration {
	if r.LocalHoldPeriod > 0 {
		return r.LocalHoldPeriod
	}
	return localHoldPeriod
}

// forgetLocal drops what countLocal remembers of a SpinApp.
func (r *SpinAppReconciler) forgetLocal(key string) {
	r.attachMu.Lock()
	defer r.attachMu.Unlock()
	for uid, k := range r.localCounted {
		if k == key {
			delete(r.localCounted, uid)
			delete(r.everReady, uid)
		}
	}
	delete(r.localHolds, key)
}

// appsForPlacement enqueues every SpinApp of an application placement's
// namespace.
func (r *SpinAppReconciler) appsForPlacement(ctx context.Context, o client.Object) []reconcile.Request {
	var apps spinv1alpha1.SpinAppList
	if err := r.Client.List(ctx, &apps, client.InNamespace(o.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "list SpinApps for application placement", "namespace", o.GetNamespace())
		return nil
	}
	return requestsFor(apps.Items)
}
