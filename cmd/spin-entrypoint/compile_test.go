package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
)

const (
	testSource  = "ghcr.io/org/app@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testRuntime = "ghcr.io/kubeswift-io/kubeswift-spin-runtime@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func TestParseCompileArgs(t *testing.T) {
	ok := []string{"--source=" + testSource, "--runtime=" + testRuntime, "--architecture=amd64"}
	o, err := parseCompileArgs(ok)
	if err != nil || o.source != testSource || o.runtime != testRuntime || o.architecture != "amd64" {
		t.Fatalf("%+v %v", o, err)
	}
	for name, args := range map[string][]string{
		"missing source":    ok[1:],
		"tagged source":     {"--source=ghcr.io/org/app:v1", ok[1], ok[2]},
		"tagged runtime":    {ok[0], "--runtime=ghcr.io/x/rt:v1", ok[2]},
		"architecture":      {ok[0], ok[1], "--architecture=s390x"},
		"duplicate":         append(append([]string(nil), ok...), "--source="+testSource),
		"unknown flag":      append(append([]string(nil), ok...), "--listen=0.0.0.0:80"),
		"positional":        append(append([]string(nil), ok...), "up"),
		"space separated":   {"--source", testSource, ok[1], ok[2]},
		"source whitespace": {"--source=ghcr.io/org/app @sha256:1111111111111111111111111111111111111111111111111111111111111111", ok[1], ok[2]},
	} {
		if _, err := parseCompileArgs(args); err == nil {
			t.Errorf("%s: accepted %q", name, args)
		}
	}
}

func lockJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPlanCompile(t *testing.T) {
	app := map[string]any{
		"spin_lock_version": 1,
		"variables": map[string]any{
			"api_host":  map[string]any{},                   // required, no value: placeholder
			"api_token": map[string]any{"secret": true},     // required, secret: placeholder
			"greeting":  map[string]any{"default": "hello"}, // has a default
			"region":    map[string]any{},                   // required, set by the controller
		},
		"triggers": []any{
			map[string]any{"trigger_type": "http", "trigger_config": map[string]any{"component": "api", "route": "/api/..."}},
			map[string]any{"trigger_type": "http", "trigger_config": map[string]any{"component": "web", "route": "/..."}},
			map[string]any{"trigger_type": "redis", "trigger_config": map[string]any{"component": "worker", "channel": "c"}},
		},
		"components": []any{
			map[string]any{"id": "api", "metadata": map[string]any{"key_value_stores": []string{"default", "cache"}, "databases": []string{"default"}}},
			map[string]any{"id": "web", "metadata": map[string]any{"key_value_stores": []string{"cache", "sessions"}}},
			map[string]any{"id": "worker", "metadata": map[string]any{"key_value_stores": []string{"queue"}}},
		},
	}
	p, err := planCompile(lockJSON(t, app), map[string]string{"SPIN_VARIABLE_REGION": "eu"}, "/state")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.components, []string{"api", "web"}) || !p.filter {
		t.Errorf("components %v filter %v", p.components, p.filter)
	}
	if !reflect.DeepEqual(p.placeholders, []string{"SPIN_VARIABLE_API_HOST", "SPIN_VARIABLE_API_TOKEN"}) {
		t.Errorf("placeholders %v", p.placeholders)
	}
	var rc map[string]map[string]map[string]string
	if err := toml.Unmarshal(p.runtimeConfig, &rc); err != nil {
		t.Fatal(err)
	}
	// The default label needs no configuration, the worker's store is not
	// loaded, and every store lives under the compile state directory.
	want := map[string]map[string]map[string]string{"key_value_store": {
		"cache":    {"type": "spin", "path": "/state/kv-0.db"},
		"sessions": {"type": "spin", "path": "/state/kv-1.db"},
	}}
	if !reflect.DeepEqual(rc, want) {
		t.Errorf("runtime config %v", rc)
	}

	httpOnly := map[string]any{"triggers": []any{map[string]any{"trigger_type": "http", "trigger_config": map[string]any{"component": "hello"}}},
		"components": []any{map[string]any{"id": "hello"}}}
	p, err = planCompile(lockJSON(t, httpOnly), nil, "/state")
	if err != nil || p.filter || len(p.placeholders) != 0 || strings.TrimSpace(string(p.runtimeConfig)) != "" {
		t.Errorf("an HTTP-only app needs no filter, placeholders or stores: %+v %v", p, err)
	}

	for name, doc := range map[string]any{
		"no http trigger":   map[string]any{"triggers": []any{map[string]any{"trigger_type": "redis", "trigger_config": map[string]any{"component": "w"}}}},
		"inline component":  map[string]any{"triggers": []any{map[string]any{"trigger_type": "http", "trigger_config": map[string]any{"component": map[string]any{"source": "x"}}}}},
		"bad variable name": map[string]any{"triggers": httpOnly["triggers"], "variables": map[string]any{"Bad-Name": map[string]any{}}},
		"bad store label": map[string]any{"triggers": httpOnly["triggers"], "components": []any{map[string]any{"id": "hello", "metadata": map[string]any{
			"key_value_stores": []string{"a\"]\n[x"}}}}},
	} {
		if _, err := planCompile(lockJSON(t, doc), nil, "/state"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := planCompile([]byte("{"), nil, "/state"); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestFindLockedApp(t *testing.T) {
	dir := t.TempDir()
	if _, err := findLockedApp(dir); err == nil {
		t.Fatal("an empty cache must fail")
	}
	a := filepath.Join(dir, "registry", "manifests", "ghcr.io", "org", "app", "latest")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "config.json"), []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "manifest.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := findLockedApp(dir); err != nil || string(b) != `{"x":1}` {
		t.Fatalf("%s %v", b, err)
	}
	// Spin versions that also store it under the digest: identical copies.
	b := filepath.Join(dir, "registry", "manifests", "ghcr.io", "org", "app", "@sha256_11")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "config.json"), []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := findLockedApp(dir); err != nil {
		t.Fatalf("identical copies: %v", err)
	}
	if err := os.WriteFile(filepath.Join(b, "config.json"), []byte(`{"x":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := findLockedApp(dir); err == nil {
		t.Fatal("two different locked apps must fail")
	}
}

func TestCollectAndWriteOutput(t *testing.T) {
	cache := t.TempDir()
	v := filepath.Join(cache, "modules", "wasmtime-49.0.1")
	if err := os.MkdirAll(v, 0o700); err != nil {
		t.Fatal(err)
	}
	keyA, keyB := strings.Repeat("A", 43), "h2vHAWfmmSvc16upNQY0rQgFufcHV6U0Cc0qfO5VI6E"
	for name, body := range map[string]string{keyA: "code-a", keyB: "code-b", keyB + ".stats": "1", ".cleanup.wip-1": "", "tmp-x": ""} {
		if err := os.WriteFile(filepath.Join(v, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	version, entries, err := collectEntries(cache)
	if err != nil || version != "wasmtime-49.0.1" || !reflect.DeepEqual(entries, []string{keyA, keyB}) {
		t.Fatalf("%s %v %v", version, entries, err)
	}
	out := t.TempDir()
	meta := runtimecontract.CacheMetadata{Kind: runtimecontract.CompiledCacheKind, Source: testSource, Runtime: testRuntime,
		Architecture: "amd64", Spin: "spin 4.2.1", WasmtimeCache: version, Entries: entries, Components: []string{"hello"}}
	if err := writeOutput(out, cache, meta); err != nil {
		t.Fatal(err)
	}
	var files []string
	_ = filepath.WalkDir(out, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(out, p)
			files = append(files, rel)
		}
		return nil
	})
	want := []string{"cache/modules/wasmtime-49.0.1/" + keyA, "cache/modules/wasmtime-49.0.1/" + keyB, "metadata.json"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("output %v", files)
	}
	var back runtimecontract.CacheMetadata
	raw, _ := os.ReadFile(filepath.Join(out, "metadata.json"))
	if err := json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(back, meta) {
		t.Fatalf("metadata %+v %v", back, err)
	}

	// Errors: no cache, two versions, no entries, an entry that is a link.
	if _, _, err := collectEntries(t.TempDir()); err == nil {
		t.Error("missing cache accepted")
	}
	two := t.TempDir()
	for _, d := range []string{"wasmtime-49.0.1", "wasmtime-50.0.0"} {
		_ = os.MkdirAll(filepath.Join(two, "modules", d), 0o700)
	}
	if _, _, err := collectEntries(two); err == nil {
		t.Error("two version directories accepted")
	}
	none := t.TempDir()
	_ = os.MkdirAll(filepath.Join(none, "modules", "wasmtime-49.0.1"), 0o700)
	if _, _, err := collectEntries(none); err == nil {
		t.Error("no entries accepted")
	}
	link := t.TempDir()
	lv := filepath.Join(link, "modules", "wasmtime-49.0.1")
	_ = os.MkdirAll(lv, 0o700)
	if err := os.Symlink("/etc/passwd", filepath.Join(lv, keyA)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collectEntries(link); err == nil {
		t.Error("a symlink entry accepted")
	}
}

func TestCheckEmptyDir(t *testing.T) {
	d := t.TempDir()
	if err := checkEmptyDir(d); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(d, "x"), nil, 0o600)
	if err := checkEmptyDir(d); err == nil {
		t.Error("non-empty accepted")
	}
	if err := checkEmptyDir(filepath.Join(d, "missing")); err == nil {
		t.Error("missing accepted")
	}
}

// Root never copies a hard link or follows a symbolic link the Spin user
// planted among the cache entries.
func TestWriteOutputRefusesLinks(t *testing.T) {
	for _, kind := range []string{"hardlink", "symlink"} {
		cache := t.TempDir()
		v := filepath.Join(cache, "modules", "wasmtime-49.0.1")
		if err := os.MkdirAll(v, 0o700); err != nil {
			t.Fatal(err)
		}
		secret := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(secret, []byte("root only"), 0o600); err != nil {
			t.Fatal(err)
		}
		entry := filepath.Join(v, strings.Repeat("A", 43))
		var err error
		if kind == "hardlink" {
			err = os.Link(secret, entry)
		} else {
			err = os.Symlink(secret, entry)
		}
		if err != nil {
			t.Fatal(err)
		}
		meta := runtimecontract.CacheMetadata{WasmtimeCache: "wasmtime-49.0.1", Entries: []string{strings.Repeat("A", 43)}}
		if err := writeOutput(t.TempDir(), cache, meta); err == nil {
			t.Errorf("%s: copied", kind)
		}
	}
}

func TestPlanCompileComponentIDs(t *testing.T) {
	for _, id := range []string{"Hello", "a_b", "../x", "-a", "a-", strings.Repeat("a", 129)} {
		doc := map[string]any{"triggers": []any{map[string]any{"trigger_type": "http", "trigger_config": map[string]any{"component": id}}}}
		if _, err := planCompile(lockJSON(t, doc), nil, "/state"); err == nil {
			t.Errorf("component ID %q accepted", id)
		}
	}
	doc := map[string]any{"triggers": []any{map[string]any{"trigger_type": "http", "trigger_config": map[string]any{"component": "my-component-2"}}}}
	if _, err := planCompile(lockJSON(t, doc), nil, "/state"); err != nil {
		t.Errorf("a valid component ID was refused: %v", err)
	}
}
