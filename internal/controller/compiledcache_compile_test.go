package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

func listCompilers(t *testing.T, ns string) []sandboxv1alpha1.SwiftSandbox {
	t.Helper()
	var l sandboxv1alpha1.SwiftSandboxList
	if err := testClient.List(context.Background(), &l, client.InNamespace(ns),
		client.MatchingLabels{compiledcache.LabelComponent: compiledcache.ComponentCompiler}); err != nil {
		t.Fatal(err)
	}
	var out []sandboxv1alpha1.SwiftSandbox
	for _, sb := range l.Items {
		if sb.DeletionTimestamp.IsZero() {
			out = append(out, sb)
		}
	}
	return out
}

// oneCompiler waits for exactly one live compiler sandbox in ns.
func oneCompiler(t *testing.T, ns string) sandboxv1alpha1.SwiftSandbox {
	t.Helper()
	var sb sandboxv1alpha1.SwiftSandbox
	eventually(t, "one compiler sandbox", func() error {
		l := listCompilers(t, ns)
		if len(l) != 1 {
			return fmt.Errorf("%d compilers", len(l))
		}
		sb = l[0]
		return nil
	})
	return sb
}

func patchSandboxStatus(t *testing.T, sb *sandboxv1alpha1.SwiftSandbox, status map[string]any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		t.Fatal(err)
	}
	if err := testClient.Status().Patch(context.Background(), sb, client.RawPatch(types.MergePatchType, b)); err != nil {
		t.Fatal(err)
	}
}

func getRecord(t *testing.T, ns, name string) *cachev1alpha1.SpinCompiledCache {
	t.Helper()
	var r cachev1alpha1.SpinCompiledCache
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func onlyRecord(t *testing.T, s compiledcache.Settings, ns string) string {
	t.Helper()
	var name string
	eventually(t, "one record", func() error {
		recs := listRecords(t, s.Namespace, ns)
		if len(recs) != 1 {
			return fmt.Errorf("%d records", len(recs))
		}
		name = recs[0].Name
		return nil
	})
	return name
}

func TestCompiledCacheCompileFlow(t *testing.T) {
	s := cacheSettings(t)
	startManager(t, harnessOpts{compiledCache: s, cacheManager: true})
	ns := enabledNamespace(t)
	mustCreate(t, cacheExecutor(ns, true), pinnedApp(ns, "hello", ccDigestA))
	name := onlyRecord(t, s, ns)

	sb := oneCompiler(t, ns)
	var rec *cachev1alpha1.SpinCompiledCache
	eventually(t, "the compiler recorded", func() error {
		rec = getRecord(t, s.Namespace, name)
		if c := rec.Status.Compile; c == nil || c.Sandbox == nil || c.Sandbox.UID != sb.UID {
			return fmt.Errorf("compile %+v", rec.Status.Compile)
		}
		return nil
	})
	if sb.Labels[compiledcache.LabelRecord] != name || sb.Labels[translate.LabelManagedBy] != translate.ManagedByValue {
		t.Fatalf("compiler labels %v", sb.Labels)
	}
	if sb.Spec.Image != ccRuntime || len(sb.Spec.Outputs) != 1 || sb.Spec.Outputs[0].Repository != rec.Spec.Repository || len(sb.Spec.Network.Ports) != 0 {
		t.Fatalf("compiler spec %+v", sb.Spec)
	}
	c := rec.Status.Compile
	if c == nil || c.Sandbox == nil || c.Sandbox.UID != sb.UID || c.SpecHash == "" || c.Attempt != 1 {
		t.Fatalf("compile status %+v", c)
	}
	var stored unstructured.Unstructured
	stored.SetGroupVersionKind(sandboxv1alpha1.GroupVersion.WithKind("SwiftSandbox"))
	if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(&sb), &stored); err != nil {
		t.Fatal(err)
	}
	if err := compiledcache.CheckCompiler(rec, &stored, s.Runtimes); err != nil {
		t.Fatalf("the signer's check rejects the cache manager's own compiler: %v", err)
	}
	expectCondition(t, ns, "hello", compiledcache.TypeCompiledCache, metav1.ConditionFalse, compiledcache.ReasonCompiling)

	// KubeSwift exports the output.
	digest := "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	art := rec.Spec.Repository + "@" + digest
	patchSandboxStatus(t, &sb, map[string]any{"phase": "Completed", "outputs": []any{map[string]any{
		"name": compiledcache.OutputName, "state": "Exported", "repository": rec.Spec.Repository, "reference": art, "digest": digest, "size": 2048}}})
	eventually(t, "the exported artifact", func() error {
		r := getRecord(t, s.Namespace, name)
		if r.Status.Compile == nil || r.Status.Compile.Artifact != art || r.Status.Compile.Size != 2048 {
			return fmt.Errorf("compile %+v", r.Status.Compile)
		}
		return nil
	})
	expectCondition(t, ns, "hello", compiledcache.TypeCompiledCache, metav1.ConditionFalse, compiledcache.ReasonSigning)
	// The signer needs the compiler sandbox: it stays.
	consistently(t, "the compiler stays until signed", time.Second, func() error {
		if len(listCompilers(t, ns)) != 1 {
			return fmt.Errorf("compiler deleted before signing")
		}
		return nil
	})

	// Signed: the compiler is removed and the SpinApp's cache is ready.
	patchRecordStatus(t, s.Namespace, name, map[string]any{"signature": map[string]any{"artifact": art, "keyID": "k", "signedAt": time.Now().UTC().Format(time.RFC3339)}})
	expectCondition(t, ns, "hello", compiledcache.TypeCompiledCache, metav1.ConditionTrue, compiledcache.ReasonReady)
	eventually(t, "the compiler removed", func() error {
		if n := len(listCompilers(t, ns)); n != 0 {
			return fmt.Errorf("%d compilers", n)
		}
		return nil
	})
}

func TestCompiledCacheCompileFailure(t *testing.T) {
	s := cacheSettings(t)
	startManager(t, harnessOpts{compiledCache: s, cacheManager: true})
	ns := enabledNamespace(t)
	mustCreate(t, cacheExecutor(ns, true), pinnedApp(ns, "hello", ccDigestA))
	name := onlyRecord(t, s, ns)
	sb := oneCompiler(t, ns)
	patchSandboxStatus(t, &sb, map[string]any{"phase": "Failed", "outputs": []any{map[string]any{
		"name": compiledcache.OutputName, "state": "Failed", "reason": "OutputDestinationDenied", "message": "no export policy selects the namespace"}}})
	eventually(t, "a recorded failure with backoff", func() error {
		c := getRecord(t, s.Namespace, name).Status.Compile
		if c == nil || c.Failures != 1 || c.Reason != "OutputDestinationDenied" || c.RetryAfter == nil || c.Sandbox != nil || c.Artifact != "" {
			return fmt.Errorf("compile %+v", c)
		}
		if time.Until(c.RetryAfter.Time) < 30*time.Second {
			return fmt.Errorf("retry too soon: %s", c.RetryAfter)
		}
		return nil
	})
	expectCondition(t, ns, "hello", compiledcache.TypeCompiledCache, metav1.ConditionFalse, compiledcache.ReasonCompileFailed)
	expectEvent(t, ns, "hello", EventCompiledCacheFailed)
	// The failed compiler is removed and no new one starts within the backoff.
	consistently(t, "no compiler during the backoff", 2*time.Second, func() error {
		if n := len(listCompilers(t, ns)); n != 0 {
			return fmt.Errorf("%d compilers", n)
		}
		return nil
	})
	// The SpinApp itself is unaffected.
	setReady(t, ns, "hello-0")
	expectCondition(t, ns, "hello", "Available", metav1.ConditionTrue, "ApplicationReady")
}

func TestCompiledCacheCompilerConcurrency(t *testing.T) {
	s := cacheSettings(t)
	startManager(t, harnessOpts{compiledCache: s, cacheManager: true})
	ns := enabledNamespace(t)
	mustCreate(t, cacheExecutor(ns, true), pinnedApp(ns, "one", ccDigestA), pinnedApp(ns, "two", ccDigestB))
	eventually(t, "two records", func() error {
		if n := len(listRecords(t, s.Namespace, ns)); n != 2 {
			return fmt.Errorf("%d records", n)
		}
		return nil
	})
	first := oneCompiler(t, ns)
	consistently(t, "one compiler per namespace", 2*time.Second, func() error {
		if n := len(listCompilers(t, ns)); n != 1 {
			return fmt.Errorf("%d compilers", n)
		}
		return nil
	})
	// When the first finishes, the second starts.
	rec := first.Labels[compiledcache.LabelRecord]
	repo := getRecord(t, s.Namespace, rec).Spec.Repository
	digest := "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	patchSandboxStatus(t, &first, map[string]any{"phase": "Completed", "outputs": []any{map[string]any{
		"name": compiledcache.OutputName, "state": "Exported", "repository": repo, "reference": repo + "@" + digest, "digest": digest}}})
	eventually(t, "the second compiler", func() error {
		for _, sb := range listCompilers(t, ns) {
			if sb.Labels[compiledcache.LabelRecord] != rec {
				return nil
			}
		}
		return fmt.Errorf("no compiler for the second record yet")
	})
}

// A compiler the cache manager created but did not record (it restarted in
// between) is never adopted: it is deleted and a new attempt starts.
func TestCompiledCacheUnrecordedCompilerIsReplaced(t *testing.T) {
	s := cacheSettings(t)
	ns := enabledNamespace(t)
	id := compiledcache.Identity{Namespace: ns, Source: ccAppRepo + "@" + ccDigestA, Runtime: ccRuntime, Architecture: "amd64", Repository: s.NamespaceRepository(ns)}
	rec := &cachev1alpha1.SpinCompiledCache{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: id.RecordName(), Labels: map[string]string{compiledcache.LabelNamespace: ns}},
		Spec:       id.Spec(),
	}
	mustCreate(t, rec)
	patchRecordStatus(t, s.Namespace, rec.Name, map[string]any{
		"compile":   map[string]any{"attempt": 1, "token": "lost"},
		"consumers": []any{map[string]any{"name": "hello", "current": true}},
	})
	stray := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "spin-compile-stray", Labels: compiledcache.CompilerLabels(rec.Name, "lost")},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{Image: ccRuntime, Memory: resource.MustParse("1Gi"),
			Command: []string{runtimecontract.EntrypointPath}, Args: compiledcache.CompilerArgs(rec.Spec),
			Outputs: []sandboxv1alpha1.SandboxOutput{{Name: compiledcache.OutputName, Path: runtimecontract.CompileOutputDir, Repository: rec.Spec.Repository}}},
	}
	mustCreate(t, stray)

	startManager(t, harnessOpts{compiledCache: s, cacheManager: true})
	mustCreate(t, cacheExecutor(ns, true), pinnedApp(ns, "hello", ccDigestA))
	eventually(t, "a new attempt with a recorded compiler", func() error {
		c := getRecord(t, s.Namespace, rec.Name).Status.Compile
		if c == nil || c.Attempt != 2 || c.Sandbox == nil || c.Sandbox.Name == stray.Name {
			return fmt.Errorf("compile %+v", c)
		}
		return nil
	})
	eventually(t, "the stray compiler deleted", func() error {
		var sb sandboxv1alpha1.SwiftSandbox
		err := testClient.Get(context.Background(), client.ObjectKeyFromObject(stray), &sb)
		if err == nil && sb.DeletionTimestamp.IsZero() {
			return fmt.Errorf("still there")
		}
		return nil
	})
}
