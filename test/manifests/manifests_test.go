// Package manifests checks that every SpinApp and SpinAppExecutor manifest
// shipped in examples/ and config/ is accepted by the same code the
// controller runs, so documentation cannot drift from behavior.
package manifests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/compatibility"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

const root = "../.."

var v16 = capabilities.Sandbox{Ports: true, ReadinessProbe: true, PodMetadata: true, SecretFiles: true, Egress: true}

func defaults() executor.Defaults {
	return executor.Defaults{
		RuntimeImage:  "ghcr.io/kubeswift-io/kubeswift-spin-runtime:v0.1.0",
		DefaultCPU:    resource.MustParse("1"),
		DefaultMemory: resource.MustParse("512Mi"),
	}
}

func glob(t *testing.T, pattern string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, pattern))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func executors(t *testing.T) map[string]*executor.Profile {
	t.Helper()
	out := map[string]*executor.Profile{}
	for _, f := range glob(t, "config/executor/*.yaml") {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var e spinv1alpha1.SpinAppExecutor
		if err := yaml.UnmarshalStrict(b, &e); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if !executor.IsManaged(&e) {
			t.Fatalf("%s: executor is not labelled as managed by kubeswift-spin", f)
		}
		p, err := executor.Parse(&e, defaults())
		if err == nil {
			err = p.CheckFeatures(v16)
		}
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[e.Name] = p
	}
	if len(out) == 0 {
		t.Fatal("no executor manifests found")
	}
	return out
}

func TestExecutorManifests(t *testing.T) {
	ex := executors(t)
	for _, name := range []string{"kubeswift", "kubeswift-open", "kubeswift-warm", "kubeswift-egress"} {
		if ex[name] == nil {
			t.Errorf("config/executor lacks %s", name)
		}
	}
}

func TestExampleSpinApps(t *testing.T) {
	ex := executors(t)
	files := append(glob(t, "examples/*/spinapp*.yaml"), glob(t, "examples/experimental/*/spinapp*.yaml")...)
	if len(files) < 6 {
		t.Fatalf("expected a SpinApp manifest per example, found %d", len(files))
	}
	// Examples target KubeSwift v0.16.0 or later.
	opts := compatibility.Options{MaxReplicas: 20, Resources: translate.DefaultResourcePolicy(), Features: v16}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var app spinv1alpha1.SpinApp
		if err := yaml.UnmarshalStrict(b, &app); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		p := ex[app.Spec.Executor]
		if p == nil {
			t.Fatalf("%s: executor %q has no manifest in config/executor", f, app.Spec.Executor)
		}
		if fs := compatibility.Analyze(&app, p, opts); compatibility.Blocking(fs) {
			t.Errorf("%s: blocked: %s", f, compatibility.Summary(fs))
		}
		if _, err := translate.BuildTemplate(&app, p, opts.Resources, opts.Features); err != nil {
			t.Errorf("%s: %v", f, err)
		}
		if !strings.HasPrefix(app.Spec.Image, "ghcr.io/kubeswift-io/kubeswift-spin-examples/") || strings.HasSuffix(app.Spec.Image, ":latest") {
			t.Errorf("%s: image %q must be a pinned kubeswift-spin example artifact", f, app.Spec.Image)
		}
	}
}

// Project-owned readiness checks set initialDelaySeconds explicitly. Without
// it Spin Operator applies 10 seconds, which kept a replica that could
// already serve out of service for about 8 seconds (docs/performance.md).
// 0 is avoided because Spin Operator's Go type omits a zero value, so a
// typed client that rewrites the spec restores the default of 10.
func TestReadinessChecksSetASmallInitialDelay(t *testing.T) {
	files := append(glob(t, "examples/*/spinapp*.yaml"), glob(t, "examples/experimental/*/spinapp*.yaml")...)
	sawHello := false
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var app spinv1alpha1.SpinApp
		if err := yaml.UnmarshalStrict(b, &app); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		r := app.Spec.Checks.Readiness
		if filepath.Base(filepath.Dir(f)) == "hello-http" {
			sawHello = true
			if r == nil || r.HTTPGet == nil || r.HTTPGet.Path != "/healthz" || r.HTTPGet.HTTPHeaders == nil {
				t.Errorf("%s: the canonical example must check /healthz with httpHeaders: []", f)
			}
		}
		if r != nil && r.InitialDelaySeconds != 1 {
			t.Errorf("%s: readiness initialDelaySeconds is %d, want 1", f, r.InitialDelaySeconds)
		}
	}
	if !sawHello {
		t.Fatal("examples/hello-http/spinapp.yaml not found")
	}

	// The KVM e2e manifests are heredocs in a shell script: every readiness
	// block there must carry the same setting.
	b, err := os.ReadFile(filepath.Join(root, "test/e2e/kvm-e2e.sh"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := 0
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "readiness:" {
			continue
		}
		blocks++
		indent := len(l) - len(strings.TrimLeft(l, " "))
		found := false
		for _, next := range lines[i+1:] {
			if len(next)-len(strings.TrimLeft(next, " ")) <= indent {
				break
			}
			if strings.TrimSpace(next) == "initialDelaySeconds: 1" {
				found = true
			}
		}
		if !found {
			t.Errorf("test/e2e/kvm-e2e.sh:%d: readiness check without initialDelaySeconds: 1", i+1)
		}
	}
	if blocks == 0 {
		t.Error("no readiness checks found in test/e2e/kvm-e2e.sh")
	}
}
