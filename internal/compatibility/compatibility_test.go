package compatibility

import (
	"os"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

func profile() *executor.Profile {
	return &executor.Profile{
		ExecutorName:  "kubeswift",
		RuntimeImage:  "ghcr.io/kubeswift-io/kubeswift-spin-runtime:v0.1.0",
		NetworkMode:   sandboxv1alpha1.SandboxNetworkRestricted,
		DefaultCPU:    resource.MustParse("1"),
		DefaultMemory: resource.MustParse("512Mi"),
	}
}

func opts() Options { return Options{MaxReplicas: 20, Resources: translate.DefaultResourcePolicy()} }

func app() *spinv1alpha1.SpinApp {
	return &spinv1alpha1.SpinApp{
		ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "apps"},
		Spec: spinv1alpha1.SpinAppSpec{
			Executor: "kubeswift",
			Image:    "ghcr.io/example/hello-http:v1.0.0",
			Replicas: 1,
		},
	}
}

// TestMatrixCoversUpstreamSpec fails when SpinAppSpec gains a field that is
// not classified, for example after bumping the spin-operator dependency.
func TestMatrixCoversUpstreamSpec(t *testing.T) {
	classified := map[string]bool{}
	for _, e := range Matrix {
		if classified[e.Field] {
			t.Fatalf("field %q classified twice", e.Field)
		}
		classified[e.Field] = true
	}
	typ := reflect.TypeOf(spinv1alpha1.SpinAppSpec{})
	upstream := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		upstream[name] = true
		if !classified[name] {
			t.Errorf("SpinAppSpec field %q is not classified in compatibility.Matrix", name)
		}
	}
	for f := range classified {
		if !upstream[f] {
			t.Errorf("compatibility.Matrix classifies %q, which SpinAppSpec does not have", f)
		}
	}
}

// TestMatrixDocumented keeps docs/compatibility.md in step with Matrix.
func TestMatrixDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/compatibility.md")
	if err != nil {
		t.Skipf("docs/compatibility.md not readable: %v", err)
	}
	for _, e := range Matrix {
		row := "| `" + e.Field + "` | " + string(e.Level) + " |"
		if !strings.Contains(string(doc), row) {
			t.Errorf("docs/compatibility.md lacks the row prefix %q", row)
		}
	}
}

func TestCleanAppHasNoFindings(t *testing.T) {
	if fs := Analyze(app(), profile(), opts()); len(fs) != 0 {
		t.Fatalf("unexpected findings: %+v", fs)
	}
}

func TestSupportedConfiguration(t *testing.T) {
	a := app()
	a.Spec.Replicas = 3
	a.Spec.Components = []string{"api", "web-ui"}
	a.Spec.Variables = []spinv1alpha1.SpinVar{{Name: "greeting", Value: "hello"}, {Name: "app_version", Value: "1"}}
	a.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")}
	a.Spec.InvocationLimits = map[string]string{"memory": "64Mi"}
	a.Spec.ServiceAnnotations = map[string]string{"example.com/x": "y"}
	a.Spec.RuntimeConfig = spinv1alpha1.RuntimeConfig{
		KeyValueStores:  []spinv1alpha1.KeyValueStoreConfig{{Name: "default", Type: "spin"}},
		SqliteDatabases: []spinv1alpha1.SqliteDatabaseConfig{{Name: "default", Type: "spin"}},
		LLMCompute: &spinv1alpha1.LLMComputeConfig{Type: "remote_http", Options: []spinv1alpha1.RuntimeConfigOption{
			{Name: "url", Value: "http://llm:8000"}, {Name: "auth_token", Value: ""}, {Name: "api_type", Value: "open_ai"}}},
	}
	if fs := Analyze(a, profile(), opts()); Blocking(fs) {
		t.Fatalf("supported configuration blocked: %s", Summary(fs))
	}
}

func TestChecksArePartiallySupportedNotBlocking(t *testing.T) {
	a := app()
	a.Spec.Checks.Readiness = &spinv1alpha1.HealthProbe{HTTPGet: &spinv1alpha1.HTTPHealthProbe{Path: "/healthz"}}
	fs := Analyze(a, profile(), opts())
	if Blocking(fs) || len(fs) != 1 || fs[0].Field != "checks" || fs[0].Level != PartiallySupported {
		t.Fatalf("unexpected findings %+v", fs)
	}
	a.Spec.Checks.Liveness = &spinv1alpha1.HealthProbe{}
	if !Blocking(Analyze(a, profile(), opts())) {
		t.Fatal("probe without httpGet accepted")
	}
}

func TestUnsupportedConfigurationBlocks(t *testing.T) {
	secretRef := &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "url"}
	cases := map[string]struct {
		mutate func(a *spinv1alpha1.SpinApp)
		field  string
		want   string
	}{
		"empty image":       {func(a *spinv1alpha1.SpinApp) { a.Spec.Image = "" }, "image", "must be set"},
		"flag-like image":   {func(a *spinv1alpha1.SpinApp) { a.Spec.Image = "--insecure" }, "image", "not a valid OCI reference"},
		"whitespace image":  {func(a *spinv1alpha1.SpinApp) { a.Spec.Image = "ghcr.io/x y:1" }, "image", "not a valid OCI reference"},
		"zero replicas":     {func(a *spinv1alpha1.SpinApp) { a.Spec.Replicas = 0 }, "replicas", "at least 1"},
		"too many replicas": {func(a *spinv1alpha1.SpinApp) { a.Spec.Replicas = 21 }, "replicas", "above the limit of 20"},
		"autoscaling":       {func(a *spinv1alpha1.SpinApp) { a.Spec.EnableAutoscaling = true; a.Spec.Replicas = 0 }, "enableAutoscaling", "no scale subresource"},
		"gpu resource": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
		}, "resources", "only cpu and memory"},
		"ephemeral storage": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}
		}, "resources", "only cpu and memory"},
		"tiny memory": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}
		}, "resources", "below the minimum"},
		"too many cpus": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64")}
		}, "resources", "above the maximum"},
		"bad component":       {func(a *spinv1alpha1.SpinApp) { a.Spec.Components = []string{"Bad_Name"} }, "components", "not a valid Spin component ID"},
		"duplicate component": {func(a *spinv1alpha1.SpinApp) { a.Spec.Components = []string{"a", "a"} }, "components", "more than once"},
		"bad variable name": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.Variables = []spinv1alpha1.SpinVar{{Name: "Bad-Name", Value: "x"}}
		}, "variables", "not a valid Spin variable name"},
		"duplicate variable": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.Variables = []spinv1alpha1.SpinVar{{Name: "a", Value: "1"}, {Name: "a", Value: "2"}}
		}, "variables", "more than once"},
		"secret variable": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.Variables = []spinv1alpha1.SpinVar{{Name: "database_url", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: secretRef}}}
		}, "variables", `variable "database_url" uses secretKeyRef, but the installed KubeSwift SwiftSandbox API does not provide secure secret projection`},
		"configmap variable": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.Variables = []spinv1alpha1.SpinVar{{Name: "x", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{Key: "k"}}}}
		}, "variables", "configMapKeyRef"},
		"fieldref variable": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.Variables = []spinv1alpha1.SpinVar{{Name: "x", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}}
		}, "variables", "no meaning inside a sandbox"},
		"loadFromSecret": {func(a *spinv1alpha1.SpinApp) { a.Spec.RuntimeConfig.LoadFromSecret = "rc" }, "runtimeConfig", "loadFromSecret is not supported"},
		"secret option": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.RuntimeConfig.KeyValueStores = []spinv1alpha1.KeyValueStoreConfig{{Name: "default", Type: "redis", Options: []spinv1alpha1.RuntimeConfigOption{
				{Name: "url", ValueFrom: &spinv1alpha1.RuntimeConfigVarSource{SecretKeyRef: secretRef}}}}}
		}, "runtimeConfig", "secure secret projection"},
		"literal credential option": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.RuntimeConfig.LLMCompute = &spinv1alpha1.LLMComputeConfig{Type: "remote_http", Options: []spinv1alpha1.RuntimeConfigOption{
				{Name: "url", Value: "http://llm"}, {Name: "auth_token", Value: "sk-live-123"}}}
		}, "runtimeConfig", "looks like a credential"},
		"credential in url": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.RuntimeConfig.KeyValueStores = []spinv1alpha1.KeyValueStoreConfig{{Name: "default", Type: "redis", Options: []spinv1alpha1.RuntimeConfigOption{
				{Name: "url", Value: "redis://user:hunter2@redis:6379"}}}}
		}, "runtimeConfig", "embedded credentials"},
		"local llm": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.RuntimeConfig.LLMCompute = &spinv1alpha1.LLMComputeConfig{Type: "spin"}
		}, "runtimeConfig", "local inference"},
		"store without type": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.RuntimeConfig.SqliteDatabases = []spinv1alpha1.SqliteDatabaseConfig{{Name: "default"}}
		}, "runtimeConfig", "has no type"},
		"duplicate store": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.RuntimeConfig.SqliteDatabases = []spinv1alpha1.SqliteDatabaseConfig{{Name: "d", Type: "spin"}, {Name: "d", Type: "spin"}}
		}, "runtimeConfig", "defined more than once"},
		"bad option name": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.RuntimeConfig.KeyValueStores = []spinv1alpha1.KeyValueStoreConfig{{Name: "d", Type: "spin", Options: []spinv1alpha1.RuntimeConfigOption{{Name: "type", Value: "x"}}}}
		}, "runtimeConfig", "invalid option name"},
		"image pull secrets":       {func(a *spinv1alpha1.SpinApp) { a.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "ghcr"}} }, "imagePullSecrets", "registry credentials"},
		"unknown invocation limit": {func(a *spinv1alpha1.SpinApp) { a.Spec.InvocationLimits = map[string]string{"cpu": "1"} }, "invocationLimits", `key "cpu" is not supported`},
		"bad memory limit":         {func(a *spinv1alpha1.SpinApp) { a.Spec.InvocationLimits = map[string]string{"memory": "lots"} }, "invocationLimits", "positive quantity"},
		"service account":          {func(a *spinv1alpha1.SpinApp) { a.Spec.ServiceAccountName = "app-sa" }, "serviceAccountName", "no Kubernetes identity"},
		"deployment annotations":   {func(a *spinv1alpha1.SpinApp) { a.Spec.DeploymentAnnotations = map[string]string{"a": "b"} }, "deploymentAnnotations", "no Deployment"},
		"pod annotations":          {func(a *spinv1alpha1.SpinApp) { a.Spec.PodAnnotations = map[string]string{"a": "b"} }, "podAnnotations", "no application pod"},
		"pod labels":               {func(a *spinv1alpha1.SpinApp) { a.Spec.PodLabels = map[string]string{"a": "b"} }, "podLabels", "do not carry user labels"},
		"volumes":                  {func(a *spinv1alpha1.SpinApp) { a.Spec.Volumes = []corev1.Volume{{Name: "v"}} }, "volumes", "file projection"},
		"volume mounts": {func(a *spinv1alpha1.SpinApp) {
			a.Spec.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "/x"}}
		}, "volumeMounts", "file projection"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := app()
			tc.mutate(a)
			fs := Analyze(a, profile(), opts())
			if !Blocking(fs) {
				t.Fatalf("not blocking: %+v", fs)
			}
			found := false
			for _, f := range fs {
				if f.Field == tc.field && f.Blocking && strings.Contains(f.Message, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("no blocking finding on %s containing %q: %+v", tc.field, tc.want, fs)
			}
		})
	}
}

// TestFindingsNeverContainValues guards status and Event leakage: findings
// name fields and keys but never echo variable or option values.
func TestFindingsNeverContainValues(t *testing.T) {
	const marker = "VALUE-MUST-NOT-LEAK-7f3a"
	a := app()
	a.Spec.Variables = []spinv1alpha1.SpinVar{
		{Name: "Bad-Name", Value: marker},
		{Name: "ok", Value: marker},
		{Name: "secret_one", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k"}}},
	}
	a.Spec.RuntimeConfig.LLMCompute = &spinv1alpha1.LLMComputeConfig{Type: "remote_http", Options: []spinv1alpha1.RuntimeConfigOption{
		{Name: "auth_token", Value: marker}, {Name: "url", Value: "https://u:" + marker + "@llm"}}}
	a.Spec.RuntimeConfig.KeyValueStores = []spinv1alpha1.KeyValueStoreConfig{{Name: "x", Type: "redis", Options: []spinv1alpha1.RuntimeConfigOption{
		{Name: "password", Value: marker}}}}
	fs := Analyze(a, profile(), opts())
	if len(fs) == 0 {
		t.Fatal("expected findings")
	}
	for _, f := range fs {
		if strings.Contains(f.Message, marker) {
			t.Fatalf("finding leaks a value: %q", f.Message)
		}
	}
	if strings.Contains(Summary(fs), marker) {
		t.Fatal("summary leaks a value")
	}
}

func TestIsCredentialOption(t *testing.T) {
	for _, n := range []string{"auth_token", "token", "password", "key", "access_key", "secret_key", "session_token", "api_key", "AUTH_TOKEN", "connection_string"} {
		if !IsCredentialOption(n) {
			t.Errorf("%q not treated as a credential", n)
		}
	}
	for _, n := range []string{"url", "path", "api_type", "region", "table", "database", "container", "account", "consistency", "keyspace"} {
		if IsCredentialOption(n) {
			t.Errorf("%q treated as a credential", n)
		}
	}
}

func TestFindingsAreDeterministic(t *testing.T) {
	a := app()
	a.Spec.PodLabels = map[string]string{"a": "b"}
	a.Spec.Volumes = []corev1.Volume{{Name: "v"}}
	a.Spec.InvocationLimits = map[string]string{"cpu": "1", "foo": "2", "bar": "3"}
	first := Summary(Analyze(a, profile(), opts()))
	for i := 0; i < 20; i++ {
		if got := Summary(Analyze(a, profile(), opts())); got != first {
			t.Fatalf("findings order changed:\n%s\n%s", first, got)
		}
	}
}

// handledNested lists, per nested upstream type, every field that analysis
// and translation handle. A field added upstream inside these types (for
// example a new runtimeConfig table) fails this test until it is handled or
// explicitly rejected, so it cannot be dropped silently.
var handledNested = map[reflect.Type][]string{
	reflect.TypeOf(spinv1alpha1.RuntimeConfig{}):          {"loadFromSecret", "sqliteDatabases", "keyValueStores", "llmCompute"},
	reflect.TypeOf(spinv1alpha1.KeyValueStoreConfig{}):    {"name", "type", "options"},
	reflect.TypeOf(spinv1alpha1.SqliteDatabaseConfig{}):   {"name", "type", "options"},
	reflect.TypeOf(spinv1alpha1.LLMComputeConfig{}):       {"type", "options"},
	reflect.TypeOf(spinv1alpha1.RuntimeConfigOption{}):    {"name", "value", "valueFrom"},
	reflect.TypeOf(spinv1alpha1.RuntimeConfigVarSource{}): {"configMapKeyRef", "secretKeyRef"},
	reflect.TypeOf(spinv1alpha1.SpinVar{}):                {"name", "value", "valueFrom"},
	reflect.TypeOf(spinv1alpha1.Resources{}):              {"limits", "requests"},
	reflect.TypeOf(spinv1alpha1.HealthChecks{}):           {"readiness", "liveness"},
	reflect.TypeOf(spinv1alpha1.HealthProbe{}): {"httpGet", "initialDelaySeconds", "timeoutSeconds",
		"periodSeconds", "successThreshold", "failureThreshold"},
	reflect.TypeOf(spinv1alpha1.HTTPHealthProbe{}):     {"path", "httpHeaders"},
	reflect.TypeOf(spinv1alpha1.SpinAppExecutorSpec{}): {"createDeployment", "deploymentConfig"},
	reflect.TypeOf(spinv1alpha1.ExecutorDeploymentConfig{}): {"runtimeClassName", "spinImage", "caCertSecret",
		"installDefaultCACerts", "otel"},
	reflect.TypeOf(spinv1alpha1.OtelConfig{}): {"exporter_otlp_endpoint", "exporter_otlp_traces_endpoint",
		"exporter_otlp_metrics_endpoint", "exporter_otlp_logs_endpoint"},
}

func TestNestedUpstreamFieldsAreHandled(t *testing.T) {
	for typ, fields := range handledNested {
		want := map[string]bool{}
		for _, f := range fields {
			want[f] = true
		}
		got := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			got[name] = true
			if !want[name] {
				t.Errorf("%s.%s is not handled by kubeswift-spin; classify it in internal/compatibility", typ.Name(), name)
			}
		}
		for f := range want {
			if !got[f] {
				t.Errorf("%s has no field %q any more; update handledNested and the translation", typ.Name(), f)
			}
		}
	}
}
