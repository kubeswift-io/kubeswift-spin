package cachemanager

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
	"github.com/kubeswift-io/kubeswift-spin/internal/scheme"
)

// TestCompileKeysComplete keeps compileKeys equal to CompileStatus's JSON
// fields: a field missing from the list could never be cleared.
func TestCompileKeysComplete(t *testing.T) {
	var want []string
	typ := reflect.TypeOf(cachev1alpha1.CompileStatus{})
	for i := 0; i < typ.NumField(); i++ {
		want = append(want, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
	}
	got := slices.Clone(compileKeys)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("compileKeys %v, CompileStatus fields %v", got, want)
	}
}

const (
	testRuntime = "ghcr.io/kubeswift-io/kubeswift-spin-runtime@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	testRepo    = "registry.example.com/c/team-a/wasmtime-cache"
)

var testSettings = compiledcache.Settings{Namespace: "kss", Repository: "registry.example.com/c", Architecture: "amd64", Runtimes: []string{testRuntime}}

func exportedRecord(uid types.UID) *cachev1alpha1.SpinCompiledCache {
	r := &cachev1alpha1.SpinCompiledCache{ObjectMeta: metav1.ObjectMeta{Namespace: "kss", Name: "spincc-1"},
		Spec: cachev1alpha1.SpinCompiledCacheSpec{Namespace: "team-a", Source: "ghcr.io/org/app@" + digestA, Runtime: testRuntime,
			Architecture: "amd64", Repository: testRepo}}
	r.Status.Compile = &cachev1alpha1.CompileStatus{Attempt: 2, Token: "t", Sandbox: &cachev1alpha1.SandboxReference{Name: "c", UID: uid},
		SpecHash: "sha256:x", FinishedAt: &metav1.Time{Time: time.Now()}, Artifact: testRepo + "@" + digestB, Failures: 1}
	return r
}

func compilerSandbox(uid types.UID, args []string) *sandboxv1alpha1.SwiftSandbox {
	sb := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "c", UID: uid,
		Labels: compiledcache.CompilerLabels("spincc-1", "t")}}
	sb.Spec.Image, sb.Spec.Command, sb.Spec.Args = testRuntime, []string{runtimecontract.EntrypointPath}, args
	sb.Spec.Outputs = []sandboxv1alpha1.SandboxOutput{{Name: compiledcache.OutputName, Path: runtimecontract.CompileOutputDir}}
	return sb
}

func newRecordReconciler(t *testing.T, now time.Time, objs ...client.Object) (*RecordReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme.New()).WithObjects(objs...).
		WithStatusSubresource(&cachev1alpha1.SpinCompiledCache{}, &sandboxv1alpha1.SwiftSandbox{}).Build()
	return &RecordReconciler{Client: c, APIReader: c, Settings: testSettings, Compiler: compiledcache.DefaultCompilerSettings(),
		MaxCompilers: 4, MaxCompilersPerNamespace: 1, Now: func() time.Time { return now }, NewToken: func() string { return "new" }}, c
}

func reconcileRecord(t *testing.T, r *RecordReconciler, c client.Client) *cachev1alpha1.SpinCompiledCache {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "kss", Name: "spincc-1"}}); err != nil {
		t.Fatal(err)
	}
	var rec cachev1alpha1.SpinCompiledCache
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "kss", Name: "spincc-1"}, &rec); err != nil {
		t.Fatal(err)
	}
	return &rec
}

var compileArgs = []string{runtimecontract.CompileCommand, "--source=x", "--runtime=y", "--architecture=amd64"}

// A rejection keeps counting failures: the backoff grows instead of
// restarting at one minute.
func TestRejectionBacksOff(t *testing.T) {
	now := time.Now()
	rec := exportedRecord("u1")
	rec.Status.Signature = &cachev1alpha1.SignatureStatus{Artifact: rec.Status.Compile.Artifact, SandboxUID: "u1", Rejected: true, Reason: "EntryInvalid"}
	r, c := newRecordReconciler(t, now, rec, compilerSandbox("u1", compileArgs))
	got := reconcileRecord(t, r, c).Status.Compile
	if got.Failures != 2 || got.Reason != "SignerRejected" || got.Artifact != "" || got.Sandbox != nil || got.RetryAfter == nil ||
		got.RetryAfter.Sub(now) < 119*time.Second {
		t.Fatalf("compile %+v", got)
	}
}

// A rejection judged another compiler: the same bytes exported again by
// this attempt wait for the signer's own verdict.
func TestRejectionOfAnEarlierCompilerIsIgnored(t *testing.T) {
	rec := exportedRecord("u2")
	rec.Status.Signature = &cachev1alpha1.SignatureStatus{Artifact: rec.Status.Compile.Artifact, SandboxUID: "u1", Rejected: true}
	r, c := newRecordReconciler(t, time.Now(), rec, compilerSandbox("u2", compileArgs))
	got := reconcileRecord(t, r, c).Status.Compile
	if got.Artifact == "" || got.Failures != 1 || got.Reason != "" {
		t.Fatalf("compile %+v", got)
	}
}

func TestCompilerGoneBeforeSigning(t *testing.T) {
	r, c := newRecordReconciler(t, time.Now(), exportedRecord("u1"))
	got := reconcileRecord(t, r, c).Status.Compile
	if got.Reason != "CompilerGoneBeforeSigning" || got.Failures != 2 || got.Artifact != "" {
		t.Fatalf("compile %+v", got)
	}
}

func TestTrustedForgetsFailures(t *testing.T) {
	rec := exportedRecord("u1")
	rec.Status.Signature = &cachev1alpha1.SignatureStatus{Artifact: rec.Status.Compile.Artifact, SandboxUID: "u1", SignedAt: &metav1.Time{Time: time.Now()}}
	r, c := newRecordReconciler(t, time.Now(), rec, compilerSandbox("u1", compileArgs))
	got := reconcileRecord(t, r, c)
	if got.Status.Compile.Failures != 0 || got.Status.Compile.Artifact == "" {
		t.Fatalf("compile %+v", got.Status.Compile)
	}
	var l sandboxv1alpha1.SwiftSandboxList
	if err := c.List(context.Background(), &l); err != nil || len(l.Items) != 0 {
		t.Fatalf("the compiler of a signed cache must go: %d left", len(l.Items))
	}
}

// Labels alone never make the cache manager delete a sandbox.
func TestOnlyCompilersAreDeleted(t *testing.T) {
	replica := compilerSandbox("u9", []string{"up", "--from=x"})
	replica.Spec.Outputs = nil
	r, c := newRecordReconciler(t, time.Now(), replica)
	// The record does not exist: its compilers are removed, the replica
	// with forged labels is not.
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "kss", Name: "spincc-1"}}); err != nil {
		t.Fatal(err)
	}
	var sb sandboxv1alpha1.SwiftSandbox
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(replica), &sb); err != nil {
		t.Fatalf("a sandbox that is not compiler-shaped was deleted: %v", err)
	}
}
