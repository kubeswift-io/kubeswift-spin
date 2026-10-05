package translate

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

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
}
