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
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
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
	{"image", Supported, "Passed to `spin up --from`. Must be a registry reference reachable from the sandbox; private registries need imagePullSecrets."},
	{"replicas", Supported, "One SwiftSandbox per replica, named <app>-<ordinal>. Bounded by the controller --max-replicas flag."},
	{"resources", PartiallySupported, "cpu and memory size the microVM (see docs/executor-contract.md). Any other resource name is rejected."},
	{"components", Supported, "Passed as `spin up --component-id`, which Spin marks experimental."},
	{"variables", PartiallySupported, "Literal values and secretKeyRef (KubeSwift v0.16.0 or later) become SPIN_VARIABLE_* environment variables. configMapKeyRef, fieldRef and resourceFieldRef are rejected."},
	{"runtimeConfig", PartiallySupported, "loadFromSecret, keyValueStores, sqliteDatabases and llmCompute are rendered into the runtime-config file; Secret-backed options need KubeSwift v0.16.0. configMapKeyRef, literal credentials and llmCompute type spin are rejected."},
	{"checks", Supported, "Become the sandbox readiness and liveness probes against the Spin HTTP port (KubeSwift v0.16.0 or later). Without a readiness check a TCP check is used."},
	{"imagePullSecrets", Supported, "Delivered as secret files to the Spin process, which pulls the application (KubeSwift v0.16.0 or later). The credentials are in the guest, outside the Wasm sandbox."},
	{"enableAutoscaling", Unsupported, "SpinApp has no scale subresource and no Deployment exists for an HPA or KEDA to target."},
	{"invocationLimits", PartiallySupported, "memory maps to SPIN_MAX_INSTANCE_MEMORY, as in Spin Operator. Other keys are rejected."},
	{"serviceAccountName", Unsupported, "A sandbox guest has no Kubernetes identity, so a service account cannot be granted to it."},
	{"serviceAnnotations", Supported, "Applied by Spin Operator, which creates the SpinApp Service when createDeployment is false."},
	{"deploymentAnnotations", NotApplicable, "No Deployment exists. Rejected if set; Spin Operator's webhook also rejects it."},
	{"podAnnotations", NotApplicable, "No application pod exists. Rejected if set; Spin Operator's webhook also rejects it."},
	{"podLabels", Supported, "Added to the KubeSwift launcher pod (KubeSwift v0.16.0 or later). Keys under kubeswift.io and core.spinkube.dev are rejected."},
	{"volumes", Unsupported, "KubeSwift mounts only Secret files and OCI artifacts into a sandbox, not Kubernetes volumes."},
	{"volumeMounts", Unsupported, "KubeSwift mounts only Secret files and OCI artifacts into a sandbox, not Kubernetes volumes."},
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
	// Features are the SwiftSandbox features detected on the cluster. Fields
	// that need a missing feature are rejected.
	Features capabilities.Sandbox
}

const (
	needSecretsMsg  = "the installed KubeSwift does not provide Secret projection into sandboxes (KubeSwift v0.16.0 or later)"
	needExposureMsg = "the installed KubeSwift does not provide sandbox port exposure, probes and launcher pod metadata (KubeSwift v0.16.0 or later)"
)

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
			block("components", Unsupported, "spec.components entry %s is not a valid Spin component ID", quoteName(c))
		}
		if seenComponent[c] {
			block("components", Unsupported, "spec.components lists %s more than once", quoteName(c))
		}
		seenComponent[c] = true
	}

	seenVar := map[string]bool{}
	for _, v := range s.Variables {
		if !spinVariableName.MatchString(v.Name) {
			block("variables", Unsupported, "variable %s is not a valid Spin variable name (lowercase letters, digits and underscores, starting with a letter)", quoteName(v.Name))
		}
		if seenVar[v.Name] {
			block("variables", Unsupported, "variable %s is defined more than once", quoteName(v.Name))
		}
		seenVar[v.Name] = true
		if vf := v.ValueFrom; vf != nil {
			switch {
			case vf.SecretKeyRef != nil:
				if !o.Features.Secrets() {
					block("variables", Unsupported, "variable %s uses secretKeyRef, but %s", quoteName(v.Name), needSecretsMsg)
				}
			case vf.ConfigMapKeyRef != nil:
				block("variables", Unsupported,
					"variable %s uses configMapKeyRef, which KubeSwift does not resolve for sandboxes; use a literal value or a Secret", quoteName(v.Name))
			default:
				block("variables", Unsupported,
					"variable %s uses a valueFrom source (fieldRef or resourceFieldRef) that has no meaning inside a sandbox guest", quoteName(v.Name))
			}
		}
	}

	analyzeRuntimeConfig(&s.RuntimeConfig, o.Features, block)

	if s.Checks.Readiness != nil || s.Checks.Liveness != nil {
		for name, probe := range map[string]*spinv1alpha1.HealthProbe{"readiness": s.Checks.Readiness, "liveness": s.Checks.Liveness} {
			if probe == nil {
				continue
			}
			if probe.HTTPGet == nil {
				block("checks", Unsupported, "spec.checks.%s has no httpGet; only HTTP checks are defined by SpinKube", name)
			} else if !strings.HasPrefix(probe.HTTPGet.Path, "/") {
				block("checks", Unsupported, "spec.checks.%s.httpGet.path must start with /", name)
			}
		}
		if !o.Features.Exposure() {
			note("checks", PartiallySupported, "spec.checks are accepted but cannot be enforced: "+needExposureMsg)
		}
	}

	if len(s.ImagePullSecrets) > 0 && !o.Features.Secrets() {
		block("imagePullSecrets", Unsupported,
			"spec.imagePullSecrets needs registry credentials inside the sandbox, where Spin pulls the application, but "+needSecretsMsg)
	}
	for i, ps := range s.ImagePullSecrets {
		if ps.Name == "" {
			block("imagePullSecrets", Unsupported, "spec.imagePullSecrets[%d] has no name", i)
		}
	}

	if o.Features.Exposure() {
		// The Service Spin Operator creates selects
		// core.spinkube.dev/app.<name>.status, whose name part must be a
		// valid label name of at most 63 characters.
		if errs := validation.IsQualifiedName(translate.StatusLabelKey(app.Name)); len(errs) > 0 {
			block("name", Unsupported,
				"the SpinApp name is too long to be selected by the SpinApp Service (label %s is invalid); use a name of at most 52 characters", quoteName(translate.StatusLabelKey(app.Name)))
		}
	}

	for k, v := range s.InvocationLimits {
		if k != "memory" {
			block("invocationLimits", Unsupported, "spec.invocationLimits key %s is not supported; only memory is", quoteName(k))
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
		if !o.Features.Exposure() {
			block("podLabels", Unsupported, "spec.podLabels cannot be honored: "+needExposureMsg)
		}
		for k, v := range s.PodLabels {
			prefix, _, hasPrefix := strings.Cut(k, "/")
			switch {
			case len(validation.IsQualifiedName(k)) > 0 || len(validation.IsValidLabelValue(v)) > 0:
				block("podLabels", Unsupported, "spec.podLabels key %s or its value is not a valid label", quoteName(k))
			case hasPrefix && (prefix == "kubeswift.io" || strings.HasSuffix(prefix, ".kubeswift.io")):
				block("podLabels", Unsupported, "spec.podLabels key %s is under a KubeSwift-reserved domain", quoteName(k))
			case hasPrefix && prefix == "core.spinkube.dev":
				block("podLabels", Unsupported, "spec.podLabels key %s is under core.spinkube.dev, which kubeswift-spin and Spin Operator set", quoteName(k))
			}
		}
	}
	if len(s.Volumes) > 0 {
		block("volumes", Unsupported, "spec.volumes is not supported: KubeSwift mounts only Secret files and OCI artifacts into a sandbox")
	}
	if len(s.VolumeMounts) > 0 {
		block("volumeMounts", Unsupported, "spec.volumeMounts is not supported: KubeSwift mounts only Secret files and OCI artifacts into a sandbox")
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		return out[i].Message < out[j].Message
	})
	return out
}

func analyzeRuntimeConfig(rc *spinv1alpha1.RuntimeConfig, f capabilities.Sandbox, block func(string, Level, string, ...any)) {
	if rc.LoadFromSecret != "" {
		if !f.Secrets() {
			block("runtimeConfig", Unsupported, "spec.runtimeConfig.loadFromSecret is not supported: "+needSecretsMsg)
		}
		if len(rc.KeyValueStores) > 0 || len(rc.SqliteDatabases) > 0 || rc.LLMCompute != nil {
			// Spin Operator silently ignores the other fields in this case;
			// kubeswift-spin reports the conflict instead.
			block("runtimeConfig", Unsupported,
				"spec.runtimeConfig.loadFromSecret replaces the whole runtime configuration; remove keyValueStores, sqliteDatabases and llmCompute or loadFromSecret")
		}
	}
	check := func(kind, storeName, typ string, opts []spinv1alpha1.RuntimeConfigOption) {
		if storeName != "" && !configName.MatchString(storeName) {
			block("runtimeConfig", Unsupported, "%s name %s is not valid", kind, quoteName(storeName))
		}
		if typ == "" {
			block("runtimeConfig", Unsupported, "%s %s has no type", kind, quoteName(storeName))
		}
		seen := map[string]bool{}
		for _, o := range opts {
			label := fmt.Sprintf("%s %s option %s", kind, quoteName(storeName), quoteName(o.Name))
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
					if !f.Secrets() {
						block("runtimeConfig", Unsupported, "%s uses secretKeyRef, but %s", label, needSecretsMsg)
					}
				} else {
					block("runtimeConfig", Unsupported, "%s uses configMapKeyRef, which KubeSwift does not resolve for sandboxes; use a literal value or a Secret", label)
				}
				continue
			}
			if IsCredentialOption(o.Name) && o.Value != "" {
				block("runtimeConfig", Unsupported,
					"%s looks like a credential; literal values are stored in plain text in the sandbox spec, so reference a Secret with valueFrom.secretKeyRef instead", label)
			}
			if urlHasCredentials(o.Value) {
				block("runtimeConfig", Unsupported,
					"%s contains a URL with embedded credentials, which would be stored in plain text in the sandbox spec; reference a Secret with valueFrom.secretKeyRef instead", label)
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
				block("runtimeConfig", Unsupported, "%s %s is defined more than once", kind, quoteName(n))
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

// urlHasCredentials reports whether a value looks like a URL carrying
// credentials, either as user information or as a credential-named query
// parameter. It fails closed: it inspects the raw text and does not depend on
// the URL parsing successfully, because Spin's URL parser accepts inputs
// that Go's rejects.
func urlHasCredentials(v string) bool {
	_, rest, ok := strings.Cut(v, "://")
	if !ok {
		return false
	}
	authority, tail := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority, tail = rest[:i], rest[i:]
	}
	if strings.Contains(authority, "@") {
		return true
	}
	_, query, ok := strings.Cut(tail, "?")
	if !ok {
		return false
	}
	query, _, _ = strings.Cut(query, "#")
	for _, pair := range strings.Split(query, "&") {
		name, _, _ := strings.Cut(pair, "=")
		if unescaped, err := url.QueryUnescape(name); err == nil {
			name = unescaped
		}
		name = strings.ToLower(name)
		for _, word := range []string{"token", "password", "passwd", "secret", "auth", "key", "sig", "credential"} {
			if strings.Contains(name, word) {
				return true
			}
		}
	}
	return false
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

// maxSummarized bounds how many findings a condition message lists, so a
// SpinApp with hundreds of invalid entries still gets a status update (the
// CRD limits condition messages to 32768 bytes).
const maxSummarized = 5

// Summary joins the messages of blocking findings for a condition message.
func Summary(fs []Finding) string {
	var msgs []string
	n := 0
	for _, f := range fs {
		if !f.Blocking {
			continue
		}
		n++
		if len(msgs) < maxSummarized {
			msgs = append(msgs, f.Message)
		}
	}
	if n > maxSummarized {
		msgs = append(msgs, fmt.Sprintf("and %d more problems", n-maxSummarized))
	}
	return strings.Join(msgs, "; ")
}

// quoteName quotes a user-provided name for a message, bounded in length.
func quoteName(s string) string {
	const maxName = 64
	if len(s) > maxName {
		s = s[:maxName] + "..."
	}
	return strconv.Quote(s)
}
