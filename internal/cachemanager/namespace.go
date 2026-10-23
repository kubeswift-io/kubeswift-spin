// Package cachemanager is the kubeswift-spin cache manager: it records which
// compiled caches the SpinApps of opted-in executors need, compiles them in
// isolated SwiftSandboxes, prepares signed caches on warm-pool nodes, and
// removes records nobody needs (ADR 0014). It runs as its own Deployment and
// ServiceAccount; the SpinApp controller only reads its records.
package cachemanager

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/compatibility"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
)

// maxPools bounds status.pools.
const maxPools = 16

// NamespaceReconciler keeps, for one namespace at a time, one
// SpinCompiledCache record per lookup identity that a SpinApp of the
// namespace needs, the records' consumer lists, and deletes records nobody
// needs any more. Keying by namespace makes every decision from a complete
// view of the namespace's SpinApps and records.
type NamespaceReconciler struct {
	Client client.Client
	// APIReader reads a record that already exists past the cache.
	APIReader client.Reader
	Defaults  executor.Defaults
	// Options are the controller's compatibility options: a SpinApp the
	// controller refuses to run gets no compiled cache either.
	Options  compatibility.Options
	Settings compiledcache.Settings
	// Delivery decides which SpinApps can get a cache at all (Serves).
	Delivery compiledcache.Delivery
	Detector capabilities.SandboxDetector
	// Retention is how long a record nobody uses as its current cache is
	// kept, from the moment it stopped being used. The most recently
	// replaced record of each existing SpinApp is kept regardless, so a
	// rollback finds its cache.
	Retention time.Duration
	// MaxRecords bounds the records of one namespace: records are created in
	// the kubeswift-spin namespace, outside the tenant's quotas.
	MaxRecords int
	Now        func() time.Time
}

// SetupWithManager registers the reconciler.
func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Retention == 0 {
		r.Retention = 24 * time.Hour
	}
	if r.MaxRecords == 0 {
		r.MaxRecords = 32
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	toNamespace := func(_ context.Context, o client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace()}}}
	}
	recordToNamespace := func(_ context.Context, o client.Object) []reconcile.Request {
		rec, ok := o.(*cachev1alpha1.SpinCompiledCache)
		if !ok || rec.Spec.Namespace == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: rec.Spec.Namespace}}}
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("compiledcache-namespace").
		Watches(&spinv1alpha1.SpinApp{}, handler.EnqueueRequestsFromMapFunc(toNamespace),
			builder.WithPredicates(predicate.Or[client.Object](predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&spinv1alpha1.SpinAppExecutor{}, handler.EnqueueRequestsFromMapFunc(toNamespace),
			builder.WithPredicates(predicate.Or[client.Object](predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}, predicate.LabelChangedPredicate{}))).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetName()}}}
		}), builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Watches(&cachev1alpha1.SpinCompiledCache{}, handler.EnqueueRequestsFromMapFunc(recordToNamespace),
			builder.WithPredicates(predicate.Funcs{
				// Only creation and deletion change what the namespace
				// needs; status updates are the record reconciler's.
				UpdateFunc: func(event.UpdateEvent) bool { return false },
			})).
		Complete(r)
}

// want is what one SpinApp needs.
type want struct {
	id   compiledcache.Identity
	pool string
}

// view is what a namespace's SpinApps need.
type view struct {
	// wants is the identity each SpinApp that gets a cache needs.
	wants map[string]want
	// apps are the names of all SpinApps of the namespace.
	apps map[string]bool
	// unknown are SpinApps whose executor is missing or invalid; their
	// consumer entries are left as they are rather than marked replaced.
	unknown map[string]bool
}

// Reconcile converges the records of one namespace.
func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ns := req.Namespace
	logger := log.FromContext(ctx).WithValues("namespace", ns)
	ctx = log.IntoContext(ctx, logger)
	now := r.Now()

	v, err := r.view(ctx, ns)
	if err != nil {
		return ctrl.Result{}, err
	}
	var list cachev1alpha1.SpinCompiledCacheList
	if err := r.Client.List(ctx, &list, client.InNamespace(r.Settings.Namespace),
		client.MatchingLabels{compiledcache.LabelNamespace: ns}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list SpinCompiledCaches: %w", err)
	}
	records := map[string]*cachev1alpha1.SpinCompiledCache{}
	for i := range list.Items {
		records[list.Items[i].Name] = &list.Items[i]
	}

	// Create the records that are needed and missing. A record that cannot
	// be created is logged and skipped; it never stops the rest of the
	// namespace from converging.
	retry := false
	for _, w := range sortedWants(v.wants) {
		name := w.id.RecordName()
		if _, ok := records[name]; ok {
			continue
		}
		if len(records) >= r.MaxRecords {
			// Make room by deleting the oldest record nobody needs (not in
			// use, not a rollback target), even before its retention ends:
			// a SpinApp's current version comes first.
			victim := oldestUnneeded(records, v.apps)
			if victim == "" {
				logger.Info("namespace reached its compiled cache record limit; not recording more", "limit", r.MaxRecords, "source", w.id.Source)
				break
			}
			if err := r.delete(ctx, records[victim]); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("deleted the oldest unused compiled cache record to stay within the limit", "record", victim, "limit", r.MaxRecords)
			delete(records, victim)
		}
		rec, err := r.create(ctx, ns, w.id)
		if err != nil {
			logger.Error(err, "cannot record a compiled cache request", "record", name, "source", w.id.Source)
			retry = true
			continue
		}
		if rec != nil {
			records[name] = rec
		}
	}

	// Update consumers, pools and reference times.
	for name, rec := range records {
		consumers, pools, referenced := Consumers(rec, name, v, now)
		last := rec.Status.LastReferencedAt
		switch {
		case referenced && (last == nil || now.Sub(last.Time) >= time.Hour || !sameConsumers(rec.Status.Consumers, consumers)):
			// While referenced, the time is refreshed when consumers change
			// and at most hourly otherwise.
			last = &metav1.Time{Time: now}
		case !referenced && inUse(rec):
			// It stopped being used now: retention starts now.
			last = &metav1.Time{Time: now}
		case last == nil:
			last = &metav1.Time{Time: rec.CreationTimestamp.Time}
		}
		if sameConsumers(rec.Status.Consumers, consumers) && slices.Equal(rec.Status.Pools, pools) && last.Equal(rec.Status.LastReferencedAt) {
			continue
		}
		patch, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": rec.ResourceVersion},
			"status":   map[string]any{"consumers": consumers, "pools": pools, "lastReferencedAt": last},
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Client.Status().Patch(ctx, rec, client.RawPatch(types.MergePatchType, patch)); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
			}
			return ctrl.Result{}, fmt.Errorf("patch SpinCompiledCache %s consumers: %w", name, err)
		}
		rec.Status.Consumers, rec.Status.Pools, rec.Status.LastReferencedAt = consumers, pools, last
	}

	// Delete what nobody needs.
	for _, name := range Expired(records, v.apps, r.Retention, now) {
		rec := records[name]
		if err := r.delete(ctx, rec); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("deleted unused compiled cache record", "record", name, "source", rec.Spec.Source)
		delete(records, name)
	}

	// Records kept only for their retention period are reconsidered when it
	// ends; records in use are revisited hourly to refresh their reference
	// time.
	requeue := time.Hour
	for _, rec := range records {
		if inUse(rec) {
			continue
		}
		if until := retainedUntil(rec, r.Retention); until.After(now) {
			requeue = min(requeue, until.Sub(now)+time.Second)
		}
	}
	if retry {
		return ctrl.Result{}, fmt.Errorf("some compiled cache requests of namespace %s could not be recorded", ns)
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// delete deletes a record if it is still the version this decision was
// made on.
func (r *NamespaceReconciler) delete(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache) error {
	uid, rv := rec.UID, rec.ResourceVersion
	if err := r.Client.Delete(ctx, rec, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("delete SpinCompiledCache %s: %w", rec.Name, err)
	}
	return nil
}

// oldestUnneeded returns the record nobody needs that was used longest ago,
// or "".
func oldestUnneeded(records map[string]*cachev1alpha1.SpinCompiledCache, apps map[string]bool) string {
	// Expired with no retention is exactly the set nobody needs.
	victim := ""
	var at time.Time
	for _, name := range Expired(records, apps, 0, time.Now().Add(time.Hour)) {
		t := retainedUntil(records[name], 0)
		if victim == "" || t.Before(at) {
			victim, at = name, t
		}
	}
	return victim
}

// create creates the record of an identity. It returns nil, nil when a
// record of that name exists but describes another identity: such an object
// was not made by the cache manager and is left alone.
func (r *NamespaceReconciler) create(ctx context.Context, ns string, id compiledcache.Identity) (*cachev1alpha1.SpinCompiledCache, error) {
	rec := &cachev1alpha1.SpinCompiledCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      id.RecordName(),
			Namespace: r.Settings.Namespace,
			Labels: map[string]string{
				compiledcache.LabelNamespace:   ns,
				"app.kubernetes.io/managed-by": "kubeswift-spin",
			},
		},
		Spec: id.Spec(),
	}
	err := r.Client.Create(ctx, rec)
	if err == nil {
		log.FromContext(ctx).Info("recorded compiled cache request", "record", rec.Name, "source", id.Source)
		return rec, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create SpinCompiledCache %s: %w", rec.Name, err)
	}
	var existing cachev1alpha1.SpinCompiledCache
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(rec), &existing); err != nil {
		return nil, err
	}
	if !id.Matches(&existing) || existing.Labels[compiledcache.LabelNamespace] != ns {
		log.FromContext(ctx).Info("a SpinCompiledCache with this identity's name describes something else; leaving it alone", "record", rec.Name)
		return nil, nil
	}
	return &existing, nil
}

func sortedWants(m map[string]want) []want {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]want, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// view reads what the namespace's SpinApps need. A SpinApp gets a cache only
// when the controller would run it: its executor is a valid kubeswift-spin
// executor and compatibility finds nothing blocking.
func (r *NamespaceReconciler) view(ctx context.Context, ns string) (view, error) {
	v := view{wants: map[string]want{}, apps: map[string]bool{}, unknown: map[string]bool{}}
	var apps spinv1alpha1.SpinAppList
	if err := r.Client.List(ctx, &apps, client.InNamespace(ns)); err != nil {
		return v, fmt.Errorf("list SpinApps: %w", err)
	}
	var nsObj corev1.Namespace
	if err := r.Client.Get(ctx, types.NamespacedName{Name: ns}, &nsObj); err != nil && !apierrors.IsNotFound(err) {
		return v, fmt.Errorf("read namespace: %w", err)
	}
	enabled := compiledcache.NamespaceEnabledFor(nsObj.Labels)
	features := capabilities.Sandbox{}
	if r.Detector != nil {
		features, _ = r.Detector.Sandbox(ctx)
	}
	opts := r.Options
	opts.Features = features
	type parsed struct {
		p       *executor.Profile
		unknown bool
	}
	execs := map[string]parsed{}
	for i := range apps.Items {
		app := &apps.Items[i]
		v.apps[app.Name] = true
		// SpinApps scaled to zero run nothing to speed up.
		if !enabled || !app.DeletionTimestamp.IsZero() || app.Spec.Executor == "" || app.Spec.Replicas == 0 {
			continue
		}
		e, ok := execs[app.Spec.Executor]
		if !ok {
			p, unknown, err := r.profile(ctx, ns, app.Spec.Executor)
			if err != nil {
				return v, err
			}
			e = parsed{p, unknown}
			execs[app.Spec.Executor] = e
		}
		if e.unknown {
			v.unknown[app.Name] = true
			continue
		}
		if e.p == nil || !compiledcache.Requested(app, e.p) || !features.CompiledCache() {
			continue
		}
		if compatibility.Blocking(compatibility.Analyze(app, e.p, opts)) {
			continue
		}
		id, skip := compiledcache.Derive(app, e.p, r.Settings)
		if skip != nil || !r.Delivery.Serves(e.p) {
			continue
		}
		v.wants[app.Name] = want{id: *id, pool: e.p.SandboxPool}
	}
	return v, nil
}

// profile returns the parsed profile of a kubeswift-spin executor, nil for
// an executor kubeswift-spin does not manage, and unknown for one that is
// missing or invalid: the controller keeps such SpinApps' replicas running,
// so their caches are kept too.
func (r *NamespaceReconciler) profile(ctx context.Context, ns, name string) (*executor.Profile, bool, error) {
	var e spinv1alpha1.SpinAppExecutor
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &e); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("read SpinAppExecutor %s: %w", name, err)
	}
	if !executor.IsManaged(&e) {
		return nil, false, nil
	}
	p, err := executor.Parse(&e, r.Defaults)
	if err != nil {
		return nil, true, nil
	}
	return p, false, nil
}

// Consumers computes the consumer list and pools of record name. A SpinApp
// that wanted the record and now wants another one, or has been deleted,
// stays listed as not current, with the time it moved away; a SpinApp whose
// executor is missing or invalid keeps its entry unchanged. The bool reports
// whether any SpinApp uses the record as its current cache.
func Consumers(rec *cachev1alpha1.SpinCompiledCache, name string, v view, now time.Time) ([]cachev1alpha1.Consumer, []string, bool) {
	byName := map[string]cachev1alpha1.Consumer{}
	for _, c := range rec.Status.Consumers {
		byName[c.Name] = c
	}
	referenced := false
	pools := map[string]bool{}
	for app, w := range v.wants {
		if w.id.RecordName() != name {
			continue
		}
		referenced = true
		byName[app] = cachev1alpha1.Consumer{Name: app, Current: true, Pool: w.pool}
		if w.pool != "" {
			pools[w.pool] = true
		}
	}
	for app, c := range byName {
		if v.unknown[app] {
			if c.Current {
				referenced = true
				if c.Pool != "" {
					pools[c.Pool] = true
				}
			}
			continue
		}
		w, wanted := v.wants[app]
		if c.Current && (!wanted || w.id.RecordName() != name) {
			c.Current = false
			c.Pool = ""
			c.ReplacedAt = &metav1.Time{Time: now}
			byName[app] = c
		}
		if !v.apps[app] && !c.Current {
			// A deleted SpinApp needs no rollback target.
			delete(byName, app)
		}
	}
	out := make([]cachev1alpha1.Consumer, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Current != out[j].Current {
			return out[i].Current
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > cachev1alpha1.MaxConsumers {
		out = out[:cachev1alpha1.MaxConsumers]
	}
	poolList := make([]string, 0, len(pools))
	for p := range pools {
		poolList = append(poolList, p)
	}
	sort.Strings(poolList)
	if len(poolList) > maxPools {
		poolList = poolList[:maxPools]
	}
	return out, poolList, referenced
}

func sameConsumers(a, b []cachev1alpha1.Consumer) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Name != y.Name || x.Current != y.Current || x.Pool != y.Pool || !x.ReplacedAt.Equal(y.ReplacedAt) {
			return false
		}
	}
	return true
}

func inUse(rec *cachev1alpha1.SpinCompiledCache) bool {
	for _, c := range rec.Status.Consumers {
		if c.Current {
			return true
		}
	}
	return false
}

func retainedUntil(rec *cachev1alpha1.SpinCompiledCache, retention time.Duration) time.Time {
	t := rec.CreationTimestamp.Time
	if rec.Status.LastReferencedAt != nil {
		t = rec.Status.LastReferencedAt.Time
	}
	return t.Add(retention)
}

// Expired returns the records to delete: nobody uses them as their current
// cache, they are not the most recently replaced record of an existing
// SpinApp (its rollback target), and their retention period has passed.
func Expired(records map[string]*cachev1alpha1.SpinCompiledCache, apps map[string]bool, retention time.Duration, now time.Time) []string {
	previous := map[string]string{} // SpinApp -> record replaced most recently
	replacedAt := map[string]time.Time{}
	for name, rec := range records {
		for _, c := range rec.Status.Consumers {
			if c.Current || c.ReplacedAt == nil || !apps[c.Name] {
				continue
			}
			if t := c.ReplacedAt.Time; t.After(replacedAt[c.Name]) || (t.Equal(replacedAt[c.Name]) && name < previous[c.Name]) {
				previous[c.Name], replacedAt[c.Name] = name, t
			}
		}
	}
	keep := map[string]bool{}
	for _, name := range previous {
		keep[name] = true
	}
	var out []string
	for name, rec := range records {
		if inUse(rec) || keep[name] || !rec.DeletionTimestamp.IsZero() {
			continue
		}
		if retainedUntil(rec, retention).After(now) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
