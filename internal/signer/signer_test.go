package signer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
	"github.com/kubeswift-io/kubeswift-spin/internal/scheme"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"
)

const (
	appDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testRT    = "ghcr.io/kubeswift-io/kubeswift-spin-runtime@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	entryA    = "fIv8YTziONl_i2AXvYDbs4IdFgMrfnVY4sW-98iyyuo"
	entryB    = "h2vHAWfmmSvc16upNQY0rQgFufcHV6U0Cc0qfO5VI6E"
)

type file struct {
	name string
	body string
	typ  byte
}

func layerBytes(t *testing.T, files []file) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		typ := f.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: f.name, Typeflag: typ, Mode: 0o644, Size: int64(len(f.body))}
		switch typ {
		case tar.TypeDir:
			h.Mode, h.Size = 0o755, 0
		case tar.TypeSymlink:
			h.Linkname, h.Size = "/etc/passwd", 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(f.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func metadata(t *testing.T, spec cachev1alpha1.SpinCompiledCacheSpec, entries ...string) string {
	t.Helper()
	b, err := json.Marshal(runtimecontract.CacheMetadata{Kind: runtimecontract.CompiledCacheKind, Source: spec.Source, Runtime: spec.Runtime,
		Architecture: spec.Architecture, Spin: "spin 4.2.1", WasmtimeCache: "wasmtime-49.0.1", Entries: entries, Components: []string{"hello"}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func goodFiles(t *testing.T, spec cachev1alpha1.SpinCompiledCacheSpec) []file {
	return []file{
		{name: "cache/", typ: tar.TypeDir}, {name: "cache/modules/", typ: tar.TypeDir}, {name: "cache/modules/wasmtime-49.0.1/", typ: tar.TypeDir},
		{name: "cache/modules/wasmtime-49.0.1/" + entryA, body: "native-a"},
		{name: "cache/modules/wasmtime-49.0.1/" + entryB, body: "native-b"},
		{name: "metadata.json", body: metadata(t, spec, entryA, entryB)},
	}
}

type rawManifest []byte

func (m rawManifest) RawManifest() ([]byte, error)        { return m, nil }
func (m rawManifest) MediaType() (types.MediaType, error) { return types.OCIManifestSchema1, nil }

// push writes an artifact the way KubeSwift's export does, tagged tag, and
// returns its manifest digest.
func push(t *testing.T, repo, tag string, layer []byte, artifactType string) string {
	t.Helper()
	r, err := name.NewRepository(repo, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	cfg := static.NewLayer([]byte("{}"), "application/vnd.oci.empty.v1+json")
	l := static.NewLayer(layer, types.OCILayer)
	if err := remote.WriteLayer(r, cfg); err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteLayer(r, l); err != nil {
		t.Fatal(err)
	}
	cd, _ := cfg.Digest()
	ld, _ := l.Digest()
	ls, _ := l.Size()
	m := map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "artifactType": artifactType,
		"config": map[string]any{"mediaType": "application/vnd.oci.empty.v1+json", "digest": cd.String(), "size": 2},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": ld.String(), "size": ls}},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(r.Tag(tag), rawManifest(raw)); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// harness is a registry, a record, its compiler sandbox and a reconciler.
type harness struct {
	t      *testing.T
	host   string
	rec    *cachev1alpha1.SpinCompiledCache
	sb     *sandboxv1alpha1.SwiftSandbox
	signer *fakeSigner
	r      *Reconciler
	c      client.Client
}

type fakeSigner struct {
	signed []string
	claims map[string]string
	err    error
}

func (f *fakeSigner) Sign(_ context.Context, ref string, claims map[string]string) error {
	if f.err != nil {
		return f.err
	}
	f.signed = append(f.signed, ref)
	f.claims = claims
	return nil
}

func (f *fakeSigner) KeyID(context.Context) (string, error) { return "key-1", nil }

// newHarness builds a record whose compiler exported files under the
// compiler's export tag; artifact overrides what the record names.
func newHarness(t *testing.T, files func(spec cachev1alpha1.SpinCompiledCacheSpec) []file, artifactType string) *harness {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	host := u.Host
	repo := host + "/kubeswift-spin-cache/team-a/wasmtime-cache"
	settings := compiledcache.Settings{Namespace: "kubeswift-spin-system", Repository: host + "/kubeswift-spin-cache", Architecture: "amd64", Runtimes: []string{testRT}}

	rec := &cachev1alpha1.SpinCompiledCache{ObjectMeta: metav1.ObjectMeta{Namespace: settings.Namespace, Name: "spincc-1"},
		Spec: cachev1alpha1.SpinCompiledCacheSpec{Namespace: "team-a", Source: "ghcr.io/org/app@" + appDigest, Runtime: testRT, Architecture: "amd64", Repository: repo}}
	app := &spinv1alpha1.SpinApp{}
	app.Namespace, app.Name, app.Spec.Image = "team-a", "hello", "ghcr.io/org/app@"+appDigest
	p := &executor.Profile{ExecutorName: "kubeswift", RuntimeImage: testRT, CompiledCache: true, NetworkMode: sandboxv1alpha1.SandboxNetworkRestricted}
	sb := compiledcache.CompilerSandbox(rec, app, p, compiledcache.DefaultCompilerSettings(), "tok")
	sb.Name, sb.UID = "spin-compile-x", "uid-1"
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(sb)
	if err != nil {
		t.Fatal(err)
	}
	h, err := compiledcache.SpecHash(&unstructured.Unstructured{Object: m})
	if err != nil {
		t.Fatal(err)
	}
	digest := push(t, repo, compiledcache.ExportTag(sb), layerBytes(t, files(rec.Spec)), artifactType)
	art := repo + "@" + digest
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxCompleted
	sb.Status.Outputs = []sandboxv1alpha1.SandboxOutputStatus{{Name: compiledcache.OutputName, State: sandboxv1alpha1.SandboxOutputExported,
		Repository: repo, Reference: art, Digest: digest}}
	rec.Status.Compile = &cachev1alpha1.CompileStatus{Attempt: 1, Token: "tok", Sandbox: &cachev1alpha1.SandboxReference{Name: sb.Name, UID: sb.UID},
		SpecHash: h, Artifact: art}

	c := fake.NewClientBuilder().WithScheme(scheme.New()).WithObjects(rec, sb).WithStatusSubresource(rec, sb).Build()
	fs := &fakeSigner{}
	r := &Reconciler{Client: c, APIReader: c, Settings: settings, Signer: fs, Identity: "kubeswift-spin-cache-signer/test",
		Fetcher:     &Fetcher{Keychain: authn.DefaultKeychain, Limits: Limits{MaxSize: 1 << 20, MaxEntries: 100}},
		NameOptions: []name.Option{name.Insecure}, Now: time.Now}
	return &harness{t: t, host: host, rec: rec, sb: sb, signer: fs, r: r, c: c}
}

func (h *harness) reconcile() *cachev1alpha1.SignatureStatus {
	h.t.Helper()
	if _, err := h.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: ktypes.NamespacedName{Namespace: h.rec.Namespace, Name: h.rec.Name}}); err != nil {
		h.t.Fatal(err)
	}
	var rec cachev1alpha1.SpinCompiledCache
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.rec), &rec); err != nil {
		h.t.Fatal(err)
	}
	return rec.Status.Signature
}

func (h *harness) mutateSandbox(f func(*sandboxv1alpha1.SwiftSandbox)) {
	h.t.Helper()
	var sb sandboxv1alpha1.SwiftSandbox
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.sb), &sb); err != nil {
		h.t.Fatal(err)
	}
	f(&sb)
	if err := h.c.Update(context.Background(), &sb); err != nil {
		h.t.Fatal(err)
	}
}

func TestSignsAValidArtifact(t *testing.T) {
	h := newHarness(t, func(spec cachev1alpha1.SpinCompiledCacheSpec) []file { return goodFiles(t, spec) }, compiledcache.ArtifactType)
	s := h.reconcile()
	if s == nil || s.SignedAt == nil || s.Rejected || s.KeyID != "key-1" || s.Artifact != h.rec.Status.Compile.Artifact {
		t.Fatalf("signature %+v", s)
	}
	if len(h.signer.signed) != 1 || h.signer.signed[0] != h.rec.Status.Compile.Artifact {
		t.Fatalf("signed %v", h.signer.signed)
	}
	want := map[string]string{
		compiledcache.ClaimKind: compiledcache.KindWasmtimeCache, compiledcache.ClaimNamespace: "team-a",
		compiledcache.ClaimSource: h.rec.Spec.Source, compiledcache.ClaimRuntime: testRT, compiledcache.ClaimArchitecture: "amd64",
		compiledcache.ClaimCompiler: "kubeswift-spin-cache-signer/test",
	}
	if fmt.Sprint(h.signer.claims) != fmt.Sprint(want) {
		t.Fatalf("claims %v", h.signer.claims)
	}
	// Signed once: a later reconcile does nothing.
	h.reconcile()
	if len(h.signer.signed) != 1 {
		t.Fatal("signed twice")
	}
}

func TestRejects(t *testing.T) {
	keep := func(spec cachev1alpha1.SpinCompiledCacheSpec) []file { return goodFiles(t, spec) }
	cases := []struct {
		name   string
		files  func(spec cachev1alpha1.SpinCompiledCacheSpec) []file
		atype  string
		setup  func(h *harness)
		reason string
	}{
		{"symlink entry", func(spec cachev1alpha1.SpinCompiledCacheSpec) []file {
			return append(goodFiles(t, spec), file{name: "cache/modules/wasmtime-49.0.1/" + strings.Repeat("C", 43), typ: tar.TypeSymlink})
		}, compiledcache.ArtifactType, nil, "EntryInvalid"},
		{"extra file", func(spec cachev1alpha1.SpinCompiledCacheSpec) []file {
			return append(goodFiles(t, spec), file{name: "cache/modules/wasmtime-49.0.1/run.sh", body: "x"})
		}, compiledcache.ArtifactType, nil, "EntryInvalid"},
		{"path traversal", func(spec cachev1alpha1.SpinCompiledCacheSpec) []file {
			return append(goodFiles(t, spec), file{name: "cache/../../etc/x", body: "x"})
		}, compiledcache.ArtifactType, nil, "EntryInvalid"},
		{"metadata for another source", func(spec cachev1alpha1.SpinCompiledCacheSpec) []file {
			other := spec
			other.Source = "ghcr.io/org/other@" + appDigest
			f := goodFiles(t, spec)
			f[len(f)-1].body = metadata(t, other, entryA, entryB)
			return f
		}, compiledcache.ArtifactType, nil, "MetadataMismatch"},
		{"metadata missing an entry", func(spec cachev1alpha1.SpinCompiledCacheSpec) []file {
			f := goodFiles(t, spec)
			f[len(f)-1].body = metadata(t, spec, entryA)
			return f
		}, compiledcache.ArtifactType, nil, "MetadataMismatch"},
		{"no metadata", func(spec cachev1alpha1.SpinCompiledCacheSpec) []file { return goodFiles(t, spec)[:5] }, compiledcache.ArtifactType, nil, "MetadataInvalid"},
		{"other artifact type", keep, "application/vnd.example.v1", nil, "ManifestInvalid"},
		{"compiler gone", keep, compiledcache.ArtifactType, func(h *harness) {
			if err := h.c.Delete(context.Background(), h.sb); err != nil {
				t.Fatal(err)
			}
		}, "CompilerGone"},
		{"compiler replaced", keep, compiledcache.ArtifactType, func(h *harness) {
			h.mutateSandbox(func(sb *sandboxv1alpha1.SwiftSandbox) { sb.Spec.CPU = 4 })
		}, "CompilerCheckFailed"},
		{"compiler not completed", keep, compiledcache.ArtifactType, func(h *harness) {
			var sb sandboxv1alpha1.SwiftSandbox
			_ = h.c.Get(context.Background(), client.ObjectKeyFromObject(h.sb), &sb)
			sb.Status.Phase = sandboxv1alpha1.SwiftSandboxFailed
			if err := h.c.Status().Update(context.Background(), &sb); err != nil {
				t.Fatal(err)
			}
		}, "CompilerCheckFailed"},
		{"runtime no longer allowed", keep, compiledcache.ArtifactType, func(h *harness) {
			h.r.Settings.Runtimes = []string{"ghcr.io/other/rt@" + appDigest}
		}, "CompilerCheckFailed"},
		{"record names another digest than the export tag", keep, compiledcache.ArtifactType, func(h *harness) {
			// A forged status.outputs or record: an artifact the tenant
			// exported from another sandbox.
			other := push(t, h.rec.Spec.Repository, "other", layerBytes(t, goodFiles(t, h.rec.Spec)[:5]), compiledcache.ArtifactType)
			var rec cachev1alpha1.SpinCompiledCache
			_ = h.c.Get(context.Background(), client.ObjectKeyFromObject(h.rec), &rec)
			rec.Status.Compile.Artifact = h.rec.Spec.Repository + "@" + other
			if err := h.c.Status().Update(context.Background(), &rec); err != nil {
				t.Fatal(err)
			}
		}, "DigestMismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.files, tc.atype)
			if tc.setup != nil {
				tc.setup(h)
			}
			s := h.reconcile()
			if s == nil || !s.Rejected || s.Reason != tc.reason || s.SignedAt != nil {
				t.Fatalf("signature %+v, want rejected with %s", s, tc.reason)
			}
			if len(h.signer.signed) != 0 {
				t.Fatal("signed a rejected artifact")
			}
		})
	}
}

func TestSigningFailureIsRetried(t *testing.T) {
	h := newHarness(t, func(spec cachev1alpha1.SpinCompiledCacheSpec) []file { return goodFiles(t, spec) }, compiledcache.ArtifactType)
	h.signer.err = fmt.Errorf("registry unavailable")
	s := h.reconcile()
	if s == nil || s.Rejected || s.Failures != 1 || s.LastFailureTime == nil || s.SignedAt != nil {
		t.Fatalf("signature %+v", s)
	}
	// Within the backoff nothing happens; after it, signing succeeds and
	// the failure is cleared.
	h.signer.err = nil
	h.reconcile()
	if len(h.signer.signed) != 0 {
		t.Fatal("retried within the backoff")
	}
	h.r.Now = func() time.Time { return time.Now().Add(time.Minute) }
	s = h.reconcile()
	if s.SignedAt == nil || s.Failures != 0 || s.LastFailureTime != nil || s.Reason != "" {
		t.Fatalf("after retry %+v", s)
	}
}

func TestLimits(t *testing.T) {
	h := newHarness(t, func(spec cachev1alpha1.SpinCompiledCacheSpec) []file {
		f := goodFiles(t, spec)
		f[3].body = strings.Repeat("x", 4096)
		return f
	}, compiledcache.ArtifactType)
	h.r.Fetcher.Limits = Limits{MaxSize: 1024, MaxEntries: 100}
	if s := h.reconcile(); s == nil || !s.Rejected || s.Reason != "ArtifactTooLarge" {
		t.Fatalf("signature %+v", s)
	}
	h = newHarness(t, func(spec cachev1alpha1.SpinCompiledCacheSpec) []file { return goodFiles(t, spec) }, compiledcache.ArtifactType)
	h.r.Fetcher.Limits = Limits{MaxSize: 1 << 20, MaxEntries: 3}
	if s := h.reconcile(); s == nil || !s.Rejected || s.Reason != "ArtifactTooLarge" {
		t.Fatalf("signature %+v", s)
	}
}

func TestKeyIDFromPEM(t *testing.T) {
	pemKey := "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==\n-----END PUBLIC KEY-----\n"
	id, err := KeyIDFromPEM([]byte(pemKey))
	if err != nil || len(id) != 64 {
		t.Fatalf("%s %v", id, err)
	}
	if _, err := KeyIDFromPEM([]byte("not pem")); err == nil {
		t.Fatal("accepted garbage")
	}
}

// A rejection judged one compiler; the same bytes exported by a later
// compiler are judged again, and once signed stay signed.
func TestDecisionsNameTheCompiler(t *testing.T) {
	h := newHarness(t, func(spec cachev1alpha1.SpinCompiledCacheSpec) []file { return goodFiles(t, spec) }, compiledcache.ArtifactType)
	var rec cachev1alpha1.SpinCompiledCache
	_ = h.c.Get(context.Background(), client.ObjectKeyFromObject(h.rec), &rec)
	rec.Status.Signature = &cachev1alpha1.SignatureStatus{Artifact: rec.Status.Compile.Artifact, SandboxUID: "uid-0", Rejected: true, Reason: "CompilerGone"}
	if err := h.c.Status().Update(context.Background(), &rec); err != nil {
		t.Fatal(err)
	}
	s := h.reconcile()
	if s.SignedAt == nil || s.Rejected || s.SandboxUID != "uid-1" {
		t.Fatalf("signature %+v", s)
	}
}
