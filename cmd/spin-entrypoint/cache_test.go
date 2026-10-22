package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
)

func testWant() cacheExpectation {
	return cacheExpectation{source: testSource, runtime: testRuntime, architecture: runtime.GOARCH}
}

func goodMetadata() runtimecontract.CacheMetadata {
	return runtimecontract.CacheMetadata{
		Kind: runtimecontract.CompiledCacheKind, Source: testSource, Runtime: testRuntime,
		Architecture: runtime.GOARCH, Spin: "spin 4.2.1", WasmtimeCache: runtimecontract.WasmtimeCacheVersion,
		Entries: []string{strings.Repeat("A", 43)}, Components: []string{"hello"},
	}
}

func mountLine(dir, opts string) string {
	return "36 35 0:30 / " + dir + " " + opts + " master:1 - virtiofs staging " + opts + "\n"
}

// cacheDir writes a cache tree with md under a temporary directory and
// returns the directory and an fs whose mount table holds mounts.
func cacheDir(t *testing.T, md any, mounts func(dir string) string) (string, cacheFS) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "opt", "kubeswift-spin-cache")
	if err := os.MkdirAll(filepath.Join(dir, "cache", "modules", runtimecontract.WasmtimeCacheVersion), 0o755); err != nil {
		t.Fatal(err)
	}
	if md != nil {
		b, _ := json.Marshal(md)
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), b, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	table := "22 1 8:1 / / rw,relatime - ext4 /dev/vda rw\n" + mounts(dir)
	return dir, cacheFS{
		readFile: func(p string) ([]byte, error) {
			if p == "/proc/self/mountinfo" {
				return []byte(table), nil
			}
			return os.ReadFile(p)
		},
		lstat: os.Lstat,
	}
}

func TestCompiledCacheConfigUsesAVerifiedMount(t *testing.T) {
	dir, fsys := cacheDir(t, goodMetadata(), func(d string) string { return mountLine(d, "ro,nosuid,nodev,relatime") })
	cfg, why := compiledCacheConfig(fsys, dir, testWant())
	if cfg == nil {
		t.Fatalf("cache not used: %s", why)
	}
	want := "[cache]\ndirectory = \"" + filepath.Join(dir, "cache") + "\"\n"
	if string(cfg) != want {
		t.Fatalf("config = %q, want %q", cfg, want)
	}
}

func TestCompiledCacheConfigRefuses(t *testing.T) {
	ro := func(d string) string { return mountLine(d, "ro,nosuid,nodev") }
	other := func(f func(*runtimecontract.CacheMetadata)) runtimecontract.CacheMetadata {
		md := goodMetadata()
		f(&md)
		return md
	}
	cases := []struct {
		name   string
		md     any
		mounts func(string) string
		want   string
	}{
		{"not a mount point", goodMetadata(), func(string) string { return "" }, "no cache is mounted"},
		{"writable mount", goodMetadata(), func(d string) string { return mountLine(d, "rw,nosuid") }, "writable"},
		{"writable mount over a read-only one", goodMetadata(), func(d string) string { return mountLine(d, "ro") + mountLine(d, "rw") }, "writable"},
		{"mount of a parent only", goodMetadata(), func(d string) string { return mountLine(filepath.Dir(d), "ro") }, "no cache is mounted"},
		{"no metadata", nil, ro, "missing"},
		{"unknown field", map[string]any{"kind": runtimecontract.CompiledCacheKind, "extra": 1}, ro, "not compiled cache metadata"},
		{"other kind", other(func(m *runtimecontract.CacheMetadata) { m.Kind = "x" }), ro, "another kind"},
		{"other source", other(func(m *runtimecontract.CacheMetadata) { m.Source = testRuntime }), ro, "another application"},
		{"other runtime", other(func(m *runtimecontract.CacheMetadata) { m.Runtime = testSource }), ro, "another runtime image"},
		{"other architecture", other(func(m *runtimecontract.CacheMetadata) { m.Architecture = "riscv64" }), ro, "another architecture"},
		{"other Wasmtime version", other(func(m *runtimecontract.CacheMetadata) { m.WasmtimeCache = "wasmtime-1.0.0" }), ro, "Wasmtime cache version"},
		{"no entries", other(func(m *runtimecontract.CacheMetadata) { m.Entries = nil }), ro, "no entries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, fsys := cacheDir(t, tc.md, tc.mounts)
			cfg, why := compiledCacheConfig(fsys, dir, testWant())
			if cfg != nil || !strings.Contains(why, tc.want) {
				t.Fatalf("got config %q, reason %q; want refusal containing %q", cfg, why, tc.want)
			}
		})
	}
}

func TestCompiledCacheConfigRefusesALinkedEntryDirectory(t *testing.T) {
	dir, fsys := cacheDir(t, goodMetadata(), func(d string) string { return mountLine(d, "ro") })
	v := filepath.Join(dir, "cache", "modules", runtimecontract.WasmtimeCacheVersion)
	if err := os.Remove(v); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), v); err != nil {
		t.Fatal(err)
	}
	if cfg, why := compiledCacheConfig(fsys, dir, testWant()); cfg != nil || !strings.Contains(why, "entry directory") {
		t.Fatalf("got config %q, reason %q", cfg, why)
	}
}

func TestCompiledCacheConfigWantsTheBuildArchitecture(t *testing.T) {
	dir, fsys := cacheDir(t, goodMetadata(), func(d string) string { return mountLine(d, "ro") })
	want := testWant()
	want.architecture = "riscv64"
	if cfg, _ := compiledCacheConfig(fsys, dir, want); cfg != nil {
		t.Fatal("a cache for another architecture than the controller expects was used")
	}
}

func TestMountStateUnescapes(t *testing.T) {
	table := "36 35 0:30 / /opt/with\\040space ro - virtiofs s ro\n"
	ro, mounted, err := mountState(func(string) ([]byte, error) { return []byte(table), nil }, "/opt/with space")
	if err != nil || !mounted || !ro {
		t.Fatalf("ro=%v mounted=%v err=%v", ro, mounted, err)
	}
}

func TestBuildPlanReservesTheWasmtimeCacheVariables(t *testing.T) {
	env := []string{
		"WASMTIME_CACHE_FILE=/tmp/evil.toml",
		"DISABLE_WASMTIME_CACHE=1",
		runtimecontract.CompiledCacheSourceEnv + "=" + testSource,
		runtimecontract.CompiledCacheRuntimeEnv + "=" + testRuntime,
		runtimecontract.CompiledCacheArchitectureEnv + "=" + runtime.GOARCH,
		"SPIN_VARIABLE_GREETING=hi",
	}
	p, err := buildPlan([]string{"up", "--listen=0.0.0.0:3000"}, env, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"WASMTIME_CACHE_FILE", "DISABLE_WASMTIME_CACHE", runtimecontract.CompiledCacheSourceEnv,
		runtimecontract.CompiledCacheRuntimeEnv, runtimecontract.CompiledCacheArchitectureEnv} {
		if _, ok := envValue(p.env, k); ok {
			t.Errorf("%s reached Spin", k)
		}
	}
	if !slices.Equal(p.dropped, []string{"WASMTIME_CACHE_FILE", "DISABLE_WASMTIME_CACHE"}) {
		t.Errorf("dropped = %v", p.dropped)
	}
	if p.cache == nil || *p.cache != testWant() {
		t.Fatalf("cache expectation = %+v", p.cache)
	}

	// Without a delivered cache, Spin gets no cache file.
	useCompiledCache(p, cacheFS{readFile: func(string) ([]byte, error) { return []byte(""), nil }, lstat: os.Lstat}, runtimecontract.CompiledCacheDir)
	if _, ok := envValue(p.env, "WASMTIME_CACHE_FILE"); ok || p.cacheConfig != nil {
		t.Fatal("a cache was configured without a mount")
	}

	// With one, Spin gets the entrypoint's own configuration.
	dir, fsys := cacheDir(t, goodMetadata(), func(d string) string { return mountLine(d, "ro") })
	useCompiledCache(p, fsys, dir)
	if v, _ := envValue(p.env, "WASMTIME_CACHE_FILE"); v != runtimecontract.CompiledCacheConfigPath || p.cacheConfig == nil {
		t.Fatalf("WASMTIME_CACHE_FILE = %q", v)
	}
}

func TestBuildPlanNeedsTheWholeExpectation(t *testing.T) {
	p, err := buildPlan([]string{"up"}, []string{runtimecontract.CompiledCacheSourceEnv + "=" + testSource}, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if p.cache != nil {
		t.Fatal("a partial expectation asked for a cache")
	}
	p, err = buildPlan([]string{"--version"}, []string{
		runtimecontract.CompiledCacheSourceEnv + "=" + testSource,
		runtimecontract.CompiledCacheRuntimeEnv + "=" + testRuntime,
		runtimecontract.CompiledCacheArchitectureEnv + "=" + runtime.GOARCH,
	}, noFiles)
	if err != nil || p.cache != nil {
		t.Fatalf("--version asked for a cache (err %v)", err)
	}
}
