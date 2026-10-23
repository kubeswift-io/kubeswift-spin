package compiledcache

import (
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
)

const (
	digestA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

var settings = Settings{Namespace: "kubeswift-spin-system", Repository: "registry.example.com/kubeswift-spin-cache", Architecture: "amd64",
	Runtimes: []string{"ghcr.io/kubeswift-io/kubeswift-spin-runtime@" + digestB}}

// testRepo is the cache repository of namespace team-a in the record tests.
const testRepo = "registry.example.com/c/team-a/wasmtime-cache"

func app(image string, ann map[string]string) *spinv1alpha1.SpinApp {
	a := &spinv1alpha1.SpinApp{Spec: spinv1alpha1.SpinAppSpec{Image: image, Executor: "kubeswift"}}
	a.Namespace, a.Name, a.Annotations = "team-a", "hello", ann
	return a
}

func profile(runtime string, enabled bool, sel map[string]string) *executor.Profile {
	return &executor.Profile{ExecutorName: "kubeswift", RuntimeImage: runtime, CompiledCache: enabled, NodeSelector: sel}
}

func TestPinnedReference(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"ghcr.io/org/app@" + digestA, "ghcr.io/org/app@" + digestA, true},
		{"ghcr.io/org/app:v1@" + digestA, "ghcr.io/org/app@" + digestA, true},
		{"docker.io/library/app@" + digestA, "index.docker.io/library/app@" + digestA, true},
		{"ghcr.io/org/app:v1", "", false},
		{"ghcr.io/org/app", "", false},
		{"app@" + digestA, "", false}, // no explicit registry
		{"not a reference", "", false},
	} {
		got, err := PinnedReference(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("PinnedReference(%q) = %q, %v; want %q, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestRequested(t *testing.T) {
	on := profile("ghcr.io/x/rt@"+digestB, true, nil)
	off := profile("ghcr.io/x/rt@"+digestB, false, nil)
	if Requested(app("ghcr.io/org/app@"+digestA, nil), off) {
		t.Error("an executor without the annotation must not request a cache")
	}
	if !Requested(app("ghcr.io/org/app@"+digestA, nil), on) {
		t.Error("an enabled executor requests a cache")
	}
	if Requested(app("ghcr.io/org/app@"+digestA, map[string]string{executor.AnnCompiledCache: "disabled"}), on) {
		t.Error("a SpinApp can opt out")
	}
	if Requested(app("ghcr.io/org/app@"+digestA, map[string]string{executor.AnnCompiledCache: "enabled"}), off) {
		t.Error("a SpinApp cannot opt in when its executor does not enable caches")
	}
	if Requested(app("ghcr.io/org/app@"+digestA, map[string]string{executor.AnnCompiledCache: "maybe"}), on) {
		t.Error("an invalid value is not a request")
	}
}

func TestDerive(t *testing.T) {
	rt := "ghcr.io/kubeswift-io/kubeswift-spin-runtime:spin-4.2.1-r2@" + digestB
	id, skip := Derive(app("ghcr.io/org/app:v1@"+digestA, nil), profile(rt, true, nil), settings)
	if skip != nil {
		t.Fatalf("unexpected skip %+v", skip)
	}
	want := Identity{Namespace: "team-a", Source: "ghcr.io/org/app@" + digestA,
		Runtime: "ghcr.io/kubeswift-io/kubeswift-spin-runtime@" + digestB, Architecture: "amd64",
		Repository: "registry.example.com/kubeswift-spin-cache/team-a/wasmtime-cache"}
	if *id != want {
		t.Fatalf("identity %+v, want %+v", *id, want)
	}

	id, _ = Derive(app("ghcr.io/org/app@"+digestA, nil), profile(rt, true, map[string]string{ArchitectureLabel: "arm64"}), settings)
	if id.Architecture != "arm64" {
		t.Errorf("the executor's node selector decides the architecture, got %q", id.Architecture)
	}

	for _, tc := range []struct {
		name   string
		app    *spinv1alpha1.SpinApp
		p      *executor.Profile
		s      Settings
		reason string
	}{
		{"not configured", app("ghcr.io/org/app@"+digestA, nil), profile(rt, true, nil), Settings{}, ReasonNotConfigured},
		{"tagged image", app("ghcr.io/org/app:v1", nil), profile(rt, true, nil), settings, ReasonImageNotPinned},
		{"tagged runtime", app("ghcr.io/org/app@"+digestA, nil), profile("ghcr.io/x/rt:v1", true, nil), settings, ReasonRuntimeNotPinned},
		{"other architecture", app("ghcr.io/org/app@"+digestA, nil), profile(rt, true, map[string]string{ArchitectureLabel: "s390x"}), settings, ReasonUnsupported},
		// KubeSwift accepts claim values of at most 256 characters.
		{"long reference", app("ghcr.io/"+strings.Repeat("a", 200)+"/app@"+digestA, nil), profile(rt, true, nil), settings, ReasonUnsupported},
	} {
		_, skip := Derive(tc.app, tc.p, tc.s)
		if skip == nil || skip.Reason != tc.reason {
			t.Errorf("%s: skip %+v, want reason %s", tc.name, skip, tc.reason)
		}
	}
}

func TestRecordName(t *testing.T) {
	base := Identity{Namespace: "a", Source: "r/s@" + digestA, Runtime: "r/rt@" + digestB, Architecture: "amd64", Repository: "r/c/a/wasmtime-cache"}
	again := base
	if base.RecordName() != again.RecordName() {
		t.Fatal("record names must be deterministic")
	}
	if !strings.HasPrefix(base.RecordName(), "spincc-") || len(base.RecordName()) != len("spincc-")+32 {
		t.Errorf("record name %q", base.RecordName())
	}
	seen := map[string]bool{base.RecordName(): true}
	for _, v := range []Identity{
		{Namespace: "b", Source: base.Source, Runtime: base.Runtime, Architecture: "amd64", Repository: base.Repository},
		{Namespace: "a", Source: "r/s@" + digestB, Runtime: base.Runtime, Architecture: "amd64", Repository: base.Repository},
		{Namespace: "a", Source: base.Source, Runtime: "r/rt@" + digestA, Architecture: "amd64", Repository: base.Repository},
		{Namespace: "a", Source: base.Source, Runtime: base.Runtime, Architecture: "arm64", Repository: base.Repository},
		// A changed platform repository is a different record.
		{Namespace: "a", Source: base.Source, Runtime: base.Runtime, Architecture: "amd64", Repository: "r/other/a/wasmtime-cache"},
		// Field boundaries are part of the hash.
		{Namespace: "ar/s@" + digestA, Source: "", Runtime: base.Runtime, Architecture: "amd64", Repository: base.Repository},
	} {
		if seen[v.RecordName()] {
			t.Errorf("identity %+v collides", v)
		}
		seen[v.RecordName()] = true
	}
}

func TestSettingsValidate(t *testing.T) {
	ok := []Settings{
		{},
		settings,
		{Namespace: "ns", Repository: "registry.example.com:5000/a/b", Architecture: "arm64", Runtimes: settings.Runtimes},
		{Namespace: "ns", Repository: "registry.example.com/a--b/c__d.e", Architecture: "amd64", Runtimes: settings.Runtimes},
	}
	for _, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	bad := []Settings{
		{Namespace: "ns", Repository: "registry.example.com", Architecture: "amd64"},
		{Namespace: "ns", Repository: "registry.example.com/a:tag", Architecture: "amd64"},
		{Namespace: "ns", Repository: "registry.example.com/a@" + digestA, Architecture: "amd64"},
		{Namespace: "ns", Repository: "Registry.example.com/A", Architecture: "amd64"},
		{Namespace: "ns", Repository: "docker.io/library/x", Architecture: "amd64"}, // not canonical
		{Namespace: "ns", Repository: "registry.example.com/a", Architecture: "386"},
		{Repository: "registry.example.com/a", Architecture: "amd64"},
		{Namespace: "ns", Repository: "registry.example.com/a-", Architecture: "amd64"},
		{Namespace: "ns", Repository: "registry.example.com/a___b", Architecture: "amd64"},
		// No room for a 63-character namespace within 255 characters.
		{Namespace: "ns", Repository: "registry.example.com/" + strings.Repeat("a", 160), Architecture: "amd64"},
		// No allowed runtime, or one that is not canonical.
		{Namespace: "ns", Repository: "registry.example.com/a", Architecture: "amd64"},
		{Namespace: "ns", Repository: "registry.example.com/a", Architecture: "amd64", Runtimes: []string{"ghcr.io/x/rt:v1"}},
		{Namespace: "ns", Repository: "registry.example.com/a", Architecture: "amd64", Runtimes: []string{"docker.io/library/rt@" + digestB}},
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("%+v: expected an error", s)
		}
	}
}

func TestBackoff(t *testing.T) {
	for f, want := range map[int32]time.Duration{0: 0, 1: time.Minute, 2: 2 * time.Minute, 4: 8 * time.Minute, 9: 256 * time.Minute, 10: 6 * time.Hour, 40: 6 * time.Hour} {
		if got := Backoff(f); got != want {
			t.Errorf("Backoff(%d) = %s, want %s", f, got, want)
		}
	}
}

func TestClaims(t *testing.T) {
	id := Identity{Namespace: "team-a", Source: "r/s@" + digestA, Runtime: "r/rt@" + digestB, Architecture: "amd64", Repository: "r/c/team-a/wasmtime-cache"}
	signed := id.Claims("kubeswift-spin/v0.2.0")
	for k, v := range id.ArtifactClaims() {
		if signed[k] != v {
			t.Errorf("a replica requires %s=%s, which the signer does not sign", k, v)
		}
	}
	if len(id.ArtifactClaims()) > 8 {
		t.Error("KubeSwift allows at most 8 artifact claims")
	}
}

func record(compile *cachev1alpha1.CompileStatus, sig *cachev1alpha1.SignatureStatus, consumers []cachev1alpha1.Consumer, placements []cachev1alpha1.PlacementStatus) *cachev1alpha1.SpinCompiledCache {
	r := &cachev1alpha1.SpinCompiledCache{}
	r.Name, r.Generation = "spincc-x", 1
	r.Spec.Namespace = "team-a"
	r.Spec.Repository = testRepo
	r.Status.Compile, r.Status.Signature, r.Status.Consumers, r.Status.Placements = compile, sig, consumers, placements
	for _, c := range consumers {
		if c.Current && c.Pool != "" {
			r.Status.Pools = append(r.Status.Pools, c.Pool)
		}
	}
	return r
}

func reasons(conds []metav1.Condition) map[string]string {
	out := map[string]string{}
	for _, c := range conds {
		out[c.Type] = string(c.Status) + "/" + c.Reason
	}
	return out
}

func TestRecordConditions(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	art := testRepo + "@" + digestA
	signedAt := &metav1.Time{Time: now}
	running := &cachev1alpha1.CompileStatus{Attempt: 1, Sandbox: &cachev1alpha1.SandboxReference{Name: "c", UID: "u"}}
	exported := &cachev1alpha1.CompileStatus{Attempt: 1, Sandbox: running.Sandbox, FinishedAt: signedAt, Artifact: art}
	failed := &cachev1alpha1.CompileStatus{Attempt: 2, Failures: 2, Reason: "WorkloadFailed", Message: "spin exited 1", RetryAfter: &metav1.Time{Time: now.Add(time.Minute)}}
	signed := &cachev1alpha1.SignatureStatus{Artifact: art, KeyID: "k", SignedAt: signedAt}
	pool := []cachev1alpha1.Consumer{{Name: "hello", Current: true, Pool: "warm"}}

	for _, tc := range []struct {
		name string
		rec  *cachev1alpha1.SpinCompiledCache
		want map[string]string
	}{
		{"new", record(nil, nil, nil, nil), map[string]string{"Compiled": "False/Pending", "Trusted": "False/NotCompiled", "Placed": "False/NotTrusted", "Ready": "False/Pending"}},
		{"compiling", record(running, nil, nil, nil), map[string]string{"Compiled": "False/Compiling", "Ready": "False/Compiling"}},
		{"failed", record(failed, nil, nil, nil), map[string]string{"Compiled": "False/CompileFailed", "Ready": "False/CompileFailed"}},
		{"waiting for signer", record(exported, nil, nil, nil), map[string]string{"Compiled": "True/Exported", "Trusted": "False/Signing", "Ready": "False/Signing"}},
		{"rejected", record(exported, &cachev1alpha1.SignatureStatus{Artifact: art, Rejected: true, Reason: "BadEntry"}, nil, nil), map[string]string{"Trusted": "False/Rejected"}},
		{"signature of an older artifact", record(exported, &cachev1alpha1.SignatureStatus{Artifact: testRepo + "@" + digestB, SignedAt: signedAt}, nil, nil), map[string]string{"Trusted": "False/Signing"}},
		{"signed, no pool", record(exported, signed, []cachev1alpha1.Consumer{{Name: "hello", Current: true}}, nil), map[string]string{"Trusted": "True/Signed", "Placed": "True/NotRequired", "Ready": "True/Ready"}},
		{"signed, placing", record(exported, signed, pool, []cachev1alpha1.PlacementStatus{{Pool: "warm", Name: "p", PreparedNodes: 1, TargetNodes: 3, Reason: "Preparing"}}), map[string]string{"Placed": "False/Progressing", "Ready": "False/Progressing"}},
		{"signed, placement failed", record(exported, signed, pool, []cachev1alpha1.PlacementStatus{{Pool: "warm", Name: "p", Reason: "ClaimMismatch"}}), map[string]string{"Placed": "False/PlacementFailed"}},
		{"ready", record(exported, signed, pool, []cachev1alpha1.PlacementStatus{{Pool: "warm", Name: "p", Prepared: true, PreparedNodes: 3, TargetNodes: 3}}), map[string]string{"Placed": "True/Prepared", "Ready": "True/Ready"}},
		{"pool without slots", record(exported, signed, pool, []cachev1alpha1.PlacementStatus{{Pool: "warm", Name: "p", Reason: "NoTargetNodes"}}), map[string]string{"Placed": "True/Prepared", "Ready": "True/Ready"}},
	} {
		got := reasons(RecordConditions(tc.rec, now))
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%s: %s is %s, want %s", tc.name, k, got[k], v)
			}
		}
	}
}

func TestTrustedArtifact(t *testing.T) {
	art := testRepo + "@" + digestA
	at := &metav1.Time{Time: time.Now()}
	if TrustedArtifact(record(&cachev1alpha1.CompileStatus{Artifact: art}, &cachev1alpha1.SignatureStatus{Artifact: art, SignedAt: at}, nil, nil)) != art {
		t.Error("a signed artifact is trusted")
	}
	for name, rec := range map[string]*cachev1alpha1.SpinCompiledCache{
		"unsigned":       record(&cachev1alpha1.CompileStatus{Artifact: art}, nil, nil, nil),
		"other artifact": record(&cachev1alpha1.CompileStatus{Artifact: art}, &cachev1alpha1.SignatureStatus{Artifact: testRepo + "@" + digestB, SignedAt: at}, nil, nil),
		"other repository": record(&cachev1alpha1.CompileStatus{Artifact: "registry.example.com/c/team-b/wasmtime-cache@" + digestA},
			&cachev1alpha1.SignatureStatus{Artifact: "registry.example.com/c/team-b/wasmtime-cache@" + digestA, SignedAt: at}, nil, nil),
		"rejected": record(&cachev1alpha1.CompileStatus{Artifact: art}, &cachev1alpha1.SignatureStatus{Artifact: art, SignedAt: at, Rejected: true}, nil, nil),
		"no time":  record(&cachev1alpha1.CompileStatus{Artifact: art}, &cachev1alpha1.SignatureStatus{Artifact: art}, nil, nil),
	} {
		if TrustedArtifact(rec) != "" {
			t.Errorf("%s: must not be trusted", name)
		}
	}
}

func TestAppCondition(t *testing.T) {
	now := time.Now()
	art := testRepo + "@" + digestA
	at := &metav1.Time{Time: now}
	withConds := func(r *cachev1alpha1.SpinCompiledCache) *cachev1alpha1.SpinCompiledCache {
		for _, c := range RecordConditions(r, now) {
			apimeta.SetStatusCondition(&r.Status.Conditions, c)
		}
		return r
	}
	exported := &cachev1alpha1.CompileStatus{Artifact: art}
	signed := &cachev1alpha1.SignatureStatus{Artifact: art, SignedAt: at}
	pool := []cachev1alpha1.Consumer{{Name: "hello", Current: true, Pool: "warm"}}
	for _, tc := range []struct {
		name   string
		skip   *Skip
		rec    *cachev1alpha1.SpinCompiledCache
		status metav1.ConditionStatus
		reason string
	}{
		{"skip", &Skip{Reason: ReasonImageNotPinned, Message: "m"}, nil, metav1.ConditionFalse, ReasonImageNotPinned},
		{"no record", nil, nil, metav1.ConditionFalse, ReasonRequested},
		{"record without conditions", nil, record(nil, nil, nil, nil), metav1.ConditionFalse, ReasonRequested},
		{"compiling", nil, withConds(record(nil, nil, nil, nil)), metav1.ConditionFalse, ReasonCompiling},
		{"compile failed", nil, withConds(record(&cachev1alpha1.CompileStatus{Failures: 1, Reason: "X"}, nil, nil, nil)), metav1.ConditionFalse, ReasonCompileFailed},
		{"signing", nil, withConds(record(exported, nil, nil, nil)), metav1.ConditionFalse, ReasonSigning},
		{"rejected", nil, withConds(record(exported, &cachev1alpha1.SignatureStatus{Artifact: art, Rejected: true}, nil, nil)), metav1.ConditionFalse, ReasonSignFailed},
		{"placing", nil, withConds(record(exported, signed, pool, nil)), metav1.ConditionTrue, ReasonPlacing},
		{"placement failed", nil, withConds(record(exported, signed, pool, []cachev1alpha1.PlacementStatus{{Pool: "warm", Reason: "PolicyDenied"}})), metav1.ConditionTrue, ReasonPlacementFailed},
		{"ready", nil, withConds(record(exported, signed, nil, nil)), metav1.ConditionTrue, ReasonReady},
	} {
		c := AppCondition(tc.skip, tc.rec)
		if c.Type != TypeCompiledCache || c.Status != tc.status || c.Reason != tc.reason {
			t.Errorf("%s: %s/%s, want %s/%s (%s)", tc.name, c.Status, c.Reason, tc.status, tc.reason, c.Message)
		}
	}
}

func TestCheckRecord(t *testing.T) {
	s := Settings{Namespace: "kubeswift-spin-system", Repository: "registry.example.com/cache", Architecture: "amd64",
		Runtimes: []string{"registry.example.com/runtime@sha256:" + strings.Repeat("2", 64)}}
	good := func() *cachev1alpha1.SpinCompiledCache {
		rec := &cachev1alpha1.SpinCompiledCache{Spec: cachev1alpha1.SpinCompiledCacheSpec{
			Namespace: "team-a", Source: "registry.example.com/app@sha256:" + strings.Repeat("1", 64),
			Runtime: s.Runtimes[0], Architecture: "amd64", Repository: s.NamespaceRepository("team-a")}}
		rec.Name = IdentityOf(rec).RecordName()
		return rec
	}
	if err := s.CheckRecord(good()); err != nil {
		t.Fatalf("good record refused: %v", err)
	}
	cases := map[string]func(*cachev1alpha1.SpinCompiledCache){
		"other name":       func(r *cachev1alpha1.SpinCompiledCache) { r.Name = "spincc-1" },
		"other repository": func(r *cachev1alpha1.SpinCompiledCache) { r.Spec.Repository = s.NamespaceRepository("team-b") },
		"runtime not allowed": func(r *cachev1alpha1.SpinCompiledCache) {
			r.Spec.Runtime = "registry.example.com/other@sha256:" + strings.Repeat("3", 64)
		},
		"architecture": func(r *cachev1alpha1.SpinCompiledCache) { r.Spec.Architecture = "riscv64" },
		// cosign splits -a values on commas: this source would override the
		// namespace and source claims.
		"claim injection": func(r *cachev1alpha1.SpinCompiledCache) {
			r.Spec.Source = "reg.example/app,spin.kubeswift.io/namespace=victim@sha256:" + strings.Repeat("1", 64)
		},
		"tagged source": func(r *cachev1alpha1.SpinCompiledCache) {
			r.Spec.Source = "registry.example.com/app:v1@sha256:" + strings.Repeat("1", 64)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rec := good()
			mutate(rec)
			if name != "other name" {
				rec.Name = IdentityOf(rec).RecordName()
			}
			if err := s.CheckRecord(rec); err == nil {
				t.Fatal("record accepted")
			}
		})
	}
}

func TestValidClaims(t *testing.T) {
	if err := ValidClaims(map[string]string{ClaimSource: "a,b=c"}); err == nil {
		t.Fatal("a claim value with a comma accepted")
	}
	if err := ValidClaims(map[string]string{ClaimSource: strings.Repeat("a", 257)}); err == nil {
		t.Fatal("a long claim value accepted")
	}
	if err := ValidClaims(map[string]string{ClaimCompiler: "kubeswift-spin-cache-signer/v0.2.0-rc.1+abc"}); err != nil {
		t.Fatal(err)
	}
}
