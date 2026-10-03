// Package compatibility classifies SpinApp fields for the KubeSwift executor
// and reports configuration that cannot be realized faithfully.
//
// Every field of SpinAppSpec appears in Matrix. A test walks the upstream Go
// type and fails when a field is added upstream without being classified
// here, so a new SpinKube field cannot be ignored silently after a dependency
// bump.
package compatibility

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

// Level is the support classification of a SpinApp field.
type Level string

const (
	Supported          Level = "supported"
	PartiallySupported Level = "partially supported"
	Unsupported        Level = "unsupported"
	NotApplicable      Level = "not applicable"
)

// Entry documents how one SpinAppSpec field is handled.
type Entry struct {
	Field string
	Level Level
	Notes string
}

// Matrix is the classification of every SpinAppSpec field, keyed by JSON
// name. docs/compatibility.md mirrors it.
var Matrix = []Entry{
	{"executor", Supported, "Selects the SpinAppExecutor. Only executors labelled spin.kubeswift.io/managed-by=kubeswift-spin are realized."},
	{"image", Supported, "Passed to `spin up --from`. Must be a registry reference reachable from the sandbox without credentials."},
	{"replicas", Supported, "One SwiftSandbox per replica, named <app>-<ordinal>. Bounded by the controller --max-replicas flag."},
	{"resources", PartiallySupported, "cpu and memory size the microVM (see docs/executor-contract.md). Any other resource name is rejected."},
	{"components", Supported, "Passed as `spin up --component-id`, which Spin marks experimental."},
	{"variables", PartiallySupported, "Literal values become SPIN_VARIABLE_* environment variables. valueFrom is rejected: KubeSwift has no secure secret or ConfigMap projection."},
	{"runtimeConfig", PartiallySupported, "keyValueStores, sqliteDatabases and llmCompute with literal, non-credential options are rendered to a runtime-config file. loadFromSecret, valueFrom and credential options are rejected."},
	{"checks", PartiallySupported, "Accepted, but KubeSwift cannot probe a sandbox, so readiness is never reported (see docs/upstream/kubeswift-sandbox-health-probes.md)."},
	{"imagePullSecrets", Unsupported, "Spin pulls the application inside the guest; KubeSwift cannot deliver registry credentials to it securely."},
	{"enableAutoscaling", Unsupported, "SpinApp has no scale subresource and no Deployment exists for an HPA or KEDA to target."},
	{"invocationLimits", PartiallySupported, "memory maps to SPIN_MAX_INSTANCE_MEMORY, as in Spin Operator. Other keys are rejected."},
	{"serviceAccountName", Unsupported, "A sandbox guest has no Kubernetes identity, so a service account cannot be granted to it."},
	{"serviceAnnotations", Supported, "Applied by Spin Operator, which still creates the SpinApp Service when createDeployment is false."},
	{"deploymentAnnotations", NotApplicable, "No Deployment exists. Rejected if set; Spin Operator's webhook also rejects it."},
	{"podAnnotations", NotApplicable, "No pod is created for the application. Rejected if set; Spin Operator's webhook also rejects it."},
	{"podLabels", Unsupported, "KubeSwift launcher pods do not take user labels, so selectors that rely on them would silently match nothing."},
	{"volumes", Unsupported, "KubeSwift has no generic file projection into a sandbox."},
	{"volumeMounts", Unsupported, "KubeSwift has no generic file projection into a sandbox."},
}

// Finding is one problem detected on a SpinApp. Messages identify fields and
// names but never values, so they are safe for status and Events.
type Finding struct {
	Field    string
	Level    Level
	Blocking bool
	Message  string
}

// Options carries the controller limits that analysis enforces.
type Options struct {
	MaxReplicas int32
	Resources   translate.ResourcePolicy
}

var (
	// Spin variable names: lowercase ASCII letters, digits and underscores,
	// starting with a letter.
	spinVariableName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// Spin component IDs are kebab-case.
	spinComponentID = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	// Store and option names used as TOML keys.
	configName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// Analyze returns every finding for app under profile p. A blocking finding
// means the controller must not create or replace sandboxes for the current
// spec.
func Analyze(app *spinv1alpha1.SpinApp, p *executor.Profile, o Options) []Finding {
	var out []Finding
	block := func(field string, level Level, format string, args ...any) {
		out = append(out, Finding{Field: field, Level: level, Blocking: true, Message: fmt.Sprintf(format, args...)})
	}
	note := func(field string, level Level, format string, args ...any) {
		out = append(out, Finding{Field: field, Level: level, Message: fmt.Sprintf(format, args...)})
	}
	s := &app.Spec

	if err := validateAppImage(s.Image); err != nil {
		block("image", Unsupported, "spec.image: %v", err)
	}

	if !s.EnableAutoscaling {
		if s.Replicas < 1 {
			block("replicas", Unsupported, "spec.replicas must be at least 1")
		} else if o.MaxReplicas > 0 && s.Replicas > o.MaxReplicas {
			block("replicas", Unsupported, "spec.replicas is %d, above the limit of %d replicas per SpinApp set by the kubeswift-spin administrator", s.Replicas, o.MaxReplicas)
		}
	} else {
		block("enableAutoscaling", Unsupported,
			"spec.enableAutoscaling is not supported by the KubeSwift executor: SpinApp has no scale subresource for an autoscaler to target; set spec.replicas instead")
	}

	for rn := range s.Resources.Limits {
		if rn != corev1.ResourceCPU && rn != corev1.ResourceMemory {
			block("resources", Unsupported, "spec.resources.limits.%s is not supported; only cpu and memory size a KubeSwift sandbox", rn)
		}
	}
	for rn := range s.Resources.Requests {
		if rn != corev1.ResourceCPU && rn != corev1.ResourceMemory {
			block("resources", Unsupported, "spec.resources.requests.%s is not supported; only cpu and memory size a KubeSwift sandbox", rn)
		}
	}
	if _, err := translate.VCPUs(s.Resources, p.DefaultCPU, o.Resources); err != nil {
		block("resources", Unsupported, "cpu: %v", err)
	}
	if _, err := translate.Memory(s.Resources, p.DefaultMemory, o.Resources); err != nil {
		block("resources", Unsupported, "memory: %v", err)
	}

	seenComponent := map[string]bool{}
	for _, c := range s.Components {
		if !spinComponentID.MatchString(c) {
			block("components", Unsupported, "spec.components entry %q is not a valid Spin component ID", c)
		}
		if seenComponent[c] {
			block("components", Unsupported, "spec.components lists %q more than once", c)
		}
		seenComponent[c] = true
	}

	seenVar := map[string]bool{}
	for _, v := range s.Variables {
		if !spinVariableName.MatchString(v.Name) {
			block("variables", Unsupported, "variable %q is not a valid Spin variable name (lowercase letters, digits and underscores, starting with a letter)", v.Name)
		}
		if seenVar[v.Name] {
			block("variables", Unsupported, "variable %q is defined more than once", v.Name)
		}
		seenVar[v.Name] = true
		if vf := v.ValueFrom; vf != nil {
			switch {
			case vf.SecretKeyRef != nil:
				block("variables", Unsupported,
					"variable %q uses secretKeyRef, but the installed KubeSwift SwiftSandbox API does not provide secure secret projection (see docs/upstream/kubeswift-sandbox-secret-projection.md)", v.Name)
			case vf.ConfigMapKeyRef != nil:
				block("variables", Unsupported,
					"variable %q uses configMapKeyRef, which the KubeSwift executor does not resolve; use a literal value", v.Name)
			default:
				block("variables", Unsupported,
					"variable %q uses a valueFrom source (fieldRef or resourceFieldRef) that has no meaning inside a sandbox guest", v.Name)
			}
		}
	}

	analyzeRuntimeConfig(&s.RuntimeConfig, block)

	if s.Checks.Readiness != nil || s.Checks.Liveness != nil {
		for name, probe := range map[string]*spinv1alpha1.HealthProbe{"readiness": s.Checks.Readiness, "liveness": s.Checks.Liveness} {
			if probe != nil && probe.HTTPGet == nil {
				block("checks", Unsupported, "spec.checks.%s has no httpGet; only HTTP checks are defined by SpinKube", name)
			}
		}
		note("checks", PartiallySupported,
			"spec.checks are accepted but cannot be enforced: the installed KubeSwift SwiftSandbox API has no health probes, so no replica is reported ready (see docs/upstream/kubeswift-sandbox-health-probes.md)")
	}

	if len(s.ImagePullSecrets) > 0 {
		block("imagePullSecrets", Unsupported,
			"spec.imagePullSecrets is not supported: Spin pulls the application inside the sandbox and KubeSwift cannot deliver registry credentials to the guest securely (see docs/upstream/kubeswift-sandbox-artifact-projection.md)")
	}

	for k, v := range s.InvocationLimits {
		if k != "memory" {
			block("invocationLimits", Unsupported, "spec.invocationLimits key %q is not supported; only memory is", k)
			continue
		}
		q, err := resource.ParseQuantity(v)
		if err != nil || q.Sign() <= 0 {
			block("invocationLimits", Unsupported, "spec.invocationLimits.memory must be a positive quantity such as 128Mi")
		}
	}

	if s.ServiceAccountName != "" {
		block("serviceAccountName", Unsupported,
			"spec.serviceAccountName is not supported: a KubeSwift sandbox guest has no Kubernetes identity")
	}
	if len(s.DeploymentAnnotations) > 0 {
		block("deploymentAnnotations", NotApplicable, "spec.deploymentAnnotations cannot be honored: no Deployment is created")
	}
	if len(s.PodAnnotations) > 0 {
		block("podAnnotations", NotApplicable, "spec.podAnnotations cannot be honored: no application pod is created")
	}
	if len(s.PodLabels) > 0 {
		block("podLabels", Unsupported, "spec.podLabels cannot be honored: KubeSwift launcher pods do not carry user labels")
	}
	if len(s.Volumes) > 0 {
		block("volumes", Unsupported, "spec.volumes is not supported: KubeSwift has no generic file projection into a sandbox")
	}
	if len(s.VolumeMounts) > 0 {
		block("volumeMounts", Unsupported, "spec.volumeMounts is not supported: KubeSwift has no generic file projection into a sandbox")
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		return out[i].Message < out[j].Message
	})
	return out
}

func analyzeRuntimeConfig(rc *spinv1alpha1.RuntimeConfig, block func(string, Level, string, ...any)) {
	if rc.LoadFromSecret != "" {
		block("runtimeConfig", Unsupported,
			"spec.runtimeConfig.loadFromSecret is not supported: KubeSwift SwiftSandbox has no secure secret projection (see docs/upstream/kubeswift-sandbox-secret-projection.md)")
	}
	check := func(kind, storeName, typ string, opts []spinv1alpha1.RuntimeConfigOption) {
		if storeName != "" && !configName.MatchString(storeName) {
			block("runtimeConfig", Unsupported, "%s name %q is not valid", kind, storeName)
		}
		if typ == "" {
			block("runtimeConfig", Unsupported, "%s %q has no type", kind, storeName)
		}
		seen := map[string]bool{}
		for _, o := range opts {
			label := fmt.Sprintf("%s %q option %q", kind, storeName, o.Name)
			if !configName.MatchString(o.Name) || o.Name == "type" {
				block("runtimeConfig", Unsupported, "%s: invalid option name", label)
				continue
			}
			if seen[o.Name] {
				block("runtimeConfig", Unsupported, "%s is set more than once", label)
			}
			seen[o.Name] = true
			if vf := o.ValueFrom; vf != nil {
				if vf.SecretKeyRef != nil {
					block("runtimeConfig", Unsupported,
						"%s uses secretKeyRef, but the installed KubeSwift SwiftSandbox API does not provide secure secret projection (see docs/upstream/kubeswift-sandbox-secret-projection.md)", label)
				} else {
					block("runtimeConfig", Unsupported, "%s uses configMapKeyRef, which the KubeSwift executor does not resolve; use a literal value", label)
				}
				continue
			}
			if IsCredentialOption(o.Name) && o.Value != "" {
				block("runtimeConfig", Unsupported,
					"%s looks like a credential; literal credentials are not copied into the sandbox spec, where KubeSwift stores them in a plain ConfigMap, and secret-backed values need KubeSwift secret projection", label)
			}
			if urlHasCredentials(o.Value) {
				block("runtimeConfig", Unsupported,
					"%s contains a URL with embedded credentials, which would be stored in plain text in the sandbox spec", label)
			}
		}
	}
	for _, s := range rc.KeyValueStores {
		check("key-value store", s.Name, s.Type, s.Options)
	}
	for _, s := range rc.SqliteDatabases {
		check("SQLite database", s.Name, s.Type, s.Options)
	}
	if l := rc.LLMCompute; l != nil {
		if l.Type == "spin" {
			block("runtimeConfig", Unsupported,
				"spec.runtimeConfig.llmCompute type \"spin\" (local inference) is not supported in a KubeSwift sandbox; use type \"remote_http\" and an inference endpoint")
		}
		check("llmCompute", "", l.Type, l.Options)
	}
	dup := func(kind string, names []string) {
		seen := map[string]bool{}
		for _, n := range names {
			if seen[n] {
				block("runtimeConfig", Unsupported, "%s %q is defined more than once", kind, n)
			}
			seen[n] = true
		}
	}
	var kv, db []string
	for _, s := range rc.KeyValueStores {
		kv = append(kv, s.Name)
	}
	for _, s := range rc.SqliteDatabases {
		db = append(db, s.Name)
	}
	dup("key-value store", kv)
	dup("SQLite database", db)
}

// IsCredentialOption reports whether a runtime-config option name denotes a
// credential. The list covers the credential options of Spin's built-in
// providers (auth_token, token, password, key, access_key, secret_key, ...).
func IsCredentialOption(n string) bool {
	n = strings.ToLower(n)
	switch n {
	case "token", "password", "secret", "key", "credentials", "connection_string":
		return true
	}
	for _, suffix := range []string{"_token", "_password", "_secret", "_key", "_credentials"} {
		if strings.HasSuffix(n, suffix) {
			return true
		}
	}
	return false
}

func urlHasCredentials(v string) bool {
	if !strings.Contains(v, "@") || !strings.Contains(v, "://") {
		return false
	}
	u, err := url.Parse(v)
	return err == nil && u.User != nil
}

// validateAppImage accepts any registry reference the OCI client can parse,
// but rejects values that are not references at all. Spin also accepts local
// paths and manifest files for --from; none of those exist inside the guest.
func validateAppImage(ref string) error {
	if ref == "" {
		return fmt.Errorf("must be set")
	}
	if strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\r\n") {
		return fmt.Errorf("is not a valid OCI reference")
	}
	if _, err := name.ParseReference(ref, name.WeakValidation); err != nil {
		return fmt.Errorf("is not a valid OCI reference: %w", err)
	}
	return nil
}

// Blocking reports whether any finding blocks reconciliation.
func Blocking(fs []Finding) bool {
	for _, f := range fs {
		if f.Blocking {
			return true
		}
	}
	return false
}

// Summary joins the messages of blocking findings for a condition message.
func Summary(fs []Finding) string {
	var msgs []string
	for _, f := range fs {
		if f.Blocking {
			msgs = append(msgs, f.Message)
		}
	}
	return strings.Join(msgs, "; ")
}
