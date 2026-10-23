package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

const (
	ccDigestA       = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ccDigestB       = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	ccRuntimeDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	ccRuntime       = "ghcr.io/kubeswift-io/kubeswift-spin-runtime@" + ccRuntimeDigest
	ccAppRepo       = "ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http"
)

func cacheSettings(t *testing.T) compiledcache.Settings {
	return compiledcache.Settings{Namespace: newNamespace(t), Repository: "registry.example.com/kubeswift-spin-cache", Architecture: "amd64",
		Runtimes: []string{ccRuntime}}
}

// enabledNamespace creates a namespace an administrator enabled for
// compiled caches.
func enabledNamespace(t *testing.T) string {
	t.Helper()
	ns := newNamespace(t)
	setNamespaceLabel(t, ns, compiledcache.NamespaceEnabled)
	return ns
}

func setNamespaceLabel(t *testing.T, ns, value string) {
	t.Helper()
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, compiledcache.NamespaceLabel, value))
	if value == "" {
		patch = []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:null}}}`, compiledcache.NamespaceLabel))
	}
	if err := testClient.Patch(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatal(err)
	}
}

func cacheExecutor(ns string, enabled bool) *spinv1alpha1.SpinAppExecutor {
	ann := map[string]string{executor.AnnRuntimeImage: ccRuntime}
	if enabled {
		ann[executor.AnnCompiledCache] = executor.CompiledCacheEnabled
	}
	return managedExecutor(ns, "kubeswift", ann)
}

func pinnedApp(ns, name, digest string) *spinv1alpha1.SpinApp {
	a := spinApp(ns, name, "kubeswift", 1)
	a.Spec.Image = ccAppRepo + "@" + digest
	return a
}

// listRecords lists the records in ns for SpinApps of namespace forNS. The
// cache manager watches every namespace, so SpinApps that earlier tests left
// behind get records too.
func listRecords(t *testing.T, ns, forNS string) []cachev1alpha1.SpinCompiledCache {
	t.Helper()
	var l cachev1alpha1.SpinCompiledCacheList
	if err := testClient.List(context.Background(), &l, client.InNamespace(ns), client.MatchingLabels{compiledcache.LabelNamespace: forNS}); err != nil {
		t.Fatal(err)
	}
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Spec.Source < l.Items[j].Spec.Source })
	return l.Items
}

func TestCompiledCacheNotRequested(t *testing.T) {
	s := cacheSettings(t)
	startManager(t, harnessOpts{compiledCache: s, cacheManager: true})
	ns := enabledNamespace(t)
	mustCreate(t, cacheExecutor(ns, false), pinnedApp(ns, "hello", ccDigestA))
	expectSandboxes(t, ns, "hello-0")
	consistently(t, "no condition and no record", 2*time.Second, func() error {
		if c := findCond(getApp(t, ns, "hello"), compiledcache.TypeCompiledCache); c != nil {
			return fmt.Errorf("unexpected condition %+v", c)
		}
		if r := listRecords(t, s.Namespace, ns); len(r) != 0 {
			return fmt.Errorf("unexpected records %d", len(r))
		}
		return nil
	})
}

func TestCompiledCacheSkips(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	mustCreate(t, cacheExecutor(ns, true), pinnedApp(ns, "hello", ccDigestA))
	// The platform has not configured compiled caches: the SpinApp runs and
	// says why it has none.
	expectSandboxes(t, ns, "hello-0")
	expectCondition(t, ns, "hello", compiledcache.TypeCompiledCache, metav1.ConditionFalse, compiledcache.ReasonNotConfigured)
	expectEvent(t, ns, "hello", EventCompiledCacheSkipped)
}

func TestCompiledCacheTaggedImage(t *testing.T) {
	s := cacheSettings(t)
	startManager(t, harnessOpts{compiledCache: s, cacheManager: true})
	ns := enabledNamespace(t)
	app := spinApp(ns, "hello", "kubeswift", 1) // tagged image
	mustCreate(t, cacheExecutor(ns, true), app)
	expectSandboxes(t, ns, "hello-0")
	expectCondition(t, ns, "hello", compiledcache.TypeCompiledCache, metav1.ConditionFalse, compiledcache.ReasonImageNotPinned)
	consistently(t, "no record for a tag", time.Second, func() error {
		if r := listRecords(t, s.Namespace, ns); len(r) != 0 {
			return fmt.Errorf("unexpected records %d", len(r))
		}
		return nil
	})
	// Available does not depend on the cache.
	setReady(t, ns, "hello-0")
	expectCondition(t, ns, "hello", "Available", metav1.ConditionTrue, "ApplicationReady")
}

func TestCompiledCacheRecordLifecycle(t *testing.T) {
	s := cacheSettings(t)
	startManager(t, harnessOpts{compiledCache: s, cacheManager: true})
	ns := enabledNamespace(t)
	mustCreate(t, cacheExecutor(ns, true), pinnedApp(ns, "one", ccDigestA), pinnedApp(ns, "two", ccDigestA))

	// Two SpinApps with one identity share one record.
	var name string
	eventually(t, "one record with two consumers", func() error {
		recs := listRecords(t, s.Namespace, ns)
		if len(recs) != 1 {
			return fmt.Errorf("%d records", len(recs))
		}
		r := recs[0]
		name = r.Name
		want := compiledcache.Identity{Namespace: ns, Source: ccAppRepo + "@" + ccDigestA, Runtime: ccRuntime, Architecture: "amd64",
			Repository: s.Repository + "/" + ns + "/wasmtime-cache"}
		if r.Name != want.RecordName() || !want.Matches(&r) {
			return fmt.Errorf("record %s: %+v", r.Name, r.Spec)
		}
		if len(r.Status.Consumers) != 2 || !r.Status.Consumers[0].Current || !r.Status.Consumers[1].Current {
			return fmt.Errorf("consumers %+v", r.Status.Consumers)
		}
		return nil
	})
	expectCondition(t, ns, "one", compiledcache.TypeCompiledCache, metav1.ConditionFalse, compiledcache.ReasonCompiling)
	expectEvent(t, ns, "one", EventCompiledCacheRequested)

	// The signer's and compiler's results make it usable.
	art := s.Repository + "/" + ns + "/wasmtime-cache@" + ccDigestB
	patchRecordStatus(t, s.Namespace, name, map[string]any{
		"compile":   map[string]any{"attempt": 1, "artifact": art},
		"signature": map[string]any{"artifact": art, "keyID": "k1", "signedAt": time.Now().UTC().Format(time.RFC3339)},
	})
	expectCondition(t, ns, "one", compiledcache.TypeCompiledCache, metav1.ConditionTrue, compiledcache.ReasonReady)
	expectEvent(t, ns, "two", EventCompiledCacheReady)

	// one moves to digest B: a second record; the first keeps two as a
	// current consumer and one as replaced.
	updateApp(t, ns, "one", func(a *spinv1alpha1.SpinApp) { a.Spec.Image = ccAppRepo + "@" + ccDigestB })
	eventually(t, "a record per digest", func() error {
		recs := listRecords(t, s.Namespace, ns)
		if len(recs) != 2 {
			return fmt.Errorf("%d records", len(recs))
		}
		var first, second *cachev1alpha1.SpinCompiledCache
		for i := range recs {
			if recs[i].Name == name {
				first = &recs[i]
			} else {
				second = &recs[i]
			}
		}
		if first == nil {
			return fmt.Errorf("first record gone")
		}
		cur := map[string]bool{}
		for _, c := range first.Status.Consumers {
			cur[c.Name] = c.Current
		}
		if !cur["two"] || cur["one"] {
			return fmt.Errorf("consumers of the first record %+v", first.Status.Consumers)
		}
		// The second record must list its consumer too: a record that was
		// never in use keeps the reference time the test backdates below and
		// would expire at once.
		if len(second.Status.Consumers) != 1 || second.Status.Consumers[0].Name != "one" || !second.Status.Consumers[0].Current {
			return fmt.Errorf("consumers of the second record %+v", second.Status.Consumers)
		}
		return nil
	})
	expectCondition(t, ns, "one", compiledcache.TypeCompiledCache, metav1.ConditionFalse, compiledcache.ReasonCompiling)

	// Deleting both SpinApps keeps records for the retention period from
	// the moment they stopped being used (one hour in this harness), even if
	// they were last referenced long ago.
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	for _, r := range listRecords(t, s.Namespace, ns) {
		patchRecordStatus(t, s.Namespace, r.Name, map[string]any{"lastReferencedAt": old})
	}
	for _, n := range []string{"one", "two"} {
		if err := testClient.Delete(context.Background(), &spinv1alpha1.SpinApp{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "no current consumers", func() error {
		for _, r := range listRecords(t, s.Namespace, ns) {
			for _, c := range r.Status.Consumers {
				return fmt.Errorf("record %s still lists %+v", r.Name, c)
			}
		}
		return nil
	})
	if n := len(listRecords(t, s.Namespace, ns)); n != 2 {
		t.Fatalf("records deleted before their retention: %d left", n)
	}
}

func TestCompiledCacheOptOut(t *testing.T) {
	s := cacheSettings(t)
	startManager(t, harnessOpts{compiledCache: s, cacheManager: true})
	ns := enabledNamespace(t)
	app := pinnedApp(ns, "hello", ccDigestA)
	app.Annotations = map[string]string{executor.AnnCompiledCache: executor.CompiledCacheDisabled}
	mustCreate(t, cacheExecutor(ns, true), app)
	expectSandboxes(t, ns, "hello-0")
	consistently(t, "opted out", 2*time.Second, func() error {
		if c := findCond(getApp(t, ns, "hello"), compiledcache.TypeCompiledCache); c != nil {
			return fmt.Errorf("unexpected condition %+v", c)
		}
		if r := listRecords(t, s.Namespace, ns); len(r) != 0 {
			return fmt.Errorf("unexpected records %d", len(r))
		}
		return nil
	})
}

// A record whose spec does not match its name was not written by the cache
// manager; the controller never treats it as the SpinApp's cache.
func TestCompiledCacheMismatchedRecordIgnored(t *testing.T) {
	s := cacheSettings(t)
	startManager(t, harnessOpts{compiledCache: s}) // no cache manager
	ns := enabledNamespace(t)
	repo := s.NamespaceRepository(ns)
	id := compiledcache.Identity{Namespace: ns, Source: ccAppRepo + "@" + ccDigestA, Runtime: ccRuntime, Architecture: "amd64", Repository: repo}
	forged := &cachev1alpha1.SpinCompiledCache{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: id.RecordName()},
		Spec:       compiledcache.Identity{Namespace: ns, Source: ccAppRepo + "@" + ccDigestB, Runtime: ccRuntime, Architecture: "amd64", Repository: repo}.Spec(),
	}
	mustCreate(t, forged)
	art := s.Repository + "/" + ns + "/wasmtime-cache@" + ccDigestB
	patchRecordStatus(t, s.Namespace, forged.Name, map[string]any{
		"compile":   map[string]any{"artifact": art},
		"signature": map[string]any{"artifact": art, "signedAt": time.Now().UTC().Format(time.RFC3339)},
		"consumers": []any{map[string]any{"name": "hello", "current": true}},
	})
	mustCreate(t, cacheExecutor(ns, true), pinnedApp(ns, "hello", ccDigestA))
	expectCondition(t, ns, "hello", compiledcache.TypeCompiledCache, metav1.ConditionFalse, compiledcache.ReasonRequested)
}

func patchRecordStatus(t *testing.T, ns, name string, status map[string]any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		t.Fatal(err)
	}
	rec := &cachev1alpha1.SpinCompiledCache{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if err := testClient.Status().Patch(context.Background(), rec, client.RawPatch(types.MergePatchType, b)); err != nil {
		t.Fatal(err)
	}
}

func TestCompiledCacheNamespaceNotEnabled(t *testing.T) {
	s := cacheSettings(t)
	startManager(t, harnessOpts{compiledCache: s, cacheManager: true})
	ns := newNamespace(t)
	mustCreate(t, cacheExecutor(ns, true), pinnedApp(ns, "hello", ccDigestA))
	expectCondition(t, ns, "hello", compiledcache.TypeCompiledCache, metav1.ConditionFalse, compiledcache.ReasonNamespaceNotEnabled)
	consistently(t, "no record without the namespace label", time.Second, func() error {
		if r := listRecords(t, s.Namespace, ns); len(r) != 0 {
			return fmt.Errorf("unexpected records %d", len(r))
		}
		return nil
	})
	// An administrator enables the namespace.
	setNamespaceLabel(t, ns, compiledcache.NamespaceEnabled)
	expectCondition(t, ns, "hello", compiledcache.TypeCompiledCache, metav1.ConditionFalse, compiledcache.ReasonCompiling)
	eventually(t, "a record", func() error {
		if r := listRecords(t, s.Namespace, ns); len(r) != 1 {
			return fmt.Errorf("%d records", len(r))
		}
		return nil
	})
}

func TestCompiledCacheRuntimeNotAllowed(t *testing.T) {
	s := cacheSettings(t)
	startManager(t, harnessOpts{compiledCache: s, cacheManager: true})
	ns := enabledNamespace(t)
	exec := cacheExecutor(ns, true)
	exec.Annotations[executor.AnnRuntimeImage] = "ghcr.io/example/other-runtime@" + ccDigestB
	mustCreate(t, exec, pinnedApp(ns, "hello", ccDigestA))
	expectSandboxes(t, ns, "hello-0")
	expectCondition(t, ns, "hello", compiledcache.TypeCompiledCache, metav1.ConditionFalse, compiledcache.ReasonRuntimeNotAllowed)
	consistently(t, "no record for an executor-chosen runtime", time.Second, func() error {
		if r := listRecords(t, s.Namespace, ns); len(r) != 0 {
			return fmt.Errorf("unexpected records %d", len(r))
		}
		return nil
	})
}

// A trusted cache is added to replicas created after it became trusted,
// outside the revision: existing replicas are not replaced.
func TestCompiledCacheDecoratesNewReplicas(t *testing.T) {
	s := cacheSettings(t)
	startManager(t, harnessOpts{compiledCache: s, delivery: compiledcache.Delivery{PullSecret: "cache-read", ColdFetchTimeout: 3 * time.Second}})
	ns := enabledNamespace(t)
	mustCreate(t, cacheExecutor(ns, true), pinnedApp(ns, "one", ccDigestA))
	expectSandboxes(t, ns, "one-0")
	first := getSandbox(t, ns, "one-0")
	if len(first.Spec.Artifacts) != 0 {
		t.Fatalf("a replica got a cache before one was trusted: %+v", first.Spec.Artifacts)
	}

	id := compiledcache.Identity{Namespace: ns, Source: ccAppRepo + "@" + ccDigestA, Runtime: ccRuntime, Architecture: "amd64",
		Repository: s.NamespaceRepository(ns)}
	mustCreate(t, &cachev1alpha1.SpinCompiledCache{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: id.RecordName()}, Spec: id.Spec()})
	art := id.Repository + "@" + ccDigestB
	patchRecordStatus(t, s.Namespace, id.RecordName(), map[string]any{
		"compile":   map[string]any{"attempt": 1, "artifact": art},
		"signature": map[string]any{"artifact": art, "keyID": "k1", "signedAt": time.Now().UTC().Format(time.RFC3339)},
	})
	expectCondition(t, ns, "one", compiledcache.TypeCompiledCache, metav1.ConditionTrue, compiledcache.ReasonReady)

	updateApp(t, ns, "one", func(a *spinv1alpha1.SpinApp) { a.Spec.Replicas = 2 })
	expectSandboxes(t, ns, "one-0", "one-1")
	second := getSandbox(t, ns, "one-1")
	if second.Labels[translate.LabelRevision] != first.Labels[translate.LabelRevision] {
		t.Fatalf("the cache changed the revision: %s, %s", second.Labels[translate.LabelRevision], first.Labels[translate.LabelRevision])
	}
	if got := getSandbox(t, ns, "one-0"); got.UID != first.UID || len(got.Spec.Artifacts) != 0 {
		t.Fatal("the existing replica was replaced or changed")
	}
	if len(second.Spec.Artifacts) != 1 {
		t.Fatalf("artifacts %+v", second.Spec.Artifacts)
	}
	a := second.Spec.Artifacts[0]
	if a.Name != compiledcache.ArtifactName || a.Ref != art || a.MountPath != runtimecontract.CompiledCacheDir || a.Layout != "unpacked" ||
		!a.Optional || !a.RequirePlatformVerification || a.FetchTimeout == nil || a.FetchTimeout.Duration != 3*time.Second ||
		a.PullSecretRef == nil || a.PullSecretRef.Name != "cache-read" || a.Claims[compiledcache.ClaimNamespace] != ns {
		t.Fatalf("artifact %+v", a)
	}
	env := map[string]string{}
	for _, e := range second.Spec.Env {
		env[e.Name] = e.Value
	}
	if env[runtimecontract.CompiledCacheSourceEnv] != id.Source || env[runtimecontract.CompiledCacheRuntimeEnv] != ccRuntime ||
		env[runtimecontract.CompiledCacheArchitectureEnv] != "amd64" {
		t.Fatalf("env %v", env)
	}
}

// KubeSwift v0.19.1 fails a sandbox whose optional artifact names a pull
// Secret it cannot read. The cache must not keep the SpinApp from
// recovering: replacements start without it, and the condition says why.
func TestCompiledCacheHoldAfterDeliveryFailure(t *testing.T) {
	s := cacheSettings(t)
	startManager(t, harnessOpts{compiledCache: s, delivery: compiledcache.Delivery{PullSecret: "missing", ColdFetchTimeout: time.Second}})
	ns := enabledNamespace(t)
	id := compiledcache.Identity{Namespace: ns, Source: ccAppRepo + "@" + ccDigestA, Runtime: ccRuntime, Architecture: "amd64",
		Repository: s.NamespaceRepository(ns)}
	mustCreate(t, &cachev1alpha1.SpinCompiledCache{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: id.RecordName()}, Spec: id.Spec()})
	art := id.Repository + "@" + ccDigestB
	patchRecordStatus(t, s.Namespace, id.RecordName(), map[string]any{
		"compile":   map[string]any{"attempt": 1, "artifact": art},
		"signature": map[string]any{"artifact": art, "keyID": "k1", "signedAt": time.Now().UTC().Format(time.RFC3339)},
	})
	mustCreate(t, cacheExecutor(ns, true), pinnedApp(ns, "one", ccDigestA))
	expectSandboxes(t, ns, "one-0")
	first := getSandbox(t, ns, "one-0")
	if len(first.Spec.Artifacts) != 1 {
		t.Fatalf("the first replica got no cache: %+v", first.Spec.Artifacts)
	}
	setPhase(t, ns, "one-0", sandboxv1alpha1.SwiftSandboxFailed, metav1.Condition{Type: sandboxv1alpha1.SwiftSandboxConditionGuestRunning,
		Status: metav1.ConditionFalse, Reason: "ArtifactResolveFailed", Message: "artifact wasmtime-cache: pull secret missing: not found"})
	expectCondition(t, ns, "one", compiledcache.TypeCompiledCache, metav1.ConditionFalse, compiledcache.ReasonDeliveryFailed)
	expectEvent(t, ns, "one", EventCompiledCacheFailed)
	eventually(t, "a replacement without the cache", func() error {
		simulateGC(t, ns)
		for _, sb := range listSandboxes(t, ns) {
			if sb.UID != first.UID && sb.DeletionTimestamp.IsZero() {
				if len(sb.Spec.Artifacts) != 0 {
					return fmt.Errorf("replacement %s carries the cache", sb.Name)
				}
				return nil
			}
		}
		return fmt.Errorf("no replacement yet")
	})
}
