package translate

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

func secretKey(name, key string) *corev1.SecretKeySelector {
	return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}
}

func TestLegacyClusterGetsNoNewFields(t *testing.T) {
	tmpl, err := BuildTemplate(testApp(), testProfile(), DefaultResourcePolicy(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	s := tmpl.Spec
	if s.Network.Ports != nil || s.PodMetadata != nil || s.ReadinessProbe != nil || s.LivenessProbe != nil ||
		s.SecretFiles != nil || s.Network.Egress != nil || s.Network.Ingress != nil {
		t.Fatalf("v0.16.0 fields set for a v0.15.1 cluster: %+v", s)
	}
}

func TestExposure(t *testing.T) {
	app := testApp()
	app.Spec.PodLabels = map[string]string{"team": "payments"}
	tmpl, err := BuildTemplate(app, testProfile(), DefaultResourcePolicy(), v16)
	if err != nil {
		t.Fatal(err)
	}
	s := tmpl.Spec
	if len(s.Network.Ports) != 1 || s.Network.Ports[0].Name != "http-app" || s.Network.Ports[0].Port != 3000 {
		t.Fatalf("ports %+v", s.Network.Ports)
	}
	want := map[string]string{"core.spinkube.dev/app.hello.status": "ready", "core.spinkube.dev/app-name": "hello", "team": "payments"}
	for k, v := range want {
		if s.PodMetadata.Labels[k] != v {
			t.Fatalf("pod label %s = %q in %v", k, s.PodMetadata.Labels[k], s.PodMetadata.Labels)
		}
	}
	rp := s.ReadinessProbe
	if rp == nil || rp.TCPSocket == nil || rp.TCPSocket.Port.String() != "http-app" {
		t.Fatalf("default readiness probe %+v", rp)
	}
	if s.LivenessProbe != nil {
		t.Fatal("liveness probe without spec.checks.liveness")
	}
	if s.Network.Ingress != nil {
		t.Fatal("ingress restriction without profile setting")
	}
	legacyTmpl, _ := BuildTemplate(app, testProfile(), DefaultResourcePolicy(), legacy)
	if legacyTmpl.Revision == tmpl.Revision {
		t.Fatal("gaining exposure did not change the revision")
	}
}

func TestChecksBecomeProbes(t *testing.T) {
	app := testApp()
	app.Spec.Checks.Readiness = &spinv1alpha1.HealthProbe{
		HTTPGet:             &spinv1alpha1.HTTPHealthProbe{Path: "/healthz", HTTPHeaders: []spinv1alpha1.HTTPHealthProbeHeader{{Name: "X-Probe", Value: "1"}}},
		InitialDelaySeconds: 1, TimeoutSeconds: 2, PeriodSeconds: 3, SuccessThreshold: 1, FailureThreshold: 4,
	}
	app.Spec.Checks.Liveness = &spinv1alpha1.HealthProbe{HTTPGet: &spinv1alpha1.HTTPHealthProbe{Path: "/live"}, PeriodSeconds: 10, FailureThreshold: 3, SuccessThreshold: 1, TimeoutSeconds: 1}
	tmpl, err := BuildTemplate(app, testProfile(), DefaultResourcePolicy(), v16)
	if err != nil {
		t.Fatal(err)
	}
	r := tmpl.Spec.ReadinessProbe
	if r.HTTPGet == nil || r.HTTPGet.Path != "/healthz" || r.HTTPGet.Port.String() != "http-app" || r.HTTPGet.Scheme != corev1.URISchemeHTTP ||
		r.InitialDelaySeconds != 1 || r.TimeoutSeconds != 2 || r.PeriodSeconds != 3 || r.FailureThreshold != 4 ||
		len(r.HTTPGet.HTTPHeaders) != 1 || r.HTTPGet.HTTPHeaders[0].Name != "X-Probe" {
		t.Fatalf("readiness %+v", r)
	}
	if l := tmpl.Spec.LivenessProbe; l == nil || l.HTTPGet.Path != "/live" {
		t.Fatalf("liveness %+v", l)
	}
}

func TestSecretVariablesAreReferences(t *testing.T) {
	app := testApp()
	app.Spec.Variables = []spinv1alpha1.SpinVar{
		{Name: "greeting", Value: "hi"},
		{Name: "database_url", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: secretKey("db", "url")}},
	}
	tmpl, err := BuildTemplate(app, testProfile(), DefaultResourcePolicy(), v16)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range tmpl.Spec.Env {
		if e.Name == "SPIN_VARIABLE_DATABASE_URL" {
			found = true
			if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != "db" || e.ValueFrom.SecretKeyRef.Key != "url" {
				t.Fatalf("secret variable %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("secret variable missing")
	}
}

func TestSecretRuntimeConfigOptions(t *testing.T) {
	app := testApp()
	app.Spec.RuntimeConfig.LLMCompute = &spinv1alpha1.LLMComputeConfig{Type: "remote_http", Options: []spinv1alpha1.RuntimeConfigOption{
		{Name: "url", Value: "https://llm.example.com"},
		{Name: "auth_token", ValueFrom: &spinv1alpha1.RuntimeConfigVarSource{SecretKeyRef: secretKey("llm", "token")}},
	}}
	env, files, err := Env(app, testProfile(), v16)
	if err != nil {
		t.Fatal(err)
	}
	if files != nil {
		t.Fatalf("unexpected secret files %+v", files)
	}
	var doc []byte
	var secret *corev1.EnvVar
	for i, e := range env {
		switch e.Name {
		case runtimecontract.RuntimeConfigEnv:
			doc, _ = base64.StdEncoding.DecodeString(e.Value)
		case runtimecontract.SecretValueEnvPrefix + "0":
			secret = &env[i]
		}
	}
	var tree map[string]map[string]string
	if err := toml.Unmarshal(doc, &tree); err != nil {
		t.Fatal(err)
	}
	if tree["llm_compute"]["auth_token"] != runtimecontract.SecretPlaceholderPrefix+runtimecontract.SecretValueEnvPrefix+"0" {
		t.Fatalf("placeholder %q", tree["llm_compute"]["auth_token"])
	}
	if secret == nil || secret.ValueFrom == nil || secret.ValueFrom.SecretKeyRef.Name != "llm" {
		t.Fatalf("secret env %+v", secret)
	}
	if _, _, err := Env(app, testProfile(), legacy); err == nil {
		t.Fatal("secret option rendered without Secret projection")
	}
}

func TestLoadFromSecret(t *testing.T) {
	app := testApp()
	app.Spec.RuntimeConfig.LoadFromSecret = "rc"
	tmpl, err := BuildTemplate(app, testProfile(), DefaultResourcePolicy(), v16)
	if err != nil {
		t.Fatal(err)
	}
	f := tmpl.Spec.SecretFiles
	if len(f) != 1 || f[0].SecretName != "rc" || f[0].Items[0].Key != "runtime-config.toml" || f[0].Items[0].Path != runtimecontract.SecretRuntimeConfigPath {
		t.Fatalf("secret files %+v", f)
	}
	if !strings.Contains(strings.Join(tmpl.Spec.Args, " "), "--runtime-config-file="+runtimecontract.RuntimeConfigPath) {
		t.Fatal("runtime config flag missing")
	}
	var fileEnv bool
	for _, e := range tmpl.Spec.Env {
		if e.Name == runtimecontract.RuntimeConfigEnv {
			t.Fatal("inline runtime config set as well")
		}
		fileEnv = fileEnv || (e.Name == runtimecontract.RuntimeConfigFileEnv && e.Value == runtimecontract.SecretRuntimeConfigPath)
	}
	if !fileEnv {
		t.Fatal("runtime config file variable missing")
	}
}

func TestImagePullSecretsBecomeSecretFiles(t *testing.T) {
	app := testApp()
	app.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "ghcr"}, {Name: "quay"}}
	tmpl, err := BuildTemplate(app, testProfile(), DefaultResourcePolicy(), v16)
	if err != nil {
		t.Fatal(err)
	}
	f := tmpl.Spec.SecretFiles
	if len(f) != 2 || f[0].SecretName != "ghcr" || f[0].Items[0].Key != ".dockerconfigjson" || f[1].Items[0].Path != runtimecontract.RegistryAuthDir+"/1.json" {
		t.Fatalf("secret files %+v", f)
	}
	var list string
	for _, e := range tmpl.Spec.Env {
		if e.Name == runtimecontract.RegistryAuthFilesEnv {
			list = e.Value
		}
	}
	if list != runtimecontract.RegistryAuthDir+"/0.json,"+runtimecontract.RegistryAuthDir+"/1.json" {
		t.Fatalf("auth file list %q", list)
	}
	if _, err := BuildTemplate(app, testProfile(), DefaultResourcePolicy(), legacy); err == nil {
		t.Fatal("imagePullSecrets translated without Secret projection")
	}
}

func TestEgressAndIngressFromProfile(t *testing.T) {
	p := testProfile()
	p.EgressAllow = []sandboxv1alpha1.SandboxEgressRule{{Service: &sandboxv1alpha1.SandboxEgressService{Name: "llm", Namespace: "inference"}}}
	p.IngressFrom = []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}}}
	tmpl, err := BuildTemplate(testApp(), p, DefaultResourcePolicy(), v16)
	if err != nil {
		t.Fatal(err)
	}
	if e := tmpl.Spec.Network.Egress; e == nil || e.Allow[0].Service.Name != "llm" {
		t.Fatalf("egress %+v", e)
	}
	if i := tmpl.Spec.Network.Ingress; i == nil || len(i.From) != 1 {
		t.Fatalf("ingress %+v", i)
	}
	if _, err := BuildTemplate(testApp(), p, DefaultResourcePolicy(), legacy); err == nil {
		t.Fatal("egress allowlist rendered without the feature")
	}
}

func TestPoolComparesPortsAndEgress(t *testing.T) {
	tmpl, _ := BuildTemplate(testApp(), testProfile(), DefaultResourcePolicy(), v16)
	pool := &sandboxv1alpha1.SwiftSandboxPool{ObjectMeta: metav1.ObjectMeta{Namespace: "apps"}, Spec: sandboxv1alpha1.SwiftSandboxPoolSpec{
		Image: runtimeImage, Memory: resource.MustParse("512Mi"),
	}}
	mm := PoolMismatches(&tmpl.Spec, "apps", pool)
	if len(mm) != 1 || !strings.HasPrefix(mm[0], "network.ports:") {
		t.Fatalf("mismatches %v", mm)
	}
	pool.Spec.Network.Ports = []sandboxv1alpha1.SandboxPort{{Name: "http-app", Port: 3000}}
	if mm := PoolMismatches(&tmpl.Spec, "apps", pool); len(mm) != 0 {
		t.Fatalf("matching pool reported %v", mm)
	}
	// A Service without a namespace is in the declaring object's namespace.
	spec := tmpl.Spec.DeepCopy()
	spec.Network.Egress = &sandboxv1alpha1.SandboxEgress{Allow: []sandboxv1alpha1.SandboxEgressRule{{Service: &sandboxv1alpha1.SandboxEgressService{Name: "llm"}}}}
	pool.Spec.Network.Egress = &sandboxv1alpha1.SandboxEgress{Allow: []sandboxv1alpha1.SandboxEgressRule{{Service: &sandboxv1alpha1.SandboxEgressService{Name: "llm", Namespace: "apps"}}}}
	if mm := PoolMismatches(spec, "apps", pool); len(mm) != 0 {
		t.Fatalf("equivalent egress reported %v", mm)
	}
	// Ingress peers must match too, or KubeSwift boots the replica cold.
	peers := []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}}}}
	spec.Network.Ingress = &sandboxv1alpha1.SandboxIngress{From: peers}
	if mm := PoolMismatches(spec, "apps", pool); len(mm) != 1 || !strings.HasPrefix(mm[0], "network.ingress.from:") {
		t.Fatalf("ingress mismatch not reported: %v", mm)
	}
	pool.Spec.Network.Ingress = &sandboxv1alpha1.SandboxIngress{From: peers}
	if mm := PoolMismatches(spec, "apps", pool); len(mm) != 0 {
		t.Fatalf("equal ingress reported %v", mm)
	}
	spec.Network.Ingress = &sandboxv1alpha1.SandboxIngress{}
	pool.Spec.Network.Ingress = nil
	if mm := PoolMismatches(spec, "apps", pool); len(mm) != 0 {
		t.Fatalf("empty ingress and no ingress reported %v", mm)
	}
}

func TestReadinessStartup(t *testing.T) {
	v162 := v16
	v162.ReadinessStartup = true
	withCheck := func() *spinv1alpha1.SpinApp {
		a := testApp()
		a.Spec.Checks.Readiness = &spinv1alpha1.HealthProbe{
			HTTPGet:             &spinv1alpha1.HTTPHealthProbe{Path: "/healthz"},
			InitialDelaySeconds: 10, PeriodSeconds: 2,
		}
		return a
	}
	cases := []struct {
		name   string
		app    *spinv1alpha1.SpinApp
		policy string
		caps   capabilities.Sandbox
		want   bool
	}{
		{"auto, own check", testApp(), "", v162, true},
		{"auto explicit, own check", testApp(), executor.ReadinessStartupAuto, v162, true},
		{"auto, SpinApp check keeps its initialDelaySeconds", withCheck(), executor.ReadinessStartupAuto, v162, false},
		{"always, SpinApp check", withCheck(), executor.ReadinessStartupAlways, v162, true},
		{"never, own check", testApp(), executor.ReadinessStartupNever, v162, false},
		{"auto, KubeSwift without readinessStartup", testApp(), executor.ReadinessStartupAuto, v16, false},
		{"always, KubeSwift without readinessStartup", withCheck(), executor.ReadinessStartupAlways, v16, false},
	}
	for _, c := range cases {
		p := testProfile()
		p.ReadinessStartup = c.policy
		tmpl, err := BuildTemplate(c.app, p, DefaultResourcePolicy(), c.caps)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := tmpl.Spec.ReadinessStartup != nil
		if got != c.want {
			t.Errorf("%s: readinessStartup %v, want %v", c.name, got, c.want)
		}
		if got && (tmpl.Spec.ReadinessStartup.Interval != nil || tmpl.Spec.ReadinessStartup.Window != nil) {
			t.Errorf("%s: interval or window set; KubeSwift defaults are intended", c.name)
		}
		if tmpl.Spec.ReadinessProbe == nil {
			t.Errorf("%s: no readiness probe", c.name)
		}
	}
	// The user's check is passed through unchanged even with "always".
	p := testProfile()
	p.ReadinessStartup = executor.ReadinessStartupAlways
	tmpl, err := BuildTemplate(withCheck(), p, DefaultResourcePolicy(), v162)
	if err != nil {
		t.Fatal(err)
	}
	if rp := tmpl.Spec.ReadinessProbe; rp.InitialDelaySeconds != 10 || rp.PeriodSeconds != 2 || rp.HTTPGet == nil || rp.HTTPGet.Path != "/healthz" {
		t.Fatalf("readiness probe changed: %+v", rp)
	}
}

func TestDrainPeriod(t *testing.T) {
	v163 := v16
	v163.ReadinessStartup = true
	v163.Termination = true
	thirty := 30 * time.Second
	unset := testProfile()
	base, err := BuildTemplate(testApp(), unset, DefaultResourcePolicy(), v163)
	if err != nil {
		t.Fatal(err)
	}
	if base.Spec.Termination != nil {
		t.Fatalf("termination set without the annotation: %+v", base.Spec.Termination)
	}
	set := testProfile()
	set.DrainPeriod = &thirty
	tmpl, err := BuildTemplate(testApp(), set, DefaultResourcePolicy(), v163)
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.Spec.Termination == nil || tmpl.Spec.Termination.DrainPeriod == nil || tmpl.Spec.Termination.DrainPeriod.Duration != thirty {
		t.Fatalf("termination %+v", tmpl.Spec.Termination)
	}
	if tmpl.Revision == base.Revision {
		t.Fatal("drain period does not change the revision")
	}
	// 0s is a value (stop right after leaving the Service), not unset.
	var zero time.Duration
	set0 := testProfile()
	set0.DrainPeriod = &zero
	z, err := BuildTemplate(testApp(), set0, DefaultResourcePolicy(), v163)
	if err != nil {
		t.Fatal(err)
	}
	if z.Spec.Termination == nil || z.Spec.Termination.DrainPeriod == nil || z.Spec.Termination.DrainPeriod.Duration != 0 || z.Revision == base.Revision {
		t.Fatalf("0s: termination %+v, revision %s (unset %s)", z.Spec.Termination, z.Revision, base.Revision)
	}
	// Without spec.termination in the schema the value is never dropped
	// silently (the reconciler reports the executor invalid first, see
	// executor.CheckFeatures).
	if _, err := BuildTemplate(testApp(), set, DefaultResourcePolicy(), v16); err == nil || !strings.Contains(err.Error(), executor.AnnDrainPeriod) {
		t.Fatalf("err = %v, want one naming %s", err, executor.AnnDrainPeriod)
	}
}
