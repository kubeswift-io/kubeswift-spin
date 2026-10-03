package translate

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
)

const runtimeImage = "ghcr.io/kubeswift-io/kubeswift-spin-runtime:v0.1.0"

func testProfile() *executor.Profile {
	return &executor.Profile{
		ExecutorName:  "kubeswift",
		RuntimeImage:  runtimeImage,
		NetworkMode:   sandboxv1alpha1.SandboxNetworkRestricted,
		DefaultCPU:    resource.MustParse("1"),
		DefaultMemory: resource.MustParse("512Mi"),
	}
}

func testApp() *spinv1alpha1.SpinApp {
	return &spinv1alpha1.SpinApp{
		ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "apps", UID: "uid-1"},
		Spec: spinv1alpha1.SpinAppSpec{
			Executor: "kubeswift",
			Image:    "ghcr.io/example/hello-http:v1.0.0",
			Replicas: 2,
		},
	}
}

func TestBuildTemplateBasics(t *testing.T) {
	tmpl, err := BuildTemplate(testApp(), testProfile(), DefaultResourcePolicy())
	if err != nil {
		t.Fatal(err)
	}
	s := tmpl.Spec
	if s.Image != runtimeImage {
		t.Fatalf("image = %q", s.Image)
	}
	if s.CPU != 1 || s.Memory.String() != "512Mi" {
		t.Fatalf("shape = %d / %s", s.CPU, s.Memory.String())
	}
	if !reflect.DeepEqual(s.Command, []string{runtimecontract.EntrypointPath}) {
		t.Fatalf("command = %v", s.Command)
	}
	wantArgs := []string{
		"up",
		"--from=ghcr.io/example/hello-http:v1.0.0",
		"--listen=0.0.0.0:3000",
	}
	if !reflect.DeepEqual(s.Args, wantArgs) {
		t.Fatalf("args = %q\nwant  %q", s.Args, wantArgs)
	}
	if s.Network.Mode != sandboxv1alpha1.SandboxNetworkRestricted {
		t.Fatalf("network = %q", s.Network.Mode)
	}
	if s.PoolRef != nil || s.KernelProfileRef != nil || s.VerifyKeySecretRef != nil || s.ImagePullSecret != "" {
		t.Fatalf("unexpected optional fields: %+v", s)
	}
	if len(s.Env) != 0 {
		t.Fatalf("unexpected env: %v", s.Env)
	}
	if s.Timeout != nil || s.TTL != nil {
		t.Fatalf("a long-running server must not get a timeout or ttl")
	}
	if len(tmpl.Revision) != revisionLen {
		t.Fatalf("revision %q", tmpl.Revision)
	}
}

func TestBuildTemplateProfileFields(t *testing.T) {
	p := testProfile()
	p.NetworkMode = sandboxv1alpha1.SandboxNetworkOpen
	p.RootfsMode = sandboxv1alpha1.SandboxRootfsVirtiofs
	p.KernelProfile = "sandbox-6-6"
	p.SandboxPool = "spin-warm"
	p.NodeSelector = map[string]string{"zone": "a"}
	p.RuntimeImagePullSecret = "runtime-pull"
	p.RuntimeImageVerifyKey = "cosign-key"
	tmpl, err := BuildTemplate(testApp(), p, DefaultResourcePolicy())
	if err != nil {
		t.Fatal(err)
	}
	s := tmpl.Spec
	if s.Network.Mode != "open" || s.RootfsMode != "virtiofs" || s.KernelProfileRef.Name != "sandbox-6-6" ||
		s.PoolRef.Name != "spin-warm" || s.NodeSelector["zone"] != "a" || s.ImagePullSecret != "runtime-pull" ||
		s.VerifyKeySecretRef.Name != "cosign-key" {
		t.Fatalf("profile not applied: %+v", s)
	}
	// The template must not alias the profile's map.
	p.NodeSelector["zone"] = "b"
	if s.NodeSelector["zone"] != "a" {
		t.Fatalf("node selector aliases the profile")
	}
}

func TestArgsComponentsAndRuntimeConfig(t *testing.T) {
	app := testApp()
	app.Spec.Components = []string{"api", "worker"}
	app.Spec.RuntimeConfig.KeyValueStores = []spinv1alpha1.KeyValueStoreConfig{{Name: "default", Type: "spin"}}
	args := Args(app)
	joined := strings.Join(args, " ")
	for _, want := range []string{"--component-id=api", "--component-id=worker", "--runtime-config-file=" + runtimecontract.RuntimeConfigPath} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", args, want)
		}
	}
}

func TestArgsNeverSplitUserValues(t *testing.T) {
	app := testApp()
	app.Spec.Image = "--insecure"
	app.Spec.Components = []string{"--quiet"}
	for _, a := range Args(app) {
		if strings.HasPrefix(a, "--insecure") || a == "--quiet" {
			t.Fatalf("user value became a separate flag: %q", a)
		}
	}
}

func TestEnvVariablesOtelAndLimits(t *testing.T) {
	app := testApp()
	app.Spec.Variables = []spinv1alpha1.SpinVar{{Name: "greeting", Value: "hi"}, {Name: "app_version", Value: "1.2.3"}}
	app.Spec.InvocationLimits = map[string]string{"memory": "64Mi"}
	p := testProfile()
	p.Otel = &spinv1alpha1.OtelConfig{ExporterOtlpEndpoint: "http://otel:4318", ExporterOtlpTracesEndpoint: "http://otel:4318/v1/traces"}
	env, err := Env(app, p)
	if err != nil {
		t.Fatal(err)
	}
	want := []corev1.EnvVar{
		{Name: "SPIN_VARIABLE_GREETING", Value: "hi"},
		{Name: "SPIN_VARIABLE_APP_VERSION", Value: "1.2.3"},
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://otel:4318"},
		{Name: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", Value: "http://otel:4318/v1/traces"},
		{Name: "SPIN_MAX_INSTANCE_MEMORY", Value: "67108864"},
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %+v\nwant %+v", env, want)
	}
	for _, e := range env {
		if e.ValueFrom != nil {
			t.Fatalf("env %s has valueFrom", e.Name)
		}
	}
}

func TestEnvRejectsValueFrom(t *testing.T) {
	app := testApp()
	app.Spec.Variables = []spinv1alpha1.SpinVar{{Name: "db", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k"}}}}
	if _, err := Env(app, testProfile()); err == nil {
		t.Fatal("valueFrom was translated")
	}
	if _, err := BuildTemplate(app, testProfile(), DefaultResourcePolicy()); err == nil {
		t.Fatal("template built with valueFrom")
	}
}

func TestRuntimeConfigTOML(t *testing.T) {
	rc := spinv1alpha1.RuntimeConfig{
		KeyValueStores: []spinv1alpha1.KeyValueStoreConfig{
			{Name: "default", Type: "spin", Options: []spinv1alpha1.RuntimeConfigOption{{Name: "path", Value: "/var/lib/kubeswift-spin/state/kv.db"}}},
			{Name: "cache", Type: "spin"},
		},
		SqliteDatabases: []spinv1alpha1.SqliteDatabaseConfig{{Name: "default", Type: "spin"}},
		LLMCompute: &spinv1alpha1.LLMComputeConfig{Type: "remote_http", Options: []spinv1alpha1.RuntimeConfigOption{
			{Name: "url", Value: "http://llm.inference.svc:8000"}, {Name: "auth_token", Value: ""}, {Name: "api_type", Value: "open_ai"},
		}},
	}
	b, err := RuntimeConfigTOML(rc)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]any
	if err := toml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("invalid TOML: %v\n%s", err, b)
	}
	kv := doc["key_value_store"]["default"].(map[string]any)
	if kv["type"] != "spin" || kv["path"] != "/var/lib/kubeswift-spin/state/kv.db" {
		t.Fatalf("key_value_store.default = %v", kv)
	}
	if doc["sqlite_database"]["default"].(map[string]any)["type"] != "spin" {
		t.Fatalf("sqlite = %v", doc["sqlite_database"])
	}
	llm := doc["llm_compute"]
	if llm["type"] != "remote_http" || llm["api_type"] != "open_ai" || llm["url"] != "http://llm.inference.svc:8000" {
		t.Fatalf("llm_compute = %v", llm)
	}
	if _, ok := llm["auth_token"]; !ok {
		t.Fatalf("auth_token must be present (Spin requires it), got %v", llm)
	}

	// Rendering is deterministic.
	b2, _ := RuntimeConfigTOML(rc)
	if string(b) != string(b2) {
		t.Fatal("runtime config rendering is not deterministic")
	}

	// It is delivered base64-encoded in the environment.
	app := testApp()
	app.Spec.RuntimeConfig = rc
	env, err := Env(app, testProfile())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range env {
		if e.Name == runtimecontract.RuntimeConfigEnv {
			dec, err := base64.StdEncoding.DecodeString(e.Value)
			if err != nil || string(dec) != string(b) {
				t.Fatalf("runtime config env does not round-trip")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("runtime config env missing")
	}
}

func TestRuntimeConfigRejects(t *testing.T) {
	cases := map[string]spinv1alpha1.RuntimeConfig{
		"duplicate store": {KeyValueStores: []spinv1alpha1.KeyValueStoreConfig{{Name: "a", Type: "spin"}, {Name: "a", Type: "spin"}}},
		"reserved type":   {KeyValueStores: []spinv1alpha1.KeyValueStoreConfig{{Name: "a", Type: "spin", Options: []spinv1alpha1.RuntimeConfigOption{{Name: "type", Value: "redis"}}}}},
		"valueFrom": {LLMCompute: &spinv1alpha1.LLMComputeConfig{Type: "remote_http", Options: []spinv1alpha1.RuntimeConfigOption{
			{Name: "auth_token", ValueFrom: &spinv1alpha1.RuntimeConfigVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}}}}},
	}
	for name, rc := range cases {
		if _, err := RuntimeConfigTOML(rc); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestFingerprint(t *testing.T) {
	base, err := BuildTemplate(testApp(), testProfile(), DefaultResourcePolicy())
	if err != nil {
		t.Fatal(err)
	}
	again, _ := BuildTemplate(testApp(), testProfile(), DefaultResourcePolicy())
	if base.Revision != again.Revision {
		t.Fatal("fingerprint is not deterministic")
	}

	// Replica count and metadata do not affect the sandbox spec.
	scaled := testApp()
	scaled.Spec.Replicas = 7
	scaled.Labels = map[string]string{"team": "x"}
	if tm, _ := BuildTemplate(scaled, testProfile(), DefaultResourcePolicy()); tm.Revision != base.Revision {
		t.Fatal("scaling changed the revision")
	}

	changes := map[string]func(a *spinv1alpha1.SpinApp, p *executor.Profile){
		"image":         func(a *spinv1alpha1.SpinApp, _ *executor.Profile) { a.Spec.Image = "ghcr.io/example/hello-http:v1.0.1" },
		"components":    func(a *spinv1alpha1.SpinApp, _ *executor.Profile) { a.Spec.Components = []string{"api"} },
		"runtime image": func(_ *spinv1alpha1.SpinApp, p *executor.Profile) { p.RuntimeImage = runtimeImage + "-rc" },
		"cpu": func(a *spinv1alpha1.SpinApp, _ *executor.Profile) {
			a.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
		},
		"memory": func(a *spinv1alpha1.SpinApp, _ *executor.Profile) {
			a.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
		},
		"network": func(_ *spinv1alpha1.SpinApp, p *executor.Profile) { p.NetworkMode = sandboxv1alpha1.SandboxNetworkOpen },
		"variable": func(a *spinv1alpha1.SpinApp, _ *executor.Profile) {
			a.Spec.Variables = []spinv1alpha1.SpinVar{{Name: "x", Value: "y"}}
		},
		"runtime config": func(a *spinv1alpha1.SpinApp, _ *executor.Profile) {
			a.Spec.RuntimeConfig.KeyValueStores = []spinv1alpha1.KeyValueStoreConfig{{Name: "default", Type: "spin"}}
		},
		"pool": func(_ *spinv1alpha1.SpinApp, p *executor.Profile) { p.SandboxPool = "warm" },
	}
	for name, mutate := range changes {
		a, p := testApp(), testProfile()
		mutate(a, p)
		tm, err := BuildTemplate(a, p, DefaultResourcePolicy())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tm.Revision == base.Revision {
			t.Fatalf("changing %s did not change the revision", name)
		}
	}
}

func TestNewSandboxLabels(t *testing.T) {
	app := testApp()
	tmpl, _ := BuildTemplate(app, testProfile(), DefaultResourcePolicy())
	sb := NewSandbox(app, "kubeswift", tmpl, 1)
	if sb.Name != "hello-1" || sb.Namespace != "apps" {
		t.Fatalf("name = %s/%s", sb.Namespace, sb.Name)
	}
	want := map[string]string{
		LabelManagedBy:       ManagedByValue,
		LabelApp:             "hello",
		LabelSpinKubeAppName: "hello",
		LabelOrdinal:         "1",
		LabelRevision:        tmpl.Revision,
		LabelExecutor:        "kubeswift",
	}
	if !reflect.DeepEqual(sb.Labels, want) {
		t.Fatalf("labels = %v", sb.Labels)
	}
	if len(sb.Annotations) != 0 {
		t.Fatalf("no annotations expected, got %v", sb.Annotations)
	}
	// The sandbox spec is a copy, not an alias of the template.
	sb.Spec.Args[0] = "changed"
	if tmpl.Spec.Args[0] != "up" {
		t.Fatal("sandbox aliases template")
	}
}

func TestPoolMismatches(t *testing.T) {
	tmpl, _ := BuildTemplate(testApp(), testProfile(), DefaultResourcePolicy())
	ok := &sandboxv1alpha1.SwiftSandboxPool{Spec: sandboxv1alpha1.SwiftSandboxPoolSpec{
		Image:  runtimeImage,
		Memory: resource.MustParse("512Mi"),
	}}
	if mm := PoolMismatches(&tmpl.Spec, ok); len(mm) != 0 {
		t.Fatalf("compatible pool reported mismatches: %v", mm)
	}
	bad := ok.DeepCopy()
	bad.Spec.Image = "ghcr.io/other/image:v1"
	bad.Spec.CPU = 2
	bad.Spec.Memory = resource.MustParse("1Gi")
	bad.Spec.Network.Mode = sandboxv1alpha1.SandboxNetworkOpen
	bad.Spec.RootfsMode = sandboxv1alpha1.SandboxRootfsVirtiofs
	bad.Spec.KernelProfileRef = &corev1.LocalObjectReference{Name: "k"}
	bad.Spec.VerifyKeySecretRef = &sandboxv1alpha1.SecretObjectReference{Name: "v"}
	bad.Spec.NodeSelector = map[string]string{"a": "b"}
	bad.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu"}
	bad.Spec.Model = &sandboxv1alpha1.SandboxModel{ImageRef: "m"}
	mm := PoolMismatches(&tmpl.Spec, bad)
	for _, field := range []string{"image", "cpu", "memory", "network.mode", "rootfsMode", "kernelProfileRef", "verifyKeySecretRef", "nodeSelector", "gpuProfileRef", "model"} {
		found := false
		for _, m := range mm {
			if strings.HasPrefix(m, field+":") {
				found = true
			}
		}
		if !found {
			t.Fatalf("mismatch on %s not reported: %v", field, mm)
		}
	}
	// Explicit values equal to the defaults are compatible.
	explicit := ok.DeepCopy()
	explicit.Spec.CPU = 1
	explicit.Spec.Network.Mode = sandboxv1alpha1.SandboxNetworkRestricted
	explicit.Spec.RootfsMode = sandboxv1alpha1.SandboxRootfsBlock
	if mm := PoolMismatches(&tmpl.Spec, explicit); len(mm) != 0 {
		t.Fatalf("defaults treated as mismatches: %v", mm)
	}
}

func TestPoolDefaultKernelIsNormalized(t *testing.T) {
	tmpl, _ := BuildTemplate(testApp(), testProfile(), DefaultResourcePolicy())
	pool := &sandboxv1alpha1.SwiftSandboxPool{Spec: sandboxv1alpha1.SwiftSandboxPoolSpec{
		Image:            runtimeImage,
		Memory:           resource.MustParse("512Mi"),
		KernelProfileRef: &corev1.LocalObjectReference{Name: "sandbox"},
	}}
	if mm := PoolMismatches(&tmpl.Spec, pool); len(mm) != 0 {
		t.Fatalf("explicit default kernel treated as a mismatch: %v", mm)
	}
}
