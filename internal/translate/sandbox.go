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
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"

	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
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

// BuildTemplate builds the replica template for app under profile, using the
// SwiftSandbox features f detected on the cluster. Features absent on the
// cluster (KubeSwift before v0.16.0) are never used: the API server would
// silently prune fields its schema does not know.
func BuildTemplate(app *spinv1alpha1.SpinApp, p *executor.Profile, policy ResourcePolicy, f capabilities.Sandbox) (*Template, error) {
	cpu, err := VCPUs(app.Spec.Resources, p.DefaultCPU, policy)
	if err != nil {
		return nil, err
	}
	mem, err := Memory(app.Spec.Resources, p.DefaultMemory, policy)
	if err != nil {
		return nil, err
	}
	env, files, err := Env(app, p, f)
	if err != nil {
		return nil, err
	}

	spec := sandboxv1alpha1.SwiftSandboxSpec{
		Image:       p.RuntimeImage,
		CPU:         cpu,
		Memory:      mem,
		Command:     []string{runtimecontract.EntrypointPath},
		Args:        Args(app),
		Env:         env,
		SecretFiles: files,
		Network:     sandboxv1alpha1.SandboxNetwork{Mode: p.NetworkMode},
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
	if len(p.EgressAllow) > 0 {
		if !f.Egress {
			return nil, fmt.Errorf("executor %q sets %s, but the installed KubeSwift has no sandbox egress allowlist", p.ExecutorName, executor.AnnEgressAllow)
		}
		spec.Network.Egress = &sandboxv1alpha1.SandboxEgress{Allow: append([]sandboxv1alpha1.SandboxEgressRule(nil), p.EgressAllow...)}
	}
	if f.Exposure() {
		expose(&spec, app, p)
	}

	rev, err := Fingerprint(&spec)
	if err != nil {
		return nil, err
	}
	return &Template{Spec: spec, Revision: rev}, nil
}

// StatusLabelKey is the pod label Spin Operator's SpinApp Service selects
// (core.spinkube.dev/app.<name>.status=ready).
func StatusLabelKey(app string) string {
	return "core.spinkube.dev/app." + app + ".status"
}

// expose makes the replica reachable through the SpinApp Service that Spin
// Operator creates: the Spin listener becomes the named port the Service
// targets, the launcher pod gets the label the Service selects, and the
// readiness probe decides when the replica is an endpoint.
func expose(spec *sandboxv1alpha1.SwiftSandboxSpec, app *spinv1alpha1.SpinApp, p *executor.Profile) {
	spec.Network.Ports = []sandboxv1alpha1.SandboxPort{{
		Name: runtimecontract.HTTPPortName, Port: runtimecontract.ListenPort, Protocol: corev1.ProtocolTCP,
	}}
	if len(p.IngressFrom) > 0 {
		spec.Network.Ingress = &sandboxv1alpha1.SandboxIngress{From: append([]networkingv1.NetworkPolicyPeer(nil), p.IngressFrom...)}
	}
	labels := map[string]string{}
	for k, v := range app.Spec.PodLabels {
		labels[k] = v
	}
	labels[LabelSpinKubeAppName] = app.Name
	labels[StatusLabelKey(app.Name)] = "ready"
	spec.PodMetadata = &sandboxv1alpha1.SandboxPodMetadata{Labels: labels}

	port := intstr.FromString(runtimecontract.HTTPPortName)
	spec.ReadinessProbe = probe(app.Spec.Checks.Readiness, port)
	if spec.ReadinessProbe == nil {
		// Without a check, the replica is ready once Spin accepts TCP
		// connections on its listener.
		spec.ReadinessProbe = &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: port}},
			PeriodSeconds:    2,
			TimeoutSeconds:   1,
			SuccessThreshold: 1,
			FailureThreshold: 3,
		}
	}
	spec.LivenessProbe = probe(app.Spec.Checks.Liveness, port)
}

// probe maps a SpinKube HTTP health check onto a probe against the guest.
func probe(h *spinv1alpha1.HealthProbe, port intstr.IntOrString) *corev1.Probe {
	if h == nil || h.HTTPGet == nil {
		return nil
	}
	pr := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path:   h.HTTPGet.Path,
			Port:   port,
			Scheme: corev1.URISchemeHTTP,
		}},
		InitialDelaySeconds: h.InitialDelaySeconds,
		TimeoutSeconds:      h.TimeoutSeconds,
		PeriodSeconds:       h.PeriodSeconds,
		SuccessThreshold:    h.SuccessThreshold,
		FailureThreshold:    h.FailureThreshold,
	}
	for _, hdr := range h.HTTPGet.HTTPHeaders {
		pr.HTTPGet.HTTPHeaders = append(pr.HTTPGet.HTTPHeaders, corev1.HTTPHeader{Name: hdr.Name, Value: hdr.Value})
	}
	return pr
}

// Fingerprint returns a short hash of every field that affects a running
// sandbox. Secret values never appear in the spec, only references to them,
// so the hash cannot leak one; it is also not reversible. Rotating a Secret
// therefore does not replace replicas.
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

// secretEnv is an environment variable whose value KubeSwift reads from a
// Secret on the host side and hands to the guest; the value is never written
// to the SwiftSandbox or any other object.
func secretEnv(name string, ref *corev1.SecretKeySelector) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: ref.DeepCopy()}}
}

// Env builds the sandbox environment and secret files. Literal values are
// stored in plain text by KubeSwift; Secret-backed values are passed as
// references only, which requires f.Secrets() (KubeSwift v0.16.0).
func Env(app *spinv1alpha1.SpinApp, p *executor.Profile, f capabilities.Sandbox) ([]corev1.EnvVar, []sandboxv1alpha1.SandboxSecretFile, error) {
	var env []corev1.EnvVar
	var files []sandboxv1alpha1.SandboxSecretFile
	needSecrets := func(what string) error {
		if f.Secrets() {
			return nil
		}
		return fmt.Errorf("%s needs Secret projection, which the installed KubeSwift does not provide", what)
	}

	for _, v := range app.Spec.Variables {
		switch {
		case v.ValueFrom == nil:
			env = append(env, corev1.EnvVar{Name: VariableEnvName(v.Name), Value: v.Value})
		case v.ValueFrom.SecretKeyRef != nil:
			if err := needSecrets(fmt.Sprintf("variable %q", v.Name)); err != nil {
				return nil, nil, err
			}
			env = append(env, secretEnv(VariableEnvName(v.Name), v.ValueFrom.SecretKeyRef))
		default:
			return nil, nil, fmt.Errorf("variable %q uses a valueFrom source that cannot be translated", v.Name)
		}
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
			return nil, nil, fmt.Errorf("invocationLimits.memory: %w", err)
		}
		// Same variable and unit (bytes) as Spin Operator's executors.
		env = append(env, corev1.EnvVar{Name: "SPIN_MAX_INSTANCE_MEMORY", Value: strconv.FormatInt(q.Value(), 10)})
	}

	rc := app.Spec.RuntimeConfig
	switch {
	case rc.LoadFromSecret != "":
		if err := needSecrets("spec.runtimeConfig.loadFromSecret"); err != nil {
			return nil, nil, err
		}
		files = append(files, sandboxv1alpha1.SandboxSecretFile{
			SecretName: rc.LoadFromSecret,
			Items:      []sandboxv1alpha1.SandboxSecretFileItem{{Key: "runtime-config.toml", Path: runtimecontract.SecretRuntimeConfigPath}},
		})
		env = append(env, corev1.EnvVar{Name: runtimecontract.RuntimeConfigFileEnv, Value: runtimecontract.SecretRuntimeConfigPath})
	case hasRuntimeConfig(app):
		doc, refs, err := RuntimeConfigTOML(rc)
		if err != nil {
			return nil, nil, err
		}
		if len(refs) > 0 {
			if err := needSecrets("a Secret-backed runtime-config option"); err != nil {
				return nil, nil, err
			}
		}
		env = append(env, corev1.EnvVar{Name: runtimecontract.RuntimeConfigEnv, Value: base64.StdEncoding.EncodeToString(doc)})
		for _, r := range refs {
			env = append(env, secretEnv(r.env, r.ref))
		}
	}

	if len(app.Spec.ImagePullSecrets) > 0 {
		if err := needSecrets("spec.imagePullSecrets"); err != nil {
			return nil, nil, err
		}
		var paths []string
		for i, ps := range app.Spec.ImagePullSecrets {
			path := fmt.Sprintf("%s/%d.json", runtimecontract.RegistryAuthDir, i)
			paths = append(paths, path)
			files = append(files, sandboxv1alpha1.SandboxSecretFile{
				SecretName: ps.Name,
				Items:      []sandboxv1alpha1.SandboxSecretFileItem{{Key: corev1.DockerConfigJsonKey, Path: path}},
			})
		}
		env = append(env, corev1.EnvVar{Name: runtimecontract.RegistryAuthFilesEnv, Value: strings.Join(paths, ",")})
	}
	return env, files, nil
}

// VariableEnvName maps a Spin variable name to the environment variable read
// by Spin's default environment variables provider.
func VariableEnvName(name string) string {
	return "SPIN_VARIABLE_" + strings.ToUpper(name)
}

func hasRuntimeConfig(app *spinv1alpha1.SpinApp) bool {
	rc := app.Spec.RuntimeConfig
	return rc.LoadFromSecret != "" || len(rc.KeyValueStores) > 0 || len(rc.SqliteDatabases) > 0 || rc.LLMCompute != nil
}

// secretRef is a runtime-config option value delivered through a Secret.
type secretRef struct {
	env string
	ref *corev1.SecretKeySelector
}

// RuntimeConfigTOML renders the Spin runtime configuration file from the
// SpinApp runtimeConfig. The layout matches what Spin Operator renders for
// its own executors: one table per store, a "type" key, then the options as
// string values. An option backed by a Secret is rendered as a placeholder
// (runtimecontract.SecretPlaceholderPrefix + environment variable name) and
// returned as a reference; the entrypoint substitutes the value in the
// guest, so it never appears in the sandbox spec.
func RuntimeConfigTOML(rc spinv1alpha1.RuntimeConfig) ([]byte, []secretRef, error) {
	doc := map[string]any{}
	var refs []secretRef
	render := func(kind, typ string, opts []spinv1alpha1.RuntimeConfigOption) (map[string]string, error) {
		out := map[string]string{"type": typ}
		for _, o := range opts {
			if o.Name == "type" {
				return nil, fmt.Errorf("%s option name \"type\" is reserved", kind)
			}
			switch {
			case o.ValueFrom == nil:
				out[o.Name] = o.Value
			case o.ValueFrom.SecretKeyRef != nil:
				name := fmt.Sprintf("%s%d", runtimecontract.SecretValueEnvPrefix, len(refs))
				refs = append(refs, secretRef{env: name, ref: o.ValueFrom.SecretKeyRef})
				out[o.Name] = runtimecontract.SecretPlaceholderPrefix + name
			default:
				return nil, fmt.Errorf("%s option %q uses a valueFrom source that cannot be translated", kind, o.Name)
			}
		}
		return out, nil
	}
	if len(rc.KeyValueStores) > 0 {
		stores := map[string]any{}
		for _, s := range rc.KeyValueStores {
			if _, dup := stores[s.Name]; dup {
				return nil, nil, fmt.Errorf("duplicate key-value store %q", s.Name)
			}
			t, err := render("key-value store", s.Type, s.Options)
			if err != nil {
				return nil, nil, err
			}
			stores[s.Name] = t
		}
		doc["key_value_store"] = stores
	}
	if len(rc.SqliteDatabases) > 0 {
		dbs := map[string]any{}
		for _, s := range rc.SqliteDatabases {
			if _, dup := dbs[s.Name]; dup {
				return nil, nil, fmt.Errorf("duplicate SQLite database %q", s.Name)
			}
			t, err := render("SQLite database", s.Type, s.Options)
			if err != nil {
				return nil, nil, err
			}
			dbs[s.Name] = t
		}
		doc["sqlite_database"] = dbs
	}
	if l := rc.LLMCompute; l != nil {
		t, err := render("llmCompute", l.Type, l.Options)
		if err != nil {
			return nil, nil, err
		}
		doc["llm_compute"] = t
	}
	b, err := toml.Marshal(doc)
	return b, refs, err
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
