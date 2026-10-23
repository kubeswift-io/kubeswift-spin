package cachemanager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/metrics"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

// Export allowances on top of a compiler's own timeout: the time KubeSwift
// may take to start exporting, and the most an export in progress (phase
// Exporting) is waited for, which covers a slow registry upload.
const (
	exportAllowance  = 15 * time.Minute
	exportingCeiling = 2 * time.Hour
)

// Record Event reasons.
const (
	EventCompileStarted   = "CompileStarted"
	EventCompileSucceeded = "CompileSucceeded"
	EventCompileFailed    = "CompileFailed"
)

// RecordReconciler drives one SpinCompiledCache record: it runs compile
// attempts in compiler sandboxes and keeps the record's conditions.
type RecordReconciler struct {
	Client client.Client
	// APIReader reads records and compiler sandboxes past the informer
	// cache: every decision, and every deletion, is made on current data.
	APIReader client.Reader
	Settings  compiledcache.Settings
	Compiler  compiledcache.CompilerSettings
	Defaults  executor.Defaults
	Recorder  events.EventRecorder
	// MaxCompilers bounds compiler sandboxes running at once, cluster-wide
	// and per namespace, so one namespace cannot hold KubeSwift's export
	// capacity.
	MaxCompilers             int
	MaxCompilersPerNamespace int
	Now                      func() time.Time
	// NewToken returns a fresh compile attempt token.
	NewToken func() string
}

// SetupWithManager registers the reconciler.
func (r *RecordReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.NewToken == nil {
		r.NewToken = randomToken
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Compiler.CPU == 0 {
		r.Compiler = compiledcache.DefaultCompilerSettings()
	}
	if r.MaxCompilers == 0 {
		r.MaxCompilers = 4
	}
	if r.MaxCompilersPerNamespace == 0 {
		r.MaxCompilersPerNamespace = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("compiledcache-record").
		For(&cachev1alpha1.SpinCompiledCache{}).
		Watches(&sandboxv1alpha1.SwiftSandbox{}, handler.EnqueueRequestsFromMapFunc(r.recordsForCompiler),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
				return o.GetLabels()[compiledcache.LabelComponent] == compiledcache.ComponentCompiler
			}))).
		Complete(r)
}

// recordsForCompiler enqueues a compiler's record, and the other records of
// its namespace, which may be waiting for its compiler slot.
func (r *RecordReconciler) recordsForCompiler(ctx context.Context, o client.Object) []reconcile.Request {
	var out []reconcile.Request
	if name := o.GetLabels()[compiledcache.LabelRecord]; name != "" {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: r.Settings.Namespace, Name: name}})
	}
	var list cachev1alpha1.SpinCompiledCacheList
	if err := r.Client.List(ctx, &list, client.InNamespace(r.Settings.Namespace),
		client.MatchingLabels{compiledcache.LabelNamespace: o.GetNamespace()}); err == nil {
		for i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return out
}

func randomToken() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Reconcile drives one record.
func (r *RecordReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rec cachev1alpha1.SpinCompiledCache
	if err := r.APIReader.Get(ctx, req.NamespacedName, &rec); err != nil {
		if apierrors.IsNotFound(err) {
			// The record is gone: so are its compilers.
			return ctrl.Result{}, r.deleteCompilers(ctx, "", req.Name, nil)
		}
		return ctrl.Result{}, err
	}
	if !rec.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.deleteCompilers(ctx, rec.Spec.Namespace, rec.Name, nil)
	}
	logger := log.FromContext(ctx).WithValues("record", rec.Name, "namespace", rec.Spec.Namespace)
	ctx = log.IntoContext(ctx, logger)

	requeue, err := r.compile(ctx, &rec)
	if err == nil {
		err = r.writeConditions(ctx, &rec)
	}
	if apierrors.IsConflict(err) {
		// Another writer (the namespace reconciler or the signer) changed
		// the record; retry on fresh data.
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: requeue}, err
}

// compilers lists the compiler sandboxes of a record (of every record when
// record is empty) that the cache manager could have created. Labels alone
// never make a sandbox a compiler: it must also run compile mode with an
// allowed runtime and export the cache output.
func (r *RecordReconciler) compilers(ctx context.Context, ns, record string) ([]sandboxv1alpha1.SwiftSandbox, error) {
	var l sandboxv1alpha1.SwiftSandboxList
	labels := client.MatchingLabels{compiledcache.LabelComponent: compiledcache.ComponentCompiler}
	if record != "" {
		labels[compiledcache.LabelRecord] = record
	}
	opts := []client.ListOption{labels}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	if err := r.Client.List(ctx, &l, opts...); err != nil {
		return nil, fmt.Errorf("list compiler sandboxes: %w", err)
	}
	var out []sandboxv1alpha1.SwiftSandbox
	for i := range l.Items {
		if compiledcache.IsCompilerShaped(&l.Items[i], r.Settings.Runtimes) {
			out = append(out, l.Items[i])
		}
	}
	return out, nil
}

// deleteCompilers deletes the record's compiler sandboxes except keep, each
// by UID precondition.
func (r *RecordReconciler) deleteCompilers(ctx context.Context, ns, record string, keep *cachev1alpha1.SandboxReference) error {
	list, err := r.compilers(ctx, ns, record)
	if err != nil {
		return err
	}
	for i := range list {
		sb := &list[i]
		if (keep != nil && sb.Name == keep.Name && sb.UID == keep.UID) || !sb.DeletionTimestamp.IsZero() {
			continue
		}
		uid := sb.UID
		if err := r.Client.Delete(ctx, sb, client.PropagationPolicy(metav1.DeletePropagationBackground), client.Preconditions{UID: &uid}); err != nil &&
			!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return fmt.Errorf("delete compiler sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
		}
		log.FromContext(ctx).Info("deleted compiler sandbox", "sandbox", sb.Name, "sandboxNamespace", sb.Namespace)
	}
	return nil
}

// compile advances the record's compile attempt. The record was read from
// the API server, and every status write carries its resourceVersion, so a
// decision made on it is either written as made or retried on fresh data.
// It returns when to look again if no watch event will say so.
func (r *RecordReconciler) compile(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache) (time.Duration, error) {
	now := r.Now()
	c := cachev1alpha1.CompileStatus{}
	if rec.Status.Compile != nil {
		c = *rec.Status.Compile.DeepCopy()
	}

	// Signed: nothing left to compile. Failures are forgotten only now, so
	// an artifact the signer keeps refusing backs off ever longer.
	if compiledcache.TrustedArtifact(rec) != "" {
		if c.Failures != 0 || c.RetryAfter != nil || c.Reason != "" {
			c.Failures, c.LastFailureTime, c.RetryAfter, c.Reason, c.Message = 0, nil, nil, "", ""
			if err := r.patchCompile(ctx, rec, &c); err != nil {
				return 0, err
			}
		}
		return 0, r.deleteCompilers(ctx, rec.Spec.Namespace, rec.Name, nil)
	}

	// Exported and waiting for the signer, which needs the compiler sandbox
	// to check the artifact's origin.
	if c.Artifact != "" && c.Sandbox != nil {
		if s := rec.Status.Signature; s != nil && s.Artifact == c.Artifact && s.SandboxUID == c.Sandbox.UID && s.Rejected {
			r.fail(&c, now, "SignerRejected", fmt.Sprintf("the signer refused %s (%s)", c.Artifact, s.Reason))
			return r.failed(ctx, rec, &c)
		}
		var sb sandboxv1alpha1.SwiftSandbox
		err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: rec.Spec.Namespace, Name: c.Sandbox.Name}, &sb)
		switch {
		case apierrors.IsNotFound(err) || (err == nil && (sb.UID != c.Sandbox.UID || !sb.DeletionTimestamp.IsZero())):
			// Without its compiler the artifact can never be signed.
			r.fail(&c, now, "CompilerGoneBeforeSigning", fmt.Sprintf("compiler sandbox %s went away before the signer checked %s", c.Sandbox.Name, c.Artifact))
			return r.failed(ctx, rec, &c)
		case err != nil:
			return 0, err
		}
		return 0, r.deleteCompilers(ctx, rec.Spec.Namespace, rec.Name, c.Sandbox)
	}

	// An attempt is running.
	if c.Sandbox != nil && c.FinishedAt == nil {
		return r.observe(ctx, rec, &c, now)
	}

	// No attempt running: start one unless in backoff.
	if c.RetryAfter != nil && now.Before(c.RetryAfter.Time) {
		return until(now, c.RetryAfter), r.deleteCompilers(ctx, rec.Spec.Namespace, rec.Name, nil)
	}
	app, p, err := r.consumer(ctx, rec, c.Attempt)
	if err != nil || app == nil {
		// Nobody to compile for (yet); a consumer change re-triggers.
		return 0, err
	}
	if busy, err := r.atCapacity(ctx, rec); err != nil || busy {
		return 30 * time.Second, err
	}
	c.Attempt++
	c.Token = r.NewToken()
	c.Sandbox, c.SpecHash, c.StartedAt, c.FinishedAt, c.Artifact, c.Size, c.RetryAfter = nil, "", nil, nil, "", 0, nil
	// Claim the attempt first: it succeeds only on the current version of
	// the record, so a second cache manager (leader handover) or a stale
	// view cannot start a second compiler.
	if err := r.patchCompile(ctx, rec, &c); err != nil {
		return 0, err
	}
	// Compilers of attempts that were never recorded (a restart between
	// creating one and recording it) are not trusted: remove them.
	if err := r.deleteCompilers(ctx, rec.Spec.Namespace, rec.Name, nil); err != nil {
		return 0, err
	}
	sb := compiledcache.CompilerSandbox(rec, app, p, r.Compiler, c.Token)
	if err := r.Client.Create(ctx, sb); err != nil {
		log.FromContext(ctx).Error(err, "create compiler sandbox")
		if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
			// Transient: the work queue retries the claimed attempt with
			// backoff.
			return 0, fmt.Errorf("create compiler sandbox: %w", err)
		}
		// A quota or admission policy refused it. The API message can echo
		// spec values, so only the log carries it.
		r.fail(&c, now, "CompilerRejected", "the API server refused to create the compiler sandbox; see the cache manager log")
		return r.failed(ctx, rec, &c)
	}
	if err := r.record(ctx, rec, &c, sb, now); err != nil {
		return 0, err
	}
	metrics.CompiledCacheCompilations.WithLabelValues("started", "").Inc()
	r.event(rec, corev1.EventTypeNormal, EventCompileStarted, "compiling %s in sandbox %s/%s (attempt %d)", rec.Spec.Source, sb.Namespace, sb.Name, c.Attempt)
	log.FromContext(ctx).Info("started compiler sandbox", "sandbox", sb.Name, "attempt", c.Attempt)
	return r.Compiler.Timeout + exportAllowance, nil
}

// record writes the new compiler's name, UID and spec hash. A conflict
// (another writer touched the record) is retried on a fresh read as long as
// the attempt is still this one; otherwise the compiler is deleted, never
// left running unrecorded.
func (r *RecordReconciler) record(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache, c *cachev1alpha1.CompileStatus, sb *sandboxv1alpha1.SwiftSandbox, now time.Time) error {
	var stored unstructured.Unstructured
	stored.SetGroupVersionKind(sandboxv1alpha1.GroupVersion.WithKind("SwiftSandbox"))
	err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(sb), &stored)
	if err == nil && stored.GetUID() != sb.UID {
		err = fmt.Errorf("compiler sandbox %s was replaced right after it was created", sb.Name)
	}
	if err == nil {
		var hash string
		if hash, err = compiledcache.SpecHash(&stored); err == nil {
			c.Sandbox = &cachev1alpha1.SandboxReference{Name: sb.Name, UID: sb.UID}
			c.SpecHash = hash
			c.StartedAt = &metav1.Time{Time: now}
			for i := 0; i < 5; i++ {
				err = r.patchCompile(ctx, rec, c)
				if !apierrors.IsConflict(err) {
					break
				}
				var fresh cachev1alpha1.SpinCompiledCache
				if gerr := r.APIReader.Get(ctx, client.ObjectKeyFromObject(rec), &fresh); gerr != nil {
					err = gerr
					break
				}
				if fc := fresh.Status.Compile; fc == nil || fc.Token != c.Token || fc.Sandbox != nil {
					err = fmt.Errorf("the compile attempt changed while its compiler was being recorded")
					break // the attempt is no longer ours
				}
				*rec = fresh
			}
		}
	}
	if err == nil {
		return nil
	}
	uid := sb.UID
	if derr := r.Client.Delete(ctx, sb, client.Preconditions{UID: &uid}); derr != nil && !apierrors.IsNotFound(derr) {
		log.FromContext(ctx).Error(derr, "delete an unrecorded compiler sandbox", "sandbox", sb.Name)
	}
	return err
}

// atCapacity reports whether starting a compiler now would exceed the
// cluster-wide or per-namespace limit. Both the running attempts in record
// status and the live compiler sandboxes count, whichever is higher.
func (r *RecordReconciler) atCapacity(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache) (bool, error) {
	var list cachev1alpha1.SpinCompiledCacheList
	if err := r.Client.List(ctx, &list, client.InNamespace(r.Settings.Namespace)); err != nil {
		return false, err
	}
	total, inNS := 0, 0
	for i := range list.Items {
		c := list.Items[i].Status.Compile
		if list.Items[i].Name == rec.Name || c == nil || c.Sandbox == nil || c.FinishedAt != nil {
			continue
		}
		total++
		if list.Items[i].Spec.Namespace == rec.Spec.Namespace {
			inNS++
		}
	}
	live, err := r.compilers(ctx, "", "")
	if err != nil {
		return false, err
	}
	liveTotal, liveNS := 0, 0
	for i := range live {
		sb := &live[i]
		if sb.Labels[compiledcache.LabelRecord] == rec.Name || !sb.DeletionTimestamp.IsZero() ||
			sb.Status.Phase == sandboxv1alpha1.SwiftSandboxCompleted || sb.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed {
			continue
		}
		liveTotal++
		if sb.Namespace == rec.Spec.Namespace {
			liveNS++
		}
	}
	return max(total, liveTotal) >= r.MaxCompilers || max(inNS, liveNS) >= r.MaxCompilersPerNamespace, nil
}

// observe follows the running attempt's compiler sandbox.
func (r *RecordReconciler) observe(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache, c *cachev1alpha1.CompileStatus, now time.Time) (time.Duration, error) {
	var sb sandboxv1alpha1.SwiftSandbox
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: rec.Spec.Namespace, Name: c.Sandbox.Name}, &sb)
	switch {
	case apierrors.IsNotFound(err):
		r.fail(c, now, "CompilerDeleted", fmt.Sprintf("compiler sandbox %s was deleted before it finished", c.Sandbox.Name))
		return r.failed(ctx, rec, c)
	case err != nil:
		return 0, err
	case sb.UID != c.Sandbox.UID:
		r.fail(c, now, "CompilerReplaced", fmt.Sprintf("compiler sandbox %s was replaced by another object", c.Sandbox.Name))
		return r.failed(ctx, rec, c)
	}
	// Strays of earlier attempts go; the current compiler stays.
	if err := r.deleteCompilers(ctx, rec.Spec.Namespace, rec.Name, c.Sandbox); err != nil {
		return 0, err
	}
	switch sb.Status.Phase {
	case sandboxv1alpha1.SwiftSandboxCompleted:
		ref, err := compiledcache.ExportedArtifact(rec, &sb)
		if err != nil {
			r.fail(c, now, "ExportInvalid", err.Error())
			return r.failed(ctx, rec, c)
		}
		c.Artifact = ref
		c.FinishedAt = &metav1.Time{Time: now}
		c.RetryAfter = nil
		for _, o := range sb.Status.Outputs {
			if o.Name == compiledcache.OutputName {
				c.Size = o.Size
			}
		}
		if err := r.patchCompile(ctx, rec, c); err != nil {
			return 0, err
		}
		metrics.CompiledCacheCompilations.WithLabelValues("succeeded", "").Inc()
		r.event(rec, corev1.EventTypeNormal, EventCompileSucceeded, "exported %s", ref)
		log.FromContext(ctx).Info("compiled cache exported", "artifact", ref, "attempt", c.Attempt)
		return 0, nil
	case sandboxv1alpha1.SwiftSandboxFailed:
		reason, msg := compiledcache.CompileFailure(&sb)
		r.fail(c, now, reason, msg)
		return r.failed(ctx, rec, c)
	}
	if c.StartedAt == nil {
		return 0, nil
	}
	deadline := c.StartedAt.Add(r.Compiler.Timeout + exportAllowance)
	if sb.Status.Phase == sandboxv1alpha1.SwiftSandboxExporting {
		// The workload exited 0; a slow upload is waited for, within a
		// ceiling.
		deadline = c.StartedAt.Add(r.Compiler.Timeout + exportingCeiling)
	}
	if now.After(deadline) {
		r.fail(c, now, "CompileTimeout", fmt.Sprintf("compiler sandbox %s did not finish within %s", c.Sandbox.Name, deadline.Sub(c.StartedAt.Time)))
		return r.failed(ctx, rec, c)
	}
	return deadline.Sub(now) + time.Second, nil
}

// fail records a failed attempt and its backoff in c.
func (r *RecordReconciler) fail(c *cachev1alpha1.CompileStatus, now time.Time, reason, msg string) {
	c.Failures++
	c.LastFailureTime = &metav1.Time{Time: now}
	c.RetryAfter = &metav1.Time{Time: now.Add(compiledcache.Backoff(c.Failures))}
	c.FinishedAt = &metav1.Time{Time: now}
	c.Sandbox, c.SpecHash, c.Artifact, c.Size = nil, "", "", 0
	c.Reason, c.Message = reason, truncate(msg, 512)
}

// failed writes a failure recorded by fail, then reports it and removes the
// failed compiler. The Event and metric follow the write, so a failure that
// was not written is not reported.
func (r *RecordReconciler) failed(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache, c *cachev1alpha1.CompileStatus) (time.Duration, error) {
	if err := r.patchCompile(ctx, rec, c); err != nil {
		return 0, err
	}
	metrics.CompiledCacheCompilations.WithLabelValues("failed", metrics.Reason(c.Reason)).Inc()
	r.event(rec, corev1.EventTypeWarning, EventCompileFailed, "attempt %d failed (%s): %s; next attempt after %s",
		c.Attempt, c.Reason, c.Message, c.RetryAfter.UTC().Format(time.RFC3339))
	log.FromContext(ctx).Info("compile attempt failed", "attempt", c.Attempt, "reason", c.Reason, "retryAfter", c.RetryAfter.Time)
	return until(r.Now(), c.RetryAfter), r.deleteCompilers(ctx, rec.Spec.Namespace, rec.Name, nil)
}

func until(now time.Time, t *metav1.Time) time.Duration {
	if t == nil || !t.After(now) {
		return 0
	}
	return t.Sub(now) + time.Second
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// consumer returns a current consumer of the record and its executor
// profile: the compiler pulls the application with that SpinApp's
// credentials and runs where its replicas run. Consumers are tried in name
// order, starting at a different one each attempt, so one SpinApp with
// broken pull credentials does not keep every attempt failing. nil when
// none still asks for this record.
func (r *RecordReconciler) consumer(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache, attempt int32) (*spinv1alpha1.SpinApp, *executor.Profile, error) {
	names := []string{}
	for _, c := range rec.Status.Consumers {
		if c.Current {
			names = append(names, c.Name)
		}
	}
	sort.Strings(names)
	for i := range names {
		name := names[(i+int(attempt))%len(names)]
		var app spinv1alpha1.SpinApp
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: rec.Spec.Namespace, Name: name}, &app); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, nil, err
		}
		var e spinv1alpha1.SpinAppExecutor
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Spec.Executor}, &e); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, nil, err
		}
		if !executor.IsManaged(&e) {
			continue
		}
		p, err := executor.Parse(&e, r.Defaults)
		if err != nil || !compiledcache.Requested(&app, p) {
			continue
		}
		id, skip := compiledcache.Derive(&app, p, r.Settings)
		if skip != nil || !id.Matches(rec) {
			continue
		}
		return &app, p, nil
	}
	return nil, nil, nil
}

// compileKeys are the JSON fields of CompileStatus (a test keeps the list
// complete). A merge patch only replaces the fields it names, so cleared
// fields are sent as null.
var compileKeys = []string{"attempt", "token", "sandbox", "specHash", "startedAt", "finishedAt", "artifact", "size",
	"failures", "lastFailureTime", "retryAfter", "reason", "message"}

// patchCompile writes status.compile, with the record's resourceVersion so
// a concurrent change is detected.
func (r *RecordReconciler) patchCompile(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache, c *cachev1alpha1.CompileStatus) error {
	obj, err := compiledcache.ReplaceObject(c, compileKeys)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": rec.ResourceVersion},
		"status":   map[string]any{"compile": obj},
	})
	if err != nil {
		return err
	}
	if err := r.Client.Status().Patch(ctx, rec, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("patch SpinCompiledCache %s compile status: %w", rec.Name, err)
	}
	rec.Status.Compile = c.DeepCopy()
	return nil
}

func (r *RecordReconciler) event(rec *cachev1alpha1.SpinCompiledCache, eventType, reason, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(rec, nil, eventType, reason, "Compile", "%s", truncate(fmt.Sprintf(note, args...), 1000))
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
