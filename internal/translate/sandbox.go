// Package translate converts a SpinApp and an executor profile into the
// SwiftSandbox objects that realize it. Everything here is a pure function of
// its inputs so the result is deterministic and testable without a cluster.
//
// translate assumes its input has passed compatibility.Analyze. It still
// returns errors for invalid input rather than producing a partial spec.
package translate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
)

// Labels set on every SwiftSandbox created by kubeswift-spin.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByValue = "kubeswift-spin"
	LabelApp       = executor.Prefix + "app"
	LabelOrdinal   = executor.Prefix + "ordinal"
	LabelRevision  = executor.Prefix + "revision"
	LabelExecutor  = executor.Prefix + "executor"
	// LabelSpinKubeAppName mirrors the label Spin Operator puts on the objects
	// it creates, so `kubectl get swiftsandbox -l core.spinkube.dev/app-name=x`
	// works the way SpinKube users expect.
	LabelSpinKubeAppName = "core.spinkube.dev/app-name"
)

const revisionLen = 10

// Template is the sandbox spec shared by every replica of one SpinApp
// revision.
type Template struct {
	Spec sandboxv1alpha1.SwiftSandboxSpec
	// Revision is a hash of Spec. A replica whose revision label differs from
	// the template revision is replaced.
	Revision string
}

// BuildTemplate builds the replica template for app under profile.
func BuildTemplate(app *spinv1alpha1.SpinApp, p *executor.Profile, policy ResourcePolicy) (*Template, error) {
	cpu, err := VCPUs(app.Spec.Resources, p.DefaultCPU, policy)
	if err != nil {
		return nil, err
	}
	mem, err := Memory(app.Spec.Resources, p.DefaultMemory, policy)
	if err != nil {
		return nil, err
	}
	env, err := Env(app, p)
	if err != nil {
		return nil, err
	}

	spec := sandboxv1alpha1.SwiftSandboxSpec{
		Image:   p.RuntimeImage,
		CPU:     cpu,
		Memory:  mem,
		Command: []string{runtimecontract.EntrypointPath},
		Args:    Args(app),
		Env:     env,
		Network: sandboxv1alpha1.SandboxNetwork{Mode: p.NetworkMode},
	}
	spec.ImagePullSecret = p.RuntimeImagePullSecret
	if p.RuntimeImageVerifyKey != "" {
		spec.VerifyKeySecretRef = &sandboxv1alpha1.SecretObjectReference{Name: p.RuntimeImageVerifyKey}
	}
	spec.RootfsMode = p.RootfsMode
	if p.KernelProfile != "" {
		spec.KernelProfileRef = &corev1.LocalObjectReference{Name: p.KernelProfile}
	}
	if len(p.NodeSelector) > 0 {
		spec.NodeSelector = make(map[string]string, len(p.NodeSelector))
		for k, v := range p.NodeSelector {
			spec.NodeSelector[k] = v
		}
	}
	if p.SandboxPool != "" {
		spec.PoolRef = &corev1.LocalObjectReference{Name: p.SandboxPool}
	}

	rev, err := Fingerprint(&spec)
	if err != nil {
		return nil, err
	}
	return &Template{Spec: spec, Revision: rev}, nil
}

// Fingerprint returns a short hash of every field that affects a running
// sandbox. The spec never contains secret values (see compatibility), so the
// hash cannot leak one; it is also not reversible.
func Fingerprint(spec *sandboxv1alpha1.SwiftSandboxSpec) (string, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("fingerprint sandbox spec: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:revisionLen], nil
}

// Args builds the Spin command line passed to the runtime entrypoint.
//
// User-derived values are always passed in --flag=value form so a value that
// starts with "-" can never be parsed as a separate flag.
func Args(app *spinv1alpha1.SpinApp) []string {
	// --state-dir is deliberately not passed. For a registry application
	// Spin then leaves the state directory unset: the default key-value
	// store and SQLite database are in memory and component output is not
	// written to log files, only to the guest console. Log files on the
	// tmpfs-backed overlay would slowly consume guest RAM. Spin 4.2.1 does
	// not accept an empty --log-dir, so omitting the state directory is the
	// supported way to disable file logging.
	args := []string{
		"up",
		"--from=" + app.Spec.Image,
		"--listen=0.0.0.0:" + strconv.Itoa(runtimecontract.ListenPort),
	}
	if hasRuntimeConfig(app) {
		args = append(args, "--runtime-config-file="+runtimecontract.RuntimeConfigPath)
	}
	for _, c := range app.Spec.Components {
		args = append(args, "--component-id="+c)
	}
	return args
}

// Env builds the sandbox environment. Only literal, non-secret values reach
// this point; compatibility.Analyze rejects every secret-backed source.
func Env(app *spinv1alpha1.SpinApp, p *executor.Profile) ([]corev1.EnvVar, error) {
	var env []corev1.EnvVar
	for _, v := range app.Spec.Variables {
		if v.ValueFrom != nil {
			return nil, fmt.Errorf("variable %q uses valueFrom, which cannot be translated", v.Name)
		}
		env = append(env, corev1.EnvVar{Name: VariableEnvName(v.Name), Value: v.Value})
	}
	if o := p.Otel; o != nil {
		for _, kv := range []struct{ name, value string }{
			{"OTEL_EXPORTER_OTLP_ENDPOINT", o.ExporterOtlpEndpoint},
			{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", o.ExporterOtlpTracesEndpoint},
			{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", o.ExporterOtlpMetricsEndpoint},
			{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", o.ExporterOtlpLogsEndpoint},
		} {
			if kv.value != "" {
				env = append(env, corev1.EnvVar{Name: kv.name, Value: kv.value})
			}
		}
	}
	if limit, ok := app.Spec.InvocationLimits["memory"]; ok {
		q, err := resource.ParseQuantity(limit)
		if err != nil {
			return nil, fmt.Errorf("invocationLimits.memory: %w", err)
		}
		// Same variable and unit (bytes) as Spin Operator's executors.
		env = append(env, corev1.EnvVar{Name: "SPIN_MAX_INSTANCE_MEMORY", Value: strconv.FormatInt(q.Value(), 10)})
	}
	if hasRuntimeConfig(app) {
		doc, err := RuntimeConfigTOML(app.Spec.RuntimeConfig)
		if err != nil {
			return nil, err
		}
		env = append(env, corev1.EnvVar{Name: runtimecontract.RuntimeConfigEnv, Value: base64.StdEncoding.EncodeToString(doc)})
	}
	return env, nil
}

// VariableEnvName maps a Spin variable name to the environment variable read
// by Spin's default environment variables provider.
func VariableEnvName(name string) string {
	return "SPIN_VARIABLE_" + strings.ToUpper(name)
}

func hasRuntimeConfig(app *spinv1alpha1.SpinApp) bool {
	rc := app.Spec.RuntimeConfig
	return len(rc.KeyValueStores) > 0 || len(rc.SqliteDatabases) > 0 || rc.LLMCompute != nil
}

// RuntimeConfigTOML renders the Spin runtime configuration file from the
// SpinApp runtimeConfig. The layout matches what Spin Operator renders for
// its own executors: one table per store, a "type" key, then the options as
// string values.
func RuntimeConfigTOML(rc spinv1alpha1.RuntimeConfig) ([]byte, error) {
	doc := map[string]any{}
	render := func(kind, typ string, opts []spinv1alpha1.RuntimeConfigOption) (map[string]string, error) {
		out := map[string]string{"type": typ}
		for _, o := range opts {
			if o.ValueFrom != nil {
				return nil, fmt.Errorf("%s option %q uses valueFrom, which cannot be translated", kind, o.Name)
			}
			if o.Name == "type" {
				return nil, fmt.Errorf("%s option name \"type\" is reserved", kind)
			}
			out[o.Name] = o.Value
		}
		return out, nil
	}
	if len(rc.KeyValueStores) > 0 {
		stores := map[string]any{}
		for _, s := range rc.KeyValueStores {
			if _, dup := stores[s.Name]; dup {
				return nil, fmt.Errorf("duplicate key-value store %q", s.Name)
			}
			t, err := render("key-value store", s.Type, s.Options)
			if err != nil {
				return nil, err
			}
			stores[s.Name] = t
		}
		doc["key_value_store"] = stores
	}
	if len(rc.SqliteDatabases) > 0 {
		dbs := map[string]any{}
		for _, s := range rc.SqliteDatabases {
			if _, dup := dbs[s.Name]; dup {
				return nil, fmt.Errorf("duplicate SQLite database %q", s.Name)
			}
			t, err := render("SQLite database", s.Type, s.Options)
			if err != nil {
				return nil, err
			}
			dbs[s.Name] = t
		}
		doc["sqlite_database"] = dbs
	}
	if l := rc.LLMCompute; l != nil {
		t, err := render("llmCompute", l.Type, l.Options)
		if err != nil {
			return nil, err
		}
		doc["llm_compute"] = t
	}
	return toml.Marshal(doc)
}

// NewSandbox returns the SwiftSandbox for one replica. The caller sets the
// controller owner reference.
func NewSandbox(app *spinv1alpha1.SpinApp, executorName string, t *Template, ordinal int) *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SandboxName(app.Name, ordinal),
			Namespace: app.Namespace,
			Labels:    Labels(app.Name, executorName, t.Revision, ordinal),
		},
		Spec: *t.Spec.DeepCopy(),
	}
}

// SelectorLabels identifies every sandbox of one SpinApp.
func SelectorLabels(app string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelApp:       AppLabelValue(app),
	}
}

// Labels returns the full label set of one replica.
func Labels(app, executorName, revision string, ordinal int) map[string]string {
	l := SelectorLabels(app)
	l[LabelSpinKubeAppName] = AppLabelValue(app)
	l[LabelOrdinal] = strconv.Itoa(ordinal)
	l[LabelRevision] = revision
	l[LabelExecutor] = AppLabelValue(executorName)
	return l
}
