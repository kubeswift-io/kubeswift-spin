// Package controller reconciles SpinApps whose executor is managed by
// kubeswift-spin into KubeSwift SwiftSandboxes.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/compatibility"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/metrics"
	"github.com/kubeswift-io/kubeswift-spin/internal/rollout"
	"github.com/kubeswift-io/kubeswift-spin/internal/status"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

// ExecutorIndex indexes SpinApps by spec.executor.
const ExecutorIndex = "spec.executor"

// Event reasons. Event notes never contain variable values, option values or
// Secret contents.
const (
	EventSandboxCreated     = "SandboxCreated"
	EventSandboxDeleted     = "SandboxDeleted"
	EventSandboxFailed      = "SandboxFailed"
	EventUnsupported        = "UnsupportedConfiguration"
	EventPartiallySupported = "PartiallySupported"
	EventExecutorInvalid    = "ExecutorInvalid"
	EventExecutorNotFound   = "ExecutorNotFound"
	EventWarmPool           = "WarmPoolIncompatible"
	EventConflict           = "SandboxConflict"
	EventRejected           = "SandboxRejected"
	EventExecutorChanged    = "ExecutorChanged"
)

// leavingRequeueMin is the shortest recheck while a SpinApp's sandbox name is
// held by a terminating sandbox of a deleted SpinApp with the same name.
// Such a sandbox is still owned by a SpinApp of that name, so its updates
// and removal already enqueue this SpinApp; the requeue is a safety net,
// timed for when the sandbox's drain and KubeSwift's stop allowance end and
// at most WaitingRequeue away, so a long drain is not polled.
const leavingRequeueMin = 5 * time.Second

// stopAllowance is KubeSwift's time to stop a microVM after its drain.
const stopAllowance = 15 * time.Second

// SpinAppReconciler realizes SpinApps as SwiftSandboxes.
type SpinAppReconciler struct {
	Client client.Client
	// APIReader reads directly from the API server. It is used to tell a
	// genuine name conflict from the controller's own sandbox that the
	// informer cache has not seen yet.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder

	Defaults executor.Defaults
	Options  compatibility.Options
	Detector capabilities.SandboxDetector
	// PoolsServed is true when the SwiftSandboxPool API is installed.
	PoolsServed bool

	// CompiledCache are the platform's compiled-cache settings; the feature
	// is off unless they are configured. CompiledCachesServed is true when
	// the SpinCompiledCache API is installed.
	CompiledCache        compiledcache.Settings
	CompiledCachesServed bool
	// CacheDelivery says how new replicas mount a signed cache.
	CacheDelivery compiledcache.Delivery

	Metrics *metrics.Tracker
	Backoff *Backoff
	Now     func() time.Time
	// WaitingRequeue is how long to wait before rechecking a SpinApp whose
	// rollout waits on something no watch reports, such as a foreign object
	// occupying a sandbox name. Defaults to 30 seconds.
	WaitingRequeue time.Duration
	// LeavingTimeout is how long a sandbox of a deleted SpinApp with the
	// same name may keep a sandbox name while it terminates before it is
	// reported as SandboxConflict, on top of the drain period in that
	// sandbox's spec. KubeSwift v0.16.3 releases it within its drain, a 15 s
	// stop allowance and a 30 s slack. Defaults to 2 minutes.
	LeavingTimeout time.Duration

	// annSeen remembers, per SpinApp, the spin.kubeswift.io/ annotations of
	// the last reconcile, so an annotation-only edit (no new generation)
	// still reports its findings.
	annMu   sync.Mutex
	annSeen map[string]string

	// attached remembers which replicas' compiled-cache outcome was
	// counted, so each replica is counted once.
	attachMu  sync.Mutex
	attached  map[types.UID]string
	holds     map[string]cacheHold
	startedAt time.Time
}

// SetupWithManager registers the controller, its watches and the field index.
func (r *SpinAppReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Backoff == nil {
		r.Backoff = NewBackoff()
	}
	if r.Metrics == nil {
		r.Metrics = metrics.NewTracker()
	}
	if r.LeavingTimeout == 0 {
		r.LeavingTimeout = 2 * time.Minute
	}
	if r.WaitingRequeue == 0 {
		r.WaitingRequeue = 30 * time.Second
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &spinv1alpha1.SpinApp{}, ExecutorIndex,
		func(o client.Object) []string {
			app := o.(*spinv1alpha1.SpinApp)
			if app.Spec.Executor == "" {
				return nil
			}
			return []string{app.Spec.Executor}
		}); err != nil {
		return fmt.Errorf("index SpinApp spec.executor: %w", err)
	}

	b := ctrl.NewControllerManagedBy(mgr).
		Named("spinapp").
		For(&spinv1alpha1.SpinApp{}, builder.WithPredicates(predicate.Or[client.Object](predicate.GenerationChangedPredicate{}, appAnnotationsChanged))).
		Owns(&sandboxv1alpha1.SwiftSandbox{}).
		Watches(&spinv1alpha1.SpinAppExecutor{}, handler.EnqueueRequestsFromMapFunc(r.appsForExecutor))
	if r.PoolsServed {
		b = b.Watches(&sandboxv1alpha1.SwiftSandboxPool{}, handler.EnqueueRequestsFromMapFunc(r.appsForPool))
	}
	if r.CompiledCache.Configured() && r.CompiledCachesServed {
		b = b.Watches(&cachev1alpha1.SpinCompiledCache{}, handler.EnqueueRequestsFromMapFunc(r.appsForRecord),
			builder.WithPredicates(recordViewChanged)).
			Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.appsForNamespace),
				builder.WithPredicates(predicate.LabelChangedPredicate{}))
	}
	return b.Complete(r)
}

func (r *SpinAppReconciler) appsForExecutor(ctx context.Context, o client.Object) []reconcile.Request {
	var apps spinv1alpha1.SpinAppList
	if err := r.Client.List(ctx, &apps, client.InNamespace(o.GetNamespace()), client.MatchingFields{ExecutorIndex: o.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "list SpinApps for executor", "executor", o.GetName())
		return nil
	}
	return requestsFor(apps.Items)
}

func (r *SpinAppReconciler) appsForPool(ctx context.Context, o client.Object) []reconcile.Request {
	var execs spinv1alpha1.SpinAppExecutorList
	if err := r.Client.List(ctx, &execs, client.InNamespace(o.GetNamespace()),
		client.MatchingLabels{executor.ManagedByLabel: executor.ManagedByValue}); err != nil {
		log.FromContext(ctx).Error(err, "list executors for pool", "pool", o.GetName())
		return nil
	}
	var out []reconcile.Request
	for i := range execs.Items {
		if execs.Items[i].Annotations[executor.AnnSandboxPool] == o.GetName() {
			out = append(out, r.appsForExecutor(ctx, &execs.Items[i])...)
		}
	}
	return out
}

func requestsFor(apps []spinv1alpha1.SpinApp) []reconcile.Request {
	out := make([]reconcile.Request, 0, len(apps))
	for i := range apps {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&apps[i])})
	}
	return out
}

// conflictRequeue is the short delay before retrying after a write conflict.
const conflictRequeue = 200 * time.Millisecond

// Reconcile realizes one SpinApp.
func (r *SpinAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res, err := r.reconcile(ctx, req)
	if apierrors.IsConflict(err) {
		// The object changed since the cache read it, usually because of
		// this controller's own previous write. The status patch carries the
		// resource version precisely to detect this; retry on fresh data
		// without reporting an error.
		log.FromContext(ctx).V(1).Info("conflict, retrying", "error", err.Error())
		metrics.Reconciliations.WithLabelValues("conflict").Inc()
		return ctrl.Result{RequeueAfter: conflictRequeue}, nil
	}
	if err != nil {
		metrics.Reconciliations.WithLabelValues("error").Inc()
		metrics.ReconcileErrors.Inc()
	} else {
		metrics.Reconciliations.WithLabelValues("success").Inc()
	}
	return res, err
}

func (r *SpinAppReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := req.String()

	var app spinv1alpha1.SpinApp
	if err := r.Client.Get(ctx, req.NamespacedName, &app); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(key)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	logger := log.FromContext(ctx).WithValues("spinapp", app.Name, "namespace", app.Namespace, "executor", app.Spec.Executor)
	ctx = log.IntoContext(ctx, logger)
	if !app.DeletionTimestamp.IsZero() {
		// Owner references let the garbage collector remove the sandboxes.
		r.forget(key)
		return ctrl.Result{}, nil
	}

	owned, err := r.ownedSandboxes(ctx, &app)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.countAttachments(key, owned)

	var exec spinv1alpha1.SpinAppExecutor
	var execErr error = apierrors.NewNotFound(spinv1alpha1.GroupVersion.WithResource("spinappexecutors").GroupResource(), app.Spec.Executor)
	if app.Spec.Executor != "" {
		execErr = r.Client.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Spec.Executor}, &exec)
	}
	switch {
	case apierrors.IsNotFound(execErr):
		if len(owned) == 0 {
			// Nothing suggests this SpinApp is ours. The executor watch
			// requeues it if a managed executor with that name appears.
			r.forget(key)
			return ctrl.Result{}, nil
		}
		// The app was ours. Keep its replicas running rather than destroying
		// a workload because its executor is temporarily missing.
		blocker := &status.Blocker{Reason: status.ReasonExecutorNotFound,
			Message: fmt.Sprintf("SpinAppExecutor %q not found in namespace %q; existing sandboxes are left running", app.Spec.Executor, app.Namespace)}
		return ctrl.Result{}, r.finish(ctx, &app, owned, "", blocker, status.Exposure{}, cacheView{}, EventExecutorNotFound)
	case execErr != nil:
		return ctrl.Result{}, execErr
	}

	if !executor.IsManaged(&exec) {
		// The SpinApp now uses an executor that kubeswift-spin does not
		// manage. Remove its sandboxes and leave the status to whoever
		// realizes the new executor.
		if len(owned) > 0 {
			for i := range owned {
				if err := r.deleteSandbox(ctx, &owned[i], "executor_change"); err != nil {
					return ctrl.Result{}, err
				}
			}
			r.event(&app, corev1.EventTypeNormal, EventExecutorChanged, "Cleanup",
				"executor %q is not managed by kubeswift-spin; deleted %d sandboxes", app.Spec.Executor, len(owned))
		}
		r.forget(key)
		return ctrl.Result{}, nil
	}

	features := r.features(ctx)
	exposure := status.Exposure{Detected: features.Exposure()}

	profile, perr := executor.Parse(&exec, r.Defaults)
	if perr == nil {
		perr = profile.CheckFeatures(features)
	}
	if perr != nil {
		if exec.Spec.CreateDeployment {
			// Spin Operator owns the SpinApp status of executors with
			// createDeployment: true and rewrites it with a full update, so
			// writing it here would make two controllers overwrite each
			// other. Report through an Event only; existing sandboxes are
			// left running.
			r.event(&app, corev1.EventTypeWarning, EventExecutorInvalid, "Reconcile", "%s", perr.Error())
			r.forget(key)
			return ctrl.Result{}, nil
		}
		blocker := &status.Blocker{Reason: status.ReasonExecutorInvalid, Message: perr.Error()}
		return ctrl.Result{}, r.finish(ctx, &app, owned, "", blocker, exposure, cacheView{}, EventExecutorInvalid)
	}

	opts := r.Options
	opts.Features = features
	findings := compatibility.Analyze(&app, profile, opts)
	newGeneration := r.isNewGeneration(&app)
	if annChanged := r.annotationsChanged(key, &app); newGeneration || annChanged {
		for _, f := range findings {
			if f.Blocking {
				metrics.UnsupportedConfiguration.WithLabelValues(f.Field).Inc()
			} else {
				r.event(&app, corev1.EventTypeWarning, EventPartiallySupported, "Validate", "%s", f.Message)
			}
		}
	}
	if compatibility.Blocking(findings) {
		blocker := &status.Blocker{Reason: status.ReasonUnsupportedConfiguration, Message: compatibility.Summary(findings)}
		return ctrl.Result{}, r.finish(ctx, &app, owned, "", blocker, exposure, cacheView{}, EventUnsupported)
	}

	cache := r.compiledCache(ctx, &app, profile, features)

	tmpl, err := translate.BuildTemplate(&app, profile, r.Options.Resources, features)
	if err != nil {
		blocker := &status.Blocker{Reason: status.ReasonUnsupportedConfiguration, Message: err.Error()}
		return ctrl.Result{}, r.finish(ctx, &app, owned, "", blocker, exposure, cache, EventUnsupported)
	}

	if profile.SandboxPool != "" {
		if blocker, err := r.checkPool(ctx, &app, profile, tmpl); err != nil {
			return ctrl.Result{}, err
		} else if blocker != nil {
			return ctrl.Result{}, r.finish(ctx, &app, owned, tmpl.Revision, blocker, exposure, cache, EventWarmPool)
		}
	}

	now := r.Now()
	instances := r.observe(&app, owned, tmpl.Revision, now)
	inBackoff, backoffWait := r.Backoff.Pending(key, instances, tmpl.Revision, now)
	retryIn := ""
	if backoffWait > 0 {
		retryIn = backoffWait.Round(time.Second).String()
	}

	plan := rollout.Compute(rollout.Input{
		Replicas:            int(app.Spec.Replicas),
		Revision:            tmpl.Revision,
		Instances:           rolloutView(instances),
		ReadinessObservable: exposure.Usable(),
		InBackoff:           inBackoff,
	})

	for _, d := range plan.Delete {
		sb := findByName(owned, d.Name)
		if sb == nil {
			continue
		}
		if err := r.deleteSandbox(ctx, sb, string(d.Reason)); err != nil {
			return ctrl.Result{}, err
		}
		r.event(&app, corev1.EventTypeNormal, EventSandboxDeleted, "Delete", "deleted sandbox %s (%s)", d.Name, d.Reason)
	}

	var blocker *status.Blocker
	var rejectedWait time.Duration
	waitForName := false
	leavingWait := time.Duration(-1) // the earliest recheck a name wait needs
	requeue := plan.Waiting
	for _, ord := range plan.Create {
		sb := translate.NewSandbox(&app, profile.ExecutorName, tmpl, ord)
		if ord == int(app.Spec.Replicas) {
			sb.Labels[translate.LabelSurge] = "true"
		}
		lookup := r.decorateCache(sb, cache)
		if err := controllerutil.SetControllerReference(&app, sb, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Client.Create(ctx, sb); err != nil {
			if reason, ok := rejectionReason(err); ok {
				// A quota or admission policy refused the object; it would
				// refuse the other creates too. Report it, stop, and retry
				// with a growing delay. The API message can echo values, so
				// only the log carries it.
				wait, first := r.Backoff.RecordRejection(key)
				if first {
					logger.Error(err, "SwiftSandbox create rejected", "sandbox", sb.Name)
				} else {
					logger.V(1).Info("SwiftSandbox create rejected again", "sandbox", sb.Name, "retryIn", wait)
				}
				note := ""
				if ord == int(app.Spec.Replicas) {
					note = "; it is the surge replica of a rolling update and the old replicas keep serving"
				}
				if blocker == nil {
					blocker = &status.Blocker{Reason: status.ReasonSandboxRejected,
						Message: fmt.Sprintf("the API server refused to create SwiftSandbox %s (%s)%s; check ResourceQuota on SwiftSandboxes and admission policies in namespace %s, and the controller log",
							sb.Name, reason, note, app.Namespace)}
				}
				rejectedWait = wait
				break
			}
			if !apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, fmt.Errorf("create SwiftSandbox %s: %w", sb.Name, err)
			}
			requeue = true
			holder, held, err := r.nameHolder(ctx, &app, sb.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			switch holder {
			case nameMine:
				// Created by an earlier reconcile; the cache has not
				// caught up yet.
				continue
			case nameFree:
				// Gone between the create and the read: retry soon.
				waitForName = true
				leavingWait = 0
				continue
			case nameLeaving:
				// A sandbox of a deleted SpinApp with the same name, still
				// terminating under KubeSwift's finalizer: not a conflict,
				// unless it takes far longer than KubeSwift allows.
				age := now.Sub(held.DeletionTimestamp.Time)
				if age < r.leavingTimeout(held) {
					logger.V(1).Info("waiting for a terminating sandbox of the same name", "sandbox", sb.Name)
					waitForName = true
					if left := r.leavingTimeout(held) - r.LeavingTimeout + stopAllowance - age; leavingWait < 0 || left < leavingWait {
						leavingWait = left
					}
					continue
				}
				if blocker == nil {
					blocker = &status.Blocker{Reason: status.ReasonSandboxConflict,
						Message: fmt.Sprintf("SwiftSandbox %s of a deleted SpinApp with this name has been terminating since %s and still has finalizers %s; check that KubeSwift is running",
							sb.Name, held.DeletionTimestamp.UTC().Format(time.RFC3339), strings.Join(held.Finalizers, ", "))}
				}
				continue
			}
			// The name is taken by an object this SpinApp does not control.
			// Never adopt it. Such an object has no owner reference to this
			// SpinApp and may not even be cached, so its removal triggers
			// no event: requeue to notice when the name is free.
			if blocker != nil {
				continue // report the first blocker; the rest need it resolved too
			}
			blocker = &status.Blocker{Reason: status.ReasonSandboxConflict,
				Message: fmt.Sprintf("SwiftSandbox %s already exists and is not controlled by this SpinApp; delete or rename it", sb.Name)}
			continue
		}
		r.Backoff.ClearRejection(key)
		metrics.SandboxCreations.Inc()
		if lookup != "" {
			metrics.CompiledCacheLookups.WithLabelValues(lookup).Inc()
		}
		logger.Info("created sandbox", "sandbox", sb.Name, "revision", tmpl.Revision)
		r.event(&app, corev1.EventTypeNormal, EventSandboxCreated, "Create", "created sandbox %s at revision %s", sb.Name, tmpl.Revision)
	}

	eventReason := EventConflict
	if blocker != nil && blocker.Reason == status.ReasonSandboxRejected {
		eventReason = EventRejected
	}
	if err := r.finishWithInstances(ctx, &app, instances, tmpl.Revision, blocker, exposure, cache, retryIn, eventReason); err != nil {
		return ctrl.Result{}, err
	}
	if rejectedWait > 0 && (backoffWait == 0 || rejectedWait < backoffWait) {
		return ctrl.Result{RequeueAfter: rejectedWait}, nil
	}
	if backoffWait > 0 {
		return ctrl.Result{RequeueAfter: backoffWait + 100*time.Millisecond}, nil
	}
	if waitForName && blocker == nil {
		return ctrl.Result{RequeueAfter: max(leavingRequeueMin, min(r.WaitingRequeue, leavingWait))}, nil
	}
	if requeue {
		// Watches normally trigger the next step (owned sandbox events);
		// this is a bounded safety net for conflicts and in-flight work.
		return ctrl.Result{RequeueAfter: r.WaitingRequeue}, nil
	}
	if cache.holdUntil != nil {
		// Report the end of a compiled-cache hold when it ends.
		return ctrl.Result{RequeueAfter: cache.holdUntil.Sub(r.Now()) + time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// rejectionReason reports whether err is a refusal by a quota or an admission
// policy, as a fixed word: an admission webhook chooses the reason text, so
// it is never echoed.
func rejectionReason(err error) (string, bool) {
	switch {
	case apierrors.IsForbidden(err):
		return "Forbidden", true
	case apierrors.IsInvalid(err):
		return "Invalid", true
	}
	return "", false
}

// appAnnotationsChanged lets an update through when a spin.kubeswift.io/
// annotation of the SpinApp changed: such an edit does not change the
// generation, which GenerationChangedPredicate looks at.
var appAnnotationsChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		if e.ObjectOld == nil || e.ObjectNew == nil {
			return false
		}
		return !maps.Equal(kubeswiftAnnotations(e.ObjectOld.GetAnnotations()), kubeswiftAnnotations(e.ObjectNew.GetAnnotations()))
	},
}

func kubeswiftAnnotations(ann map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range ann {
		if strings.HasPrefix(k, executor.Prefix) {
			out[k] = v
		}
	}
	return out
}

// leavingTimeout is how long a predecessor's terminating sandbox may hold a
// name: LeavingTimeout plus the drain period in that sandbox's own spec, so
// a long drain is not reported as a stuck sandbox whatever the current
// executor says.
func (r *SpinAppReconciler) leavingTimeout(sb *sandboxv1alpha1.SwiftSandbox) time.Duration {
	if t := sb.Spec.Termination; t != nil && t.DrainPeriod != nil && t.DrainPeriod.Duration > 0 {
		return r.LeavingTimeout + min(t.DrainPeriod.Duration, executor.MaxDrainPeriod)
	}
	return r.LeavingTimeout
}

// nameState is who holds a sandbox name this SpinApp wants to create.
type nameState int

const (
	nameTaken   nameState = iota // a foreign object: never adopted, a conflict
	nameFree                     // nobody (any longer)
	nameMine                     // this SpinApp
	nameLeaving                  // a deleted same-named SpinApp's sandbox, being deleted
)

// nameHolder reads a sandbox from the API server, bypassing the cache, and
// says who holds the name.
func (r *SpinAppReconciler) nameHolder(ctx context.Context, app *spinv1alpha1.SpinApp, name string) (nameState, *sandboxv1alpha1.SwiftSandbox, error) {
	var sb sandboxv1alpha1.SwiftSandbox
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: name}, &sb); err != nil {
		if apierrors.IsNotFound(err) {
			return nameFree, nil, nil
		}
		return nameTaken, nil, fmt.Errorf("read SwiftSandbox %s: %w", name, err)
	}
	if metav1.IsControlledBy(&sb, app) {
		return nameMine, &sb, nil
	}
	if !sb.DeletionTimestamp.IsZero() && ownedByPredecessor(&sb, app) {
		return nameLeaving, &sb, nil
	}
	return nameTaken, &sb, nil
}

// ownedByPredecessor reports whether sb is controlled by an earlier SpinApp
// with app's name (same name, another UID), which a deletion and re-creation
// of the SpinApp leaves behind.
func ownedByPredecessor(sb *sandboxv1alpha1.SwiftSandbox, app *spinv1alpha1.SpinApp) bool {
	ref := metav1.GetControllerOf(sb)
	if ref == nil || ref.Kind != "SpinApp" || ref.Name != app.Name || ref.UID == app.UID {
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && gv.Group == spinv1alpha1.GroupVersion.Group
}

// features returns the SwiftSandbox features of the installed KubeSwift.
// The detector keeps its last successful result on errors, so a transient
// discovery failure never changes the rendered sandbox spec.
func (r *SpinAppReconciler) features(ctx context.Context) capabilities.Sandbox {
	if r.Detector == nil {
		return capabilities.Sandbox{}
	}
	f, err := r.Detector.Sandbox(ctx)
	if err != nil {
		log.FromContext(ctx).V(1).Info("sandbox capability detection failed; using the last result", "error", err.Error())
	}
	return f
}

// isNewGeneration reports whether this generation has not been processed
// yet, so per-generation Events and counters fire once.
func (r *SpinAppReconciler) isNewGeneration(app *spinv1alpha1.SpinApp) bool {
	c := apimeta.FindStatusCondition(app.Status.Conditions, status.TypeProgressing)
	return c == nil || c.ObservedGeneration != app.Generation
}

func (r *SpinAppReconciler) checkPool(ctx context.Context, app *spinv1alpha1.SpinApp, p *executor.Profile, tmpl *translate.Template) (*status.Blocker, error) {
	if !r.PoolsServed {
		return &status.Blocker{Reason: status.ReasonWarmPoolIncompatible,
			Message: fmt.Sprintf("executor %q selects SwiftSandboxPool %q, but the SwiftSandboxPool API was not installed when kubeswift-spin started; install it and restart the controller", p.ExecutorName, p.SandboxPool)}, nil
	}
	var pool sandboxv1alpha1.SwiftSandboxPool
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: p.SandboxPool}, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			return &status.Blocker{Reason: status.ReasonWarmPoolIncompatible,
				Message: fmt.Sprintf("SwiftSandboxPool %q selected by executor %q does not exist in namespace %q", p.SandboxPool, p.ExecutorName, app.Namespace)}, nil
		}
		return nil, err
	}
	if mm := translate.PoolMismatches(&tmpl.Spec, app.Namespace, &pool); len(mm) > 0 {
		return &status.Blocker{Reason: status.ReasonWarmPoolIncompatible,
			Message: fmt.Sprintf("SwiftSandboxPool %q is incompatible with this SpinApp: %s", p.SandboxPool, strings.Join(mm, "; "))}, nil
	}
	return nil, nil
}

// ownedSandboxes lists the sandboxes this SpinApp controls. Selection uses
// labels, but only a controller owner reference to this exact SpinApp (by
// UID) makes a sandbox owned, so a lookalike object is never adopted.
func (r *SpinAppReconciler) ownedSandboxes(ctx context.Context, app *spinv1alpha1.SpinApp) ([]sandboxv1alpha1.SwiftSandbox, error) {
	var list sandboxv1alpha1.SwiftSandboxList
	if err := r.Client.List(ctx, &list, client.InNamespace(app.Namespace), client.MatchingLabels(translate.SelectorLabels(app.Name))); err != nil {
		return nil, fmt.Errorf("list SwiftSandboxes: %w", err)
	}
	var out []sandboxv1alpha1.SwiftSandbox
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], app) {
			out = append(out, list.Items[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *SpinAppReconciler) deleteSandbox(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, reason string) error {
	if !sb.DeletionTimestamp.IsZero() {
		return nil
	}
	uid := sb.UID
	// Foreground deletion keeps the SwiftSandbox until KubeSwift's launcher
	// pod, intent ConfigMap and NetworkPolicy are gone, so a replacement with
	// the same name never races the old launcher pod.
	err := r.Client.Delete(ctx, sb,
		client.PropagationPolicy(metav1.DeletePropagationForeground),
		client.Preconditions{UID: &uid})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("delete SwiftSandbox %s: %w", sb.Name, err)
	}
	metrics.SandboxDeletions.WithLabelValues(reason).Inc()
	log.FromContext(ctx).Info("deleted sandbox", "sandbox", sb.Name, "reason", reason)
	return nil
}

// observe converts owned sandboxes into status instances and records
// failures for backoff and metrics.
func (r *SpinAppReconciler) observe(app *spinv1alpha1.SpinApp, owned []sandboxv1alpha1.SwiftSandbox, revision string, now time.Time) []status.Instance {
	key := client.ObjectKeyFromObject(app).String()
	out := make([]status.Instance, 0, len(owned))
	for i := range owned {
		sb := &owned[i]
		inst := Classify(app.Name, sb)
		if inst.Health == rollout.Terminal && !inst.Deleting {
			if r.Backoff.RecordFailure(key, inst.Ordinal, string(sb.UID), inst.Revision, now) {
				metrics.SandboxFailures.Inc()
				r.event(app, corev1.EventTypeWarning, EventSandboxFailed, "Observe", "sandbox %s reached phase %s (%s)",
					sb.Name, sb.Status.Phase, firstNonEmpty(inst.FailureReason, "no reason reported"))
			}
		}
		if (inst.Health == rollout.Running || inst.Health == rollout.Ready) && inst.Revision == revision {
			r.Backoff.RecordRunning(key, inst.Ordinal, string(sb.UID), now)
		}
		out = append(out, inst)
	}
	return out
}

// Classify derives the replica state of one sandbox.
func Classify(appName string, sb *sandboxv1alpha1.SwiftSandbox) status.Instance {
	inst := status.Instance{Instance: rollout.Instance{
		Name:     sb.Name,
		Revision: sb.Labels[translate.LabelRevision],
		Deleting: !sb.DeletionTimestamp.IsZero(),
		Surge:    sb.Labels[translate.LabelSurge] == "true",
	}}
	ord, err := translate.ParseOrdinal(sb.Labels[translate.LabelOrdinal])
	if err != nil || translate.SandboxName(appName, ord) != sb.Name {
		inst.Stale = true
	}
	inst.Ordinal = ord

	switch sb.Status.Phase {
	case sandboxv1alpha1.SwiftSandboxRunning:
		inst.Health = rollout.Running
		// Ready needs evidence that Spin serves: a readiness probe on this
		// sandbox and KubeSwift reporting it passes. A running guest alone
		// is not enough, and a sandbox without a probe (created before
		// KubeSwift supported them) is never counted as ready.
		if sb.Spec.ReadinessProbe != nil && apimeta.IsStatusConditionTrue(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionWorkloadReady) {
			inst.Health = rollout.Ready
		}
	case sandboxv1alpha1.SwiftSandboxCompleted, sandboxv1alpha1.SwiftSandboxFailed:
		inst.Health = rollout.Terminal
		if c := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning); c != nil && c.Status == metav1.ConditionFalse {
			inst.FailureReason, inst.FailureMessage = c.Reason, c.Message
		} else {
			inst.FailureReason, inst.FailureMessage = string(sb.Status.Phase), sb.Status.Message
		}
	case sandboxv1alpha1.SwiftSandboxMaterializing:
		inst.Health = rollout.Pending
		inst.Materializing = true
	default:
		inst.Health = rollout.Pending
	}
	if inst.Health == rollout.Pending {
		if c := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved); c != nil && c.Status == metav1.ConditionFalse {
			inst.WaitingReason, inst.WaitingMessage = c.Reason, c.Message
		}
	}
	return inst
}

func rolloutView(in []status.Instance) []rollout.Instance {
	out := make([]rollout.Instance, len(in))
	for i := range in {
		out[i] = in[i].Instance
	}
	return out
}

func (r *SpinAppReconciler) finish(ctx context.Context, app *spinv1alpha1.SpinApp, owned []sandboxv1alpha1.SwiftSandbox,
	revision string, blocker *status.Blocker, exposure status.Exposure, cache cacheView, eventReason string) error {
	instances := make([]status.Instance, 0, len(owned))
	for i := range owned {
		instances = append(instances, Classify(app.Name, &owned[i]))
	}
	if revision == "" {
		// No template could be built; compare against the revision the
		// replicas already run so they are not reported as outdated.
		revision = commonRevision(instances)
	}
	return r.finishWithInstances(ctx, app, instances, revision, blocker, exposure, cache, "", eventReason)
}

func (r *SpinAppReconciler) finishWithInstances(ctx context.Context, app *spinv1alpha1.SpinApp, instances []status.Instance,
	revision string, blocker *status.Blocker, exposure status.Exposure, cache cacheView, retryIn, eventReason string) error {
	res := status.Compute(status.Input{
		Namespace:  app.Namespace,
		Generation: app.Generation,
		Replicas:   app.Spec.Replicas,
		Revision:   revision,
		Instances:  instances,
		Exposure:   exposure,
		Blocker:    blocker,
		RetryIn:    retryIn,
	})

	prev := apimeta.FindStatusCondition(app.Status.Conditions, status.TypeProgressing)
	if blocker != nil && (prev == nil || prev.Reason != blocker.Reason || prev.Message != res.Progressing.Message) {
		r.event(app, corev1.EventTypeWarning, eventReason, "Reconcile", "%s", blocker.Message)
	}

	base := app.DeepCopy()
	app.Status.ActiveScheduler = app.Spec.Executor
	app.Status.ReadyReplicas = res.ReadyReplicas
	status.Apply(&app.Status.Conditions, res)
	r.applyCache(app, base.Status.Conditions, cache)

	state := metrics.StateProgressing
	switch {
	case res.Available.Status == metav1.ConditionTrue:
		state = metrics.StateAvailable
	case blocker != nil:
		state = metrics.StateBlocked
	case res.Progressing.Status == metav1.ConditionFalse || res.Available.Reason == status.ReasonNetworkUnavailable:
		state = metrics.StateUnavailable
	}
	r.Metrics.Set(client.ObjectKeyFromObject(app).String(), state, res.ReadyReplicas, app.Spec.Replicas)

	if apiequality.Semantic.DeepEqual(base.Status, app.Status) {
		return nil
	}
	patch, err := statusPatch(app)
	if err != nil {
		return err
	}
	if err := r.Client.Status().Patch(ctx, app, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("patch SpinApp status: %w", err)
	}
	return nil
}

// statusPatch builds a JSON merge patch carrying exactly the status fields
// kubeswift-spin owns: activeScheduler, readyReplicas and the conditions
// list. readyReplicas is always sent because the SpinApp CRD requires it.
// Fields of status not named here are left untouched by the merge.
//
// A merge patch replaces the whole conditions list, so the patch carries the
// resourceVersion it was computed from: if anyone else changed the SpinApp in
// between, the API server rejects the patch with a conflict and the reconcile
// is retried on fresh data. Conditions of other types are carried over by
// status.Apply, so they are never dropped.
func statusPatch(app *spinv1alpha1.SpinApp) ([]byte, error) {
	conds := app.Status.Conditions
	if conds == nil {
		conds = []metav1.Condition{}
	}
	p := map[string]any{
		"metadata": map[string]any{"resourceVersion": app.ResourceVersion},
		"status": map[string]any{
			"activeScheduler": app.Status.ActiveScheduler,
			"readyReplicas":   app.Status.ReadyReplicas,
			"conditions":      conds,
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode SpinApp status patch: %w", err)
	}
	return b, nil
}

func (r *SpinAppReconciler) event(app *spinv1alpha1.SpinApp, eventType, reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	// events.k8s.io/v1 rejects notes longer than 1024 bytes.
	r.Recorder.Eventf(app, nil, eventType, reason, action, "%s", status.Truncate(fmt.Sprintf(note, args...), maxEventNote))
}

const maxEventNote = 1000

func (r *SpinAppReconciler) forget(key string) {
	r.Metrics.Forget(key)
	r.Backoff.Forget(key)
	r.annMu.Lock()
	delete(r.annSeen, key)
	r.annMu.Unlock()
	r.forgetAttachments(key)
}

// annotationsChanged records the SpinApp's spin.kubeswift.io/ annotations
// and reports whether they differ from those of its previous reconcile. The
// first reconcile after a restart reports no change, so a restart does not
// repeat Events for every annotated SpinApp.
func (r *SpinAppReconciler) annotationsChanged(key string, app *spinv1alpha1.SpinApp) bool {
	ann := kubeswiftAnnotations(app.Annotations)
	keys := slices.Sorted(maps.Keys(ann))
	var sig strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sig, "%s=%s\n", k, ann[k])
	}
	r.annMu.Lock()
	defer r.annMu.Unlock()
	if r.annSeen == nil {
		r.annSeen = map[string]string{}
	}
	prev, seen := r.annSeen[key]
	r.annSeen[key] = sig.String()
	return seen && prev != sig.String()
}

func findByName(list []sandboxv1alpha1.SwiftSandbox, name string) *sandboxv1alpha1.SwiftSandbox {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

func commonRevision(in []status.Instance) string {
	counts := map[string]int{}
	best, bestN := "", 0
	for _, i := range in {
		counts[i.Revision]++
		if counts[i.Revision] > bestN || (counts[i.Revision] == bestN && i.Revision < best) {
			best, bestN = i.Revision, counts[i.Revision]
		}
	}
	return best
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
