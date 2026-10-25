package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
)

// testLayout writes a minimal valid Spin application layout (one Wasm
// component, an http trigger) and returns its directory and manifest digest.
func testLayout(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(b []byte) map[string]any {
		s := sha256.Sum256(b)
		d := "sha256:" + hex.EncodeToString(s[:])
		if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", d[7:]), b, 0o444); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"digest": d, "size": len(b)}
	}
	wasm := put([]byte("\x00asm"))
	wasm["mediaType"] = "application/vnd.wasm.content.layer.v1+wasm"
	lock, _ := json.Marshal(map[string]any{"spin_lock_version": 0,
		"triggers":   []any{map[string]any{"id": "t", "trigger_type": "http", "trigger_config": map[string]any{"component": "c", "route": "/..."}}},
		"components": []any{map[string]any{"id": "c", "source": map[string]any{"content_type": "application/wasm", "digest": wasm["digest"]}}}})
	l := put(lock)
	l["mediaType"] = "application/vnd.fermyon.spin.application.v1+config"
	cfg := put([]byte("{}"))
	cfg["mediaType"] = "application/vnd.oci.image.config.v1+json"
	man, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": cfg, "layers": []any{wasm, l}})
	m := put(man)
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": m["digest"], "size": m["size"]}}})
	_ = os.WriteFile(filepath.Join(dir, "index.json"), idx, 0o444)
	_ = os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o444)
	return dir, m["digest"].(string)
}

// mounted fakes the mount table: dir is a mount point with the given options.
func mounted(dir, opts string) cacheFS {
	return cacheFS{readFile: func(p string) ([]byte, error) {
		if p == "/proc/self/mountinfo" {
			return []byte("100 1 0:50 / " + dir + " " + opts + " - virtiofs sbxart0 " + opts + "\n"), nil
		}
		return os.ReadFile(p)
	}, readMeta: readRegular, lstat: os.Lstat}
}

func localPlan(digest string, extra ...string) *plan {
	argv := append([]string{"spin", "up", "--from=registry.example.com/apps/hello@" + digest, "--listen=0.0.0.0:3000"}, extra...)
	return &plan{argv: argv, env: []string{"HOME=/h", "SPIN_VARIABLE_X=1"}, localDigest: digest}
}

func TestUseLocalApp(t *testing.T) {
	dir, d := testLayout(t)
	out := filepath.Join(t.TempDir(), "app")
	p := localPlan(d, "--runtime-config-file=/x.toml")
	if why := planLocalApp(p, mounted(dir, "ro,relatime"), dir, out, runtimecontract.LocalAppSpinVersion); why != "" {
		t.Fatalf("not used: %s", why)
	}
	defer removeOut(out)
	if want := []string{"spin", "trigger", "http", "--listen=0.0.0.0:3000", "--runtime-config-file=/x.toml"}; !slices.Equal(p.argv, want) {
		t.Fatalf("argv %v", p.argv)
	}
	if want := []string{"HOME=/h", "SPIN_VARIABLE_X=1", "SPIN_LOCKED_URL=file://" + out + "/spin.lock", "SPIN_WORKING_DIR=" + out}; !slices.Equal(p.env, want) {
		t.Fatalf("env %v", p.env)
	}
	if _, err := os.Stat(out + "/spin.lock"); err != nil {
		t.Fatal(err)
	}
}

func removeOut(out string) {
	_ = filepath.Walk(out, func(p string, fi os.FileInfo, _ error) error {
		if fi != nil && fi.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(out)
}

func TestUseLocalAppFallsBack(t *testing.T) {
	dir, d := testLayout(t)
	other := "sha256:" + strings.Repeat("0", 64)
	cases := []struct {
		name, want string
		plan       *plan
		fsys       cacheFS
		supported  string
	}{
		{"unsupported spin", "not supported by the local runner", localPlan(d), mounted(dir, "ro"), ""},
		{"component selection", "--component-id is not supported", localPlan(d, "--component-id=c"), mounted(dir, "ro"), runtimecontract.LocalAppSpinVersion},
		{"not mounted", "no application is mounted", localPlan(d), mounted("/elsewhere", "ro"), runtimecontract.LocalAppSpinVersion},
		{"writable mount", "the application mount is writable", localPlan(d), mounted(dir, "rw,relatime"), runtimecontract.LocalAppSpinVersion},
		{"other digest", "OCI layout", localPlan(other), mounted(dir, "ro"), runtimecontract.LocalAppSpinVersion},
		{"tag reference", "not pinned", &plan{argv: []string{"spin", "up", "--from=registry.example.com/apps/hello:v1", "--listen=0.0.0.0:3000"}, localDigest: d},
			mounted(dir, "ro"), runtimecontract.LocalAppSpinVersion},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := slices.Clone(c.plan.argv)
			envBefore := slices.Clone(c.plan.env)
			out := filepath.Join(t.TempDir(), "app")
			why := planLocalApp(c.plan, c.fsys, dir, out, c.supported)
			if !strings.Contains(why, c.want) {
				t.Fatalf("reason %q, want it to mention %q", why, c.want)
			}
			// The fallback plan is the plan with the feature off.
			if !slices.Equal(c.plan.argv, before) || !slices.Equal(c.plan.env, envBefore) {
				t.Fatalf("plan changed: %v %v", c.plan.argv, c.plan.env)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("output left behind: %v", err)
			}
		})
	}
}

func TestReservedEnvironment(t *testing.T) {
	environ := []string{
		"SPIN_LOCKED_URL=file:///evil", "SPIN_WORKING_DIR=/", "SPIN_LOCAL_APP_DIR=/x", "SPIN_LOG_DIR=/x",
		"RUNTIME_CONFIG_FILE=/x", "SPIN_HTTP_LISTEN_ADDR=0.0.0.0:1", "SPIN_TLS_CERT=/c", "SPIN_TLS_KEY=/k",
		runtimecontract.LocalAppDigestEnv + "=sha256:" + strings.Repeat("a", 64), "KUBESWIFT_SPIN_ANYTHING=x",
		"SPIN_VARIABLE_KEEP=1",
	}
	p, err := buildPlan([]string{"up", "--from=r.example/a@sha256:" + strings.Repeat("a", 64)}, environ, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range p.env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "KUBESWIFT_SPIN_") || (strings.HasPrefix(k, "SPIN_") && k != "SPIN_VARIABLE_KEEP") || k == "RUNTIME_CONFIG_FILE" {
			t.Errorf("%s reached Spin", k)
		}
	}
	if len(p.dropped) != 8 {
		t.Errorf("dropped %v", p.dropped)
	}
	if p.localDigest != "sha256:"+strings.Repeat("a", 64) {
		t.Errorf("digest %q", p.localDigest)
	}
}
