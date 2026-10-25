package spinlayout

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testRepo = "registry.example.com/apps/hello"

type layout struct {
	t      *testing.T
	dir    string
	layers []map[string]any
	config map[string]any
}

func newLayout(t *testing.T) *layout {
	t.Helper()
	l := &layout{t: t, dir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(l.dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	l.config = l.blob("application/vnd.oci.image.config.v1+json", []byte("{}"))
	return l
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// blob stores b and returns its descriptor.
func (l *layout) blob(mediaType string, b []byte) map[string]any {
	d := digestOf(b)
	if err := os.WriteFile(filepath.Join(l.dir, "blobs", "sha256", d[7:]), b, 0o444); err != nil {
		l.t.Fatal(err)
	}
	return map[string]any{"mediaType": mediaType, "digest": d, "size": len(b)}
}

func (l *layout) layer(mediaType string, b []byte) string {
	d := l.blob(mediaType, b)
	l.layers = append(l.layers, d)
	return d["digest"].(string)
}

func mustJSON(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// finish writes the manifest (raw if given), index.json and oci-layout and
// returns the manifest digest.
func (l *layout) finish(raw []byte) string {
	if raw == nil {
		raw = mustJSON(l.t, map[string]any{"schemaVersion": 2, "mediaType": mediaOCIManifest, "config": l.config, "layers": l.layers})
	}
	d := l.blob(mediaOCIManifest, raw)
	idx := map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []any{map[string]any{"mediaType": mediaOCIManifest, "digest": d["digest"], "size": d["size"],
			"annotations": map[string]any{"org.opencontainers.image.ref.name": "other.example.com/someone/else@" + d["digest"].(string)}}}}
	if err := os.WriteFile(filepath.Join(l.dir, "index.json"), mustJSON(l.t, idx), 0o444); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o444); err != nil {
		l.t.Fatal(err)
	}
	return d["digest"].(string)
}

func targz(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

// app builds a complete application: one Wasm component with an inline
// file, a per-file layer, an archive member, and the lock as a layer.
func app(t *testing.T, mutate func(lock map[string]any)) (*layout, string) {
	l := newLayout(t)
	wasm := l.layer(mediaWasm, []byte("\x00asm component"))
	file := l.layer("application/vnd.wasm.content.layer.v1+data", []byte("index"))
	l.layer(MediaArchive, targz(t, map[string]string{"x/arch.txt": "archived"}))
	lock := map[string]any{
		"spin_lock_version": 0,
		"metadata":          map[string]any{"name": "hello", "unknown_meta": "kept"},
		"variables":         map[string]any{"greeting": map[string]any{"default": "hi"}},
		"triggers": []any{map[string]any{"id": "t", "trigger_type": "http",
			"trigger_config": map[string]any{"component": "hello", "route": "/..."}}},
		"components": []any{map[string]any{
			"id":     "hello",
			"source": map[string]any{"content_type": "application/wasm", "digest": wasm},
			"env":    map[string]any{"A": "b"},
			"files": []any{
				map[string]any{"digest": file, "path": "static/index.html"},
				map[string]any{"digest": digestOf([]byte("archived")), "path": "a/arch.txt"},
				map[string]any{"inline": base64.RawStdEncoding.EncodeToString([]byte("inl")), "path": "inline.txt"},
			},
		}},
	}
	if mutate != nil {
		mutate(lock)
	}
	l.layer(MediaLockedApp, mustJSON(t, lock))
	return l, ""
}

func prepare(t *testing.T, l *layout, d string) (*Result, string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "app")
	r, err := Prepare(context.Background(), l.dir, out, d, testRepo+"@"+d, DefaultLimits)
	return r, out, err
}

func reason(err error) Reason {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return Reason("not a refusal: " + err.Error())
}

func TestPrepareConverts(t *testing.T) {
	l, _ := app(t, nil)
	d := l.finish(nil)
	r, out, err := prepare(t, l, d)
	if err != nil {
		t.Fatal(err)
	}
	if r.LockPath != out+"/spin.lock" || r.WorkingDir != out {
		t.Fatalf("result %+v", r)
	}
	raw, err := os.ReadFile(r.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	var lock map[string]any
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	md := lock["metadata"].(map[string]any)
	if md["origin"] != OriginScheme+testRepo+"@"+d || md["unknown_meta"] != "kept" || md["oci_image_digest"] != nil {
		t.Fatalf("metadata %v", md)
	}
	c := lock["components"].([]any)[0].(map[string]any)
	src := c["source"].(map[string]any)
	if len(src) != 2 || src["content_type"] != "application/wasm" || !strings.HasPrefix(src["source"].(string), "file://"+out+"/blobs/") {
		t.Fatalf("source %v", src)
	}
	wasm, err := os.ReadFile(strings.TrimPrefix(src["source"].(string), "file://"))
	if err != nil || string(wasm) != "\x00asm component" {
		t.Fatalf("wasm copy %q %v", wasm, err)
	}
	files := c["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["source"] != "file://"+out+"/assets/hello" || files[0].(map[string]any)["path"] != "/" {
		t.Fatalf("files %v", files)
	}
	for rel, want := range map[string]string{"static/index.html": "index", "a/arch.txt": "archived", "inline.txt": "inl"} {
		b, err := os.ReadFile(filepath.Join(out, "assets", "hello", rel))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", rel, b, err)
		}
	}
	// Sealed: directories 0555, files 0444.
	_ = filepath.Walk(out, func(p string, fi os.FileInfo, _ error) error {
		want := os.FileMode(0o444)
		if fi.IsDir() {
			want = 0o555
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %v", p, fi.Mode().Perm())
		}
		return nil
	})
	removeTree(out)
}

func TestPrepareLockAsConfig(t *testing.T) {
	l := newLayout(t)
	wasm := l.layer(mediaWasm, []byte("\x00asm"))
	lock := map[string]any{"spin_lock_version": 1, "triggers": []any{map[string]any{"id": "t", "trigger_type": "http", "trigger_config": map[string]any{}}},
		"components": []any{map[string]any{"id": "c", "source": map[string]any{"content_type": "application/wasm", "digest": wasm}}}}
	l.config = l.blob("application/vnd.oci.image.config.v1+json", mustJSON(t, lock))
	d := l.finish(nil)
	if _, out, err := prepare(t, l, d); err != nil {
		t.Fatal(err)
	} else {
		removeTree(out)
	}
}

func TestPrepareForgedSourcesAreReplaced(t *testing.T) {
	l, _ := app(t, func(lock map[string]any) {
		c := lock["components"].([]any)[0].(map[string]any)
		c["source"].(map[string]any)["source"] = "file:///etc/shadow"
		files := c["files"].([]any)
		files[0].(map[string]any)["source"] = "file:///var/lib/kubeswift-spin"
	})
	d := l.finish(nil)
	r, out, err := prepare(t, l, d)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(r.LockPath)
	if bytes.Contains(raw, []byte("/etc/shadow")) || bytes.Contains(raw, []byte("/var/lib/kubeswift-spin")) {
		t.Fatalf("forged source survived: %s", raw)
	}
	removeTree(out)
}

func TestPrepareRefuses(t *testing.T) {
	setLock := func(f func(lock map[string]any)) func(*testing.T) (*layout, string) {
		return func(t *testing.T) (*layout, string) {
			l, _ := app(t, f)
			return l, l.finish(nil)
		}
	}
	comp := func(lock map[string]any) map[string]any { return lock["components"].([]any)[0].(map[string]any) }
	cases := []struct {
		name  string
		build func(*testing.T) (*layout, string)
		from  func(d string) string
		want  Reason
	}{
		{"redis trigger", setLock(func(l map[string]any) {
			l["triggers"] = append(l["triggers"].([]any), map[string]any{"id": "r", "trigger_type": "redis", "trigger_config": map[string]any{}})
		}), nil, ReasonTriggers},
		{"no triggers", setLock(func(l map[string]any) { l["triggers"] = []any{} }), nil, ReasonTriggers},
		{"dependencies", setLock(func(l map[string]any) { comp(l)["dependencies"] = map[string]any{"x": map[string]any{}} }), nil, ReasonComponent},
		{"dependencies array", setLock(func(l map[string]any) { comp(l)["dependencies"] = []any{} }), nil, ReasonComponent},
		{"dependencies null", setLock(func(l map[string]any) { comp(l)["dependencies"] = nil }), nil, ReasonComponent},
		{"trigger dependencies", setLock(func(l map[string]any) { comp(l)["trigger_dependencies"] = map[string]any{"x": []any{}} }), nil, ReasonComponent},
		{"component id with slash", setLock(func(l map[string]any) { comp(l)["id"] = "../x" }), nil, ReasonComponent},
		{"component id dotdot", setLock(func(l map[string]any) { comp(l)["id"] = ".." }), nil, ReasonComponent},
		{"component id encoded slashes", setLock(func(l map[string]any) { comp(l)["id"] = "..%2F..%2F..%2F.." }), nil, ReasonComponent},
		{"component id encoded dot segment", setLock(func(l map[string]any) { comp(l)["id"] = "%2e%2e" }), nil, ReasonComponent},
		{"component id query", setLock(func(l map[string]any) { comp(l)["id"] = "a?b" }), nil, ReasonComponent},
		{"component id fragment", setLock(func(l map[string]any) { comp(l)["id"] = "a#b" }), nil, ReasonComponent},
		{"component id percent", setLock(func(l map[string]any) { comp(l)["id"] = "a%41" }), nil, ReasonComponent},
		{"component id backslash", setLock(func(l map[string]any) { comp(l)["id"] = "a\\b" }), nil, ReasonComponent},
		{"component id space", setLock(func(l map[string]any) { comp(l)["id"] = "a b" }), nil, ReasonComponent},
		{"component id leading digit", setLock(func(l map[string]any) { comp(l)["id"] = "1abc" }), nil, ReasonComponent},
		{"component id dot", setLock(func(l map[string]any) { comp(l)["id"] = "a.b" }), nil, ReasonComponent},
		{"component id underscore", setLock(func(l map[string]any) { comp(l)["id"] = "a_b" }), nil, ReasonComponent},
		{"component id empty word", setLock(func(l map[string]any) { comp(l)["id"] = "a--b" }), nil, ReasonComponent},
		{"component id trailing dash", setLock(func(l map[string]any) { comp(l)["id"] = "a-" }), nil, ReasonComponent},
		{"component id empty", setLock(func(l map[string]any) { comp(l)["id"] = "" }), nil, ReasonComponent},
		{"duplicate component", setLock(func(l map[string]any) { l["components"] = append(l["components"].([]any), comp(l)) }), nil, ReasonComponent},
		{"source without digest", setLock(func(l map[string]any) { delete(comp(l)["source"].(map[string]any), "digest") }), nil, ReasonComponent},
		{"files not an array", setLock(func(l map[string]any) { comp(l)["files"] = map[string]any{} }), nil, ReasonFile},
		{"path escape", setLock(func(l map[string]any) { comp(l)["files"].([]any)[0].(map[string]any)["path"] = "../x" }), nil, ReasonFile},
		{"absolute path", setLock(func(l map[string]any) { comp(l)["files"].([]any)[0].(map[string]any)["path"] = "/etc/x" }), nil, ReasonFile},
		{"dot path", setLock(func(l map[string]any) { comp(l)["files"].([]any)[0].(map[string]any)["path"] = "./x" }), nil, ReasonFile},
		{"empty element", setLock(func(l map[string]any) { comp(l)["files"].([]any)[0].(map[string]any)["path"] = "a//b" }), nil, ReasonFile},
		{"duplicate file path", setLock(func(l map[string]any) {
			fs := comp(l)["files"].([]any)
			fs[1].(map[string]any)["path"] = "static/index.html"
		}), nil, ReasonFile},
		{"padded inline", setLock(func(l map[string]any) { comp(l)["files"].([]any)[2].(map[string]any)["inline"] = "aW5s=" }), nil, ReasonFile},
		{"wasm digest on a data layer", setLock(func(l map[string]any) {
			comp(l)["source"].(map[string]any)["digest"] = comp(l)["files"].([]any)[0].(map[string]any)["digest"]
		}), nil, ReasonBlob},
		{"file digest on the wasm layer", setLock(func(l map[string]any) {
			comp(l)["files"].([]any)[0].(map[string]any)["digest"] = comp(l)["source"].(map[string]any)["digest"]
		}), nil, ReasonBlob},
		{"lock version 2", setLock(func(l map[string]any) { l["spin_lock_version"] = 2 }), nil, ReasonLock},
		{"bare wasm", setLock(func(l map[string]any) { delete(l, "spin_lock_version") }), nil, ReasonBareWasm},
		{"two lock layers", func(t *testing.T) (*layout, string) {
			l, _ := app(t, nil)
			l.layer(MediaLockedApp, []byte(`{"spin_lock_version":0}`))
			return l, l.finish(nil)
		}, nil, ReasonLock},
		{"manifest tampered", func(t *testing.T) (*layout, string) {
			l, _ := app(t, nil)
			d := l.finish(nil)
			p := filepath.Join(l.dir, "blobs", "sha256", d[7:])
			_ = os.Chmod(p, 0o644)
			_ = os.WriteFile(p, []byte(`{"schemaVersion":2}`), 0o444)
			return l, d
		}, nil, ReasonManifest},
		{"wasm tampered", func(t *testing.T) (*layout, string) {
			l, _ := app(t, nil)
			d := l.finish(nil)
			p := filepath.Join(l.dir, "blobs", "sha256", l.layers[0]["digest"].(string)[7:])
			_ = os.Chmod(p, 0o644)
			_ = os.WriteFile(p, []byte("\x00asm componenX"), 0o444)
			return l, d
		}, nil, ReasonBlob},
		{"wasm symlink", func(t *testing.T) (*layout, string) {
			l, _ := app(t, nil)
			d := l.finish(nil)
			p := filepath.Join(l.dir, "blobs", "sha256", l.layers[0]["digest"].(string)[7:])
			_ = os.Remove(p)
			if err := os.Symlink("/etc/hostname", p); err != nil {
				t.Fatal(err)
			}
			return l, d
		}, nil, ReasonBlob},
		{"case variant key", func(t *testing.T) (*layout, string) {
			l, _ := app(t, nil)
			raw := mustJSON(t, map[string]any{"schemaVersion": 2, "config": l.config, "layers": l.layers, "LAYERS": []any{}})
			return l, l.finish(raw)
		}, nil, ReasonManifest},
		{"duplicate key", func(t *testing.T) (*layout, string) {
			l, _ := app(t, nil)
			ls := string(mustJSON(t, l.layers))
			raw := []byte(`{"schemaVersion":2,"config":` + string(mustJSON(t, l.config)) + `,"layers":` + ls + `,"layers":[]}`)
			return l, l.finish(raw)
		}, nil, ReasonManifest},
		{"invalid utf-8 lock", func(t *testing.T) (*layout, string) {
			l, _ := app(t, nil)
			l.layers = l.layers[:len(l.layers)-1]
			l.layer(MediaLockedApp, []byte("{\"spin_lock_version\":0,\"metadata\":{\"name\":\"\xff\"}}"))
			return l, l.finish(nil)
		}, nil, ReasonLock},
		{"wrong digest", func(t *testing.T) (*layout, string) {
			l, _ := app(t, nil)
			l.finish(nil)
			return l, "sha256:" + strings.Repeat("0", 64)
		}, nil, ReasonLayout},
		{"no oci-layout", func(t *testing.T) (*layout, string) {
			l, _ := app(t, nil)
			d := l.finish(nil)
			_ = os.Remove(filepath.Join(l.dir, "oci-layout"))
			return l, d
		}, nil, ReasonLayout},
		{"unexpected layout version", func(t *testing.T) (*layout, string) {
			l, _ := app(t, nil)
			d := l.finish(nil)
			p := filepath.Join(l.dir, "oci-layout")
			_ = os.Chmod(p, 0o644)
			_ = os.WriteFile(p, []byte(`{"imageLayoutVersion":"2.0.0"}`), 0o444)
			return l, d
		}, nil, ReasonLayout},
		{"tag reference", func(t *testing.T) (*layout, string) { l, _ := app(t, nil); return l, l.finish(nil) },
			func(string) string { return testRepo + ":v1" }, ReasonReference},
		{"no registry host", func(t *testing.T) (*layout, string) { l, _ := app(t, nil); return l, l.finish(nil) },
			func(d string) string { return "apps/hello@" + d }, ReasonReference},
		{"bad digest", func(t *testing.T) (*layout, string) { l, _ := app(t, nil); l.finish(nil); return l, "sha256:ABC" },
			nil, ReasonDigest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, d := c.build(t)
			from := testRepo + "@" + d
			if c.from != nil {
				from = c.from(d)
			}
			out := filepath.Join(t.TempDir(), "app")
			_, err := Prepare(context.Background(), l.dir, out, d, from, DefaultLimits)
			if err == nil {
				t.Fatal("accepted")
			}
			if got := reason(err); got != c.want {
				t.Fatalf("reason %q, want %q", got, c.want)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("output left behind: %v", err)
			}
		})
	}
}

func TestPrepareLimitsAndDeadline(t *testing.T) {
	l, _ := app(t, nil)
	d := l.finish(nil)
	small := DefaultLimits
	small.Blob = 4
	if _, err := Prepare(context.Background(), l.dir, filepath.Join(t.TempDir(), "a"), d, testRepo+"@"+d, small); reason(err) != ReasonLimit {
		t.Fatalf("blob limit: %v", err)
	}
	few := DefaultLimits
	few.Files = 2
	if _, err := Prepare(context.Background(), l.dir, filepath.Join(t.TempDir(), "a"), d, testRepo+"@"+d, few); reason(err) != ReasonLimit {
		t.Fatalf("file limit: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prepare(ctx, l.dir, filepath.Join(t.TempDir(), "a"), d, testRepo+"@"+d, DefaultLimits); reason(err) != ReasonDeadline {
		t.Fatalf("deadline: %v", err)
	}
	existing := t.TempDir()
	if _, err := Prepare(context.Background(), l.dir, existing, d, testRepo+"@"+d, DefaultLimits); reason(err) != ReasonOutput {
		t.Fatalf("existing output: %v", err)
	}
}

func TestDecodeStrict(t *testing.T) {
	for _, bad := range []string{`{"a":1,"a":2}`, `{"x":{"b":1,"b":1}}`, `[{"k":1,"k":1}]`, "{\"a\":\"\xff\"}", `{"a":1} {}`} {
		if _, err := decodeStrict([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, good := range []string{`{"a":1,"b":{"a":1}}`, `[{"k":1},{"k":2}]`, `{"n":9007199254740993}`} {
		if _, err := decodeStrict([]byte(good)); err != nil {
			t.Errorf("rejected %q: %v", good, err)
		}
	}
	v, _ := decodeStrict([]byte(`{"n":9007199254740993}`))
	b, _ := encode(v)
	if string(b) != `{"n":9007199254740993}` {
		t.Errorf("number not kept: %s", b)
	}
}

// FuzzDecodeStrict checks that the strict decoder never accepts a document
// encoding/json would read differently: if it accepts, a second pass over
// its own output yields the same value.
func FuzzDecodeStrict(f *testing.F) {
	for _, s := range []string{`{"a":1}`, `{"a":{"b":[1,2,{"c":"d"}]}}`, `{"a":1,"a":2}`, `{"LAYERS":[],"layers":[]}`, `[1,"x",null,true]`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, doc []byte) {
		v, err := decodeStrict(doc)
		if err != nil {
			return
		}
		b, err := encode(v)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		v2, err := decodeStrict(b)
		if err != nil {
			t.Fatalf("own output rejected: %v", err)
		}
		b2, _ := encode(v2)
		if !bytes.Equal(b, b2) {
			t.Fatalf("not stable: %s vs %s", b, b2)
		}
	})
}

// spinDecodedPath decodes a file:// URL as Spin 4.2.5 does (url crate:
// WHATWG parsing, then percent-decoding each path segment), refusing what
// would not be a plain path.
func spinDecodedPath(t *testing.T, s string) string {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "file" || u.Host != "" || u.RawQuery != "" || u.Fragment != "" {
		t.Fatalf("emitted URL %q is not a plain file URL", s)
	}
	var segs []string
	for _, seg := range strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/") {
		lower := strings.ToLower(seg)
		if lower == "." || lower == ".." || lower == "%2e" || lower == "%2e%2e" || lower == ".%2e" || lower == "%2e." {
			t.Fatalf("emitted URL %q has a dot segment", s)
		}
		d, err := url.PathUnescape(seg)
		if err != nil || strings.ContainsAny(d, "/\\\x00") {
			t.Fatalf("emitted URL %q decodes a segment to %q", s, d)
		}
		segs = append(segs, d)
	}
	return "/" + strings.Join(segs, "/")
}

// emittedSources returns every file:// source in a converted lock.
func emittedSources(t *testing.T, lockPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var lock map[string]any
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, cv := range lock["components"].([]any) {
		c := cv.(map[string]any)
		out = append(out, c["source"].(map[string]any)["source"].(string))
		if fs, ok := c["files"].([]any); ok {
			for _, f := range fs {
				out = append(out, f.(map[string]any)["source"].(string))
			}
		}
	}
	return out
}

// FuzzComponentID: whatever the component id, the conversion is refused or
// every emitted file:// source decodes, as Spin decodes it, to a path
// beneath the output root.
func FuzzComponentID(f *testing.F) {
	for _, s := range []string{"hello", "a-b", "..%2F..%2Fx", "%2e%2e", "a?b", "a#b", "A-B2", "x%00", "é"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, id string) {
		l, _ := app(t, func(lock map[string]any) { lock["components"].([]any)[0].(map[string]any)["id"] = id })
		d := l.finish(nil)
		r, out, err := prepare(t, l, d)
		if err != nil {
			return
		}
		defer removeTree(out)
		for _, s := range emittedSources(t, r.LockPath) {
			if p := spinDecodedPath(t, s); !strings.HasPrefix(p, out+"/") {
				t.Fatalf("id %q: source %q decodes to %q, outside %q", id, s, p, out)
			}
		}
	})
}

func TestEmittedSourcesStayBeneathOutput(t *testing.T) {
	l, _ := app(t, nil)
	d := l.finish(nil)
	r, out, err := prepare(t, l, d)
	if err != nil {
		t.Fatal(err)
	}
	defer removeTree(out)
	srcs := emittedSources(t, r.LockPath)
	if len(srcs) < 2 {
		t.Fatalf("sources %v", srcs)
	}
	for _, s := range srcs {
		if p := spinDecodedPath(t, s); !strings.HasPrefix(p, out+"/") {
			t.Fatalf("source %q decodes to %q", s, p)
		}
	}
}

// Archive entries that are not regular files are skipped, but their
// decompressed bytes count against the total limit.
func TestPrepareArchiveInflateLimit(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	big := bytes.Repeat([]byte{0}, 1<<20)
	if err := tw.WriteHeader(&tar.Header{Name: "skipped", Mode: 0o644, Size: int64(len(big)), Typeflag: 'Z'}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(big)
	if err := tw.WriteHeader(&tar.Header{Name: "x/arch.txt", Mode: 0o644, Size: 8, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("archived"))
	_ = tw.Close()
	_ = zw.Close()
	l := newLayout(t)
	wasm := l.layer(mediaWasm, []byte("\x00asm component"))
	l.layer(MediaArchive, buf.Bytes())
	l.layer(MediaLockedApp, mustJSON(t, map[string]any{
		"spin_lock_version": 0,
		"triggers":          []any{map[string]any{"id": "t", "trigger_type": "http", "trigger_config": map[string]any{"component": "c", "route": "/..."}}},
		"components": []any{map[string]any{"id": "c",
			"source": map[string]any{"content_type": "application/wasm", "digest": wasm},
			"files":  []any{map[string]any{"digest": digestOf([]byte("archived")), "path": "a.txt"}}}},
	}))
	d := l.finish(nil)
	small := DefaultLimits
	small.Total = 256 << 10
	if _, err := Prepare(context.Background(), l.dir, filepath.Join(t.TempDir(), "a"), d, testRepo+"@"+d, small); reason(err) != ReasonLimit {
		t.Fatalf("inflate limit: %v", err)
	}
	r, out, err := prepare(t, l, d)
	if err != nil {
		t.Fatalf("default limits: %v", err)
	}
	_ = r
	removeTree(out)
}
