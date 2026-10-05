// Package controller reconciles SpinApps whose executor is managed by
// kubeswift-spin into KubeSwift SwiftSandboxes.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"

	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/compatibility"
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
	EventExecutorChanged    = "ExecutorChanged"
)

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

	Metrics *metrics.Tracker
	Backoff *Backoff
	Now     func() time.Time
	// WaitingRequeue is how long to wait before rechecking a SpinApp whose
	// rollout waits on something no watch reports, such as a foreign object
	// occupying a sandbox name. Defaults to 30 seconds.
	WaitingRequeue time.Duration
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
		For(&spinv1alpha1.SpinApp{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&sandboxv1alpha1.SwiftSandbox{}).
		Watches(&spinv1alpha1.SpinAppExecutor{}, handler.EnqueueRequestsFromMapFunc(r.appsForExecutor))
	if r.PoolsServed {
		b = b.Watches(&sandboxv1alpha1.SwiftSandboxPool{}, handler.EnqueueRequestsFromMapFunc(r.appsForPool))
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

// Reconcile realizes one SpinApp.
func (r *SpinAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	defer func() {
		if err != nil {
			metrics.Reconciliations.WithLabelValues("error").Inc()
			metrics.ReconcileErrors.Inc()
		} else {
			metrics.Reconciliations.WithLabelValues("success").Inc()
		}
	}()
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
		return ctrl.Result{}, r.finish(ctx, &app, owned, "", blocker, status.Exposure{}, EventExecutorNotFound)
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
		return ctrl.Result{}, r.finish(ctx, &app, owned, "", blocker, exposure, EventExecutorInvalid)
	}

	opts := r.Options
	opts.Features = features
	findings := compatibility.Analyze(&app, profile, opts)
	newGeneration := r.isNewGeneration(&app)
	if newGeneration {
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
		return ctrl.Result{}, r.finish(ctx, &app, owned, "", blocker, exposure, EventUnsupported)
	}

	tmpl, err := translate.BuildTemplate(&app, profile, r.Options.Resources, features)
	if err != nil {
		blocker := &status.Blocker{Reason: status.ReasonUnsupportedConfiguration, Message: err.Error()}
		return ctrl.Result{}, r.finish(ctx, &app, owned, "", blocker, exposure, EventUnsupported)
	}

	if profile.SandboxPool != "" {
		if blocker, err := r.checkPool(ctx, &app, profile, tmpl); err != nil {
			return ctrl.Result{}, err
		} else if blocker != nil {
			return ctrl.Result{}, r.finish(ctx, &app, owned, tmpl.Revision, blocker, exposure, EventWarmPool)
		}
	}

	now := r.Now()
	instances := r.observe(&app, owned, tmpl.Revision, now)
	inBackoff, retryIn := r.Backoff.Pending(key, instances, now)

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
	requeue := plan.Waiting
	for _, ord := range plan.Create {
		sb := translate.NewSandbox(&app, profile.ExecutorName, tmpl, ord)
		if err := controllerutil.SetControllerReference(&app, sb, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Client.Create(ctx, sb); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, fmt.Errorf("create SwiftSandbox %s: %w", sb.Name, err)
			}
			requeue = true
			mine, err := r.controlledByApp(ctx, &app, sb.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			if mine {
				// Created by an earlier reconcile; the cache has not
				// caught up yet.
				continue
			}
			// The name is taken by an object this SpinApp does not control.
			// Never adopt it. Such an object has no owner reference to this
			// SpinApp and may not even be cached, so its removal triggers
			// no event: requeue to notice when the name is free.
			blocker = &status.Blocker{Reason: status.ReasonSandboxConflict,
				Message: fmt.Sprintf("SwiftSandbox %s already exists and is not controlled by this SpinApp; delete or rename it", sb.Name)}
			continue
		}
		metrics.SandboxCreations.Inc()
		logger.Info("created sandbox", "sandbox", sb.Name, "revision", tmpl.Revision)
		r.event(&app, corev1.EventTypeNormal, EventSandboxCreated, "Create", "created sandbox %s at revision %s", sb.Name, tmpl.Revision)
	}

	if err := r.finishWithInstances(ctx, &app, instances, tmpl.Revision, blocker, exposure, retryIn, EventConflict); err != nil {
		return ctrl.Result{}, err
	}
	if retryIn != "" {
		return ctrl.Result{RequeueAfter: r.Backoff.NextDelay(key, now)}, nil
	}
	if requeue {
		// Watches normally trigger the next step (owned sandbox events);
		// this is a bounded safety net for conflicts and in-flight work.
		return ctrl.Result{RequeueAfter: r.WaitingRequeue}, nil
	}
	return ctrl.Result{}, nil
}

// controlledByApp reads a sandbox from the API server, bypassing the cache,
// and reports whether this SpinApp controls it.
func (r *SpinAppReconciler) controlledByApp(ctx context.Context, app *spinv1alpha1.SpinApp, name string) (bool, error) {
	var sb sandboxv1alpha1.SwiftSandbox
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: name}, &sb); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read SwiftSandbox %s: %w", name, err)
	}
	return metav1.IsControlledBy(&sb, app), nil
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
			if r.Backoff.RecordFailure(key, inst.Ordinal, string(sb.UID), now) {
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
	revision string, blocker *status.Blocker, exposure status.Exposure, eventReason string) error {
	instances := make([]status.Instance, 0, len(owned))
	for i := range owned {
		instances = append(instances, Classify(app.Name, &owned[i]))
	}
	if revision == "" {
		// No template could be built; compare against the revision the
		// replicas already run so they are not reported as outdated.
		revision = commonRevision(instances)
	}
	return r.finishWithInstances(ctx, app, instances, revision, blocker, exposure, "", eventReason)
}

func (r *SpinAppReconciler) finishWithInstances(ctx context.Context, app *spinv1alpha1.SpinApp, instances []status.Instance,
	revision string, blocker *status.Blocker, exposure status.Exposure, retryIn, eventReason string) error {
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
