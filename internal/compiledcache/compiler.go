package compiledcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

// Compiler sandbox constants. They are part of what the signer checks.
const (
	// OutputName is the compiler sandbox's KubeSwift output.
	OutputName = "wasmtime-cache"
	// ArtifactType is the artifactType of exported compiled caches.
	ArtifactType = "application/vnd.kubeswift-spin.wasmtime-cache.v1"
	// ComponentCompiler is the LabelComponent value of compiler sandboxes.
	ComponentCompiler = "compiler"
	// LabelToken carries the compile attempt's token.
	LabelToken = executor.Prefix + "compile-token"
	// compilerTTL removes a compiler sandbox the cache manager did not
	// delete itself (it normally deletes it once its output is signed or it
	// failed).
	compilerTTL = time.Hour
)

// CompilerSettings bound a compiler sandbox.
type CompilerSettings struct {
	CPU        int32
	Memory     resource.Quantity
	Timeout    time.Duration
	MaxSize    resource.Quantity
	MaxEntries int64
}

// DefaultCompilerSettings are the defaults of the cache manager's flags.
func DefaultCompilerSettings() CompilerSettings {
	return CompilerSettings{CPU: 1, Memory: resource.MustParse("1Gi"), Timeout: 10 * time.Minute,
		MaxSize: resource.MustParse("512Mi"), MaxEntries: 10000}
}

// CompilerArgs is the entrypoint command line of a record's compiler.
func CompilerArgs(spec cachev1alpha1.SpinCompiledCacheSpec) []string {
	return []string{runtimecontract.CompileCommand,
		"--source=" + spec.Source, "--runtime=" + spec.Runtime, "--architecture=" + spec.Architecture}
}

// CompilerLabels are the labels of a record's compiler sandbox.
func CompilerLabels(record, token string) map[string]string {
	return map[string]string{
		translate.LabelManagedBy: translate.ManagedByValue,
		LabelComponent:           ComponentCompiler,
		LabelRecord:              record,
		LabelToken:               token,
	}
}

// CompilerSandbox builds the compiler sandbox of a record. app is a current
// consumer: the compiler pulls the application with its imagePullSecrets and
// gets its literal variables, so it can reach exactly what a replica of that
// SpinApp can. Secret-backed variables and runtime configuration are not
// passed: the compiler holds the SpinApp's pull credentials, as a replica
// does, and no other tenant Secret. p is that SpinApp's executor profile:
// the compiler runs where its replicas run (node selector, kernel, network
// posture), so it compiles for their CPU class.
func CompilerSandbox(rec *cachev1alpha1.SpinCompiledCache, app *spinv1alpha1.SpinApp, p *executor.Profile, cs CompilerSettings, token string) *sandboxv1alpha1.SwiftSandbox {
	var env []corev1.EnvVar
	for _, v := range app.Spec.Variables {
		if v.ValueFrom == nil {
			env = append(env, corev1.EnvVar{Name: translate.VariableEnvName(v.Name), Value: v.Value})
		}
	}
	authEnv, files := translate.RegistryAuth(app)
	env = append(env, authEnv...)

	nodeSelector := map[string]string{ArchitectureLabel: rec.Spec.Architecture}
	for k, v := range p.NodeSelector {
		nodeSelector[k] = v
	}
	maxSize, maxEntries := cs.MaxSize.DeepCopy(), cs.MaxEntries
	spec := sandboxv1alpha1.SwiftSandboxSpec{
		Image:       rec.Spec.Runtime,
		CPU:         cs.CPU,
		Memory:      cs.Memory.DeepCopy(),
		Command:     []string{runtimecontract.EntrypointPath},
		Args:        CompilerArgs(rec.Spec),
		Env:         env,
		SecretFiles: files,
		// No ports and no launcher labels: no Service can select it.
		Network:      sandboxv1alpha1.SandboxNetwork{Mode: p.NetworkMode},
		RootfsMode:   p.RootfsMode,
		NodeSelector: nodeSelector,
		Timeout:      &metav1.Duration{Duration: cs.Timeout},
		TTL:          &metav1.Duration{Duration: compilerTTL},
		Outputs: []sandboxv1alpha1.SandboxOutput{{
			Name:         OutputName,
			Path:         runtimecontract.CompileOutputDir,
			Repository:   rec.Spec.Repository,
			MaxSize:      &maxSize,
			MaxEntries:   &maxEntries,
			ArtifactType: ArtifactType,
		}},
	}
	spec.ImagePullSecret = p.RuntimeImagePullSecret
	if p.RuntimeImageVerifyKey != "" {
		spec.VerifyKeySecretRef = &sandboxv1alpha1.SecretObjectReference{Name: p.RuntimeImageVerifyKey}
	}
	if p.KernelProfile != "" {
		spec.KernelProfileRef = &corev1.LocalObjectReference{Name: p.KernelProfile}
	}
	if len(p.EgressAllow) > 0 {
		spec.Network.Egress = &sandboxv1alpha1.SandboxEgress{Allow: append([]sandboxv1alpha1.SandboxEgressRule(nil), p.EgressAllow...)}
	}
	return &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "spin-compile-",
			Namespace:    rec.Spec.Namespace,
			Labels:       CompilerLabels(rec.Name, token),
		},
		Spec: spec,
	}
}

// compilerSpecFields are the only SwiftSandbox spec fields a compiler may
// have. Any other field (artifacts, model, scratch disk, GPU, working
// directory, probes, pool, pod metadata) could change what runs in the
// compiler, so a sandbox that has one is never signed for.
var compilerSpecFields = map[string]bool{
	"image": true, "imagePullSecret": true, "verifyKeySecretRef": true, "cpu": true, "memory": true,
	"command": true, "args": true, "env": true, "secretFiles": true, "timeout": true, "ttl": true,
	"network": true, "rootfsMode": true, "kernelProfileRef": true, "nodeSelector": true, "outputs": true,
}

// SpecHash is the SHA-256 of a sandbox's spec as the API server stores it,
// every field included except ttl (the one field KubeSwift lets change).
// The cache manager records it right after creating the compiler; the signer
// compares the sandbox it checks with it.
func SpecHash(sb *unstructured.Unstructured) (string, error) {
	spec, ok, err := unstructured.NestedMap(sb.Object, "spec")
	if err != nil || !ok {
		return "", fmt.Errorf("sandbox %s has no spec", sb.GetName())
	}
	delete(spec, "ttl")
	b, err := json.Marshal(spec) // map keys are encoded in sorted order
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

var (
	compilerEnvName  = regexp.MustCompile(`^SPIN_VARIABLE_[A-Z0-9_]+$`)
	registryAuthFile = regexp.MustCompile(`^` + regexp.QuoteMeta(runtimecontract.RegistryAuthDir) + `/[0-9]+\.json$`)
)

// CheckCompiler is the signer's check of a record's compiler sandbox, read
// as stored. It relies only on what neither a tenant nor a compromised cache
// manager can change after the fact: the UID recorded from the create
// response, the spec (immutable while the sandbox is not being deleted) and
// its recorded hash, and an independent check of every spec field. Labels
// and status are not evidence.
func CheckCompiler(rec *cachev1alpha1.SpinCompiledCache, obj *unstructured.Unstructured, runtimes []string) error {
	c := rec.Status.Compile
	switch {
	case c == nil || c.Sandbox == nil || c.SpecHash == "":
		return fmt.Errorf("record %s names no compiler sandbox", rec.Name)
	case obj.GetNamespace() != rec.Spec.Namespace || obj.GetName() != c.Sandbox.Name || obj.GetUID() != c.Sandbox.UID:
		return fmt.Errorf("sandbox %s/%s is not the compiler sandbox the record names", obj.GetNamespace(), obj.GetName())
	case obj.GetDeletionTimestamp() != nil:
		return fmt.Errorf("compiler sandbox %s is being deleted; its spec is no longer protected", obj.GetName())
	case !slices.Contains(runtimes, rec.Spec.Runtime):
		return fmt.Errorf("runtime %s is not an allowed runtime", rec.Spec.Runtime)
	}
	h, err := SpecHash(obj)
	if err != nil {
		return err
	}
	if h != c.SpecHash {
		return fmt.Errorf("compiler sandbox %s has another spec than the one the cache manager created", obj.GetName())
	}
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	for k := range spec {
		if !compilerSpecFields[k] {
			return fmt.Errorf("compiler sandbox %s sets spec.%s, which a compiler never has", obj.GetName(), k)
		}
	}
	var sb sandboxv1alpha1.SwiftSandbox
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &sb); err != nil {
		return fmt.Errorf("decode compiler sandbox %s: %w", obj.GetName(), err)
	}
	if err := checkCompilerSpec(rec, &sb); err != nil {
		return err
	}
	for _, e := range sb.Spec.Env {
		if e.ValueFrom != nil || (e.Name != runtimecontract.RegistryAuthFilesEnv && !compilerEnvName.MatchString(e.Name)) {
			return fmt.Errorf("compiler sandbox %s has environment variable %s, which a compiler never has", sb.Name, e.Name)
		}
	}
	for _, f := range sb.Spec.SecretFiles {
		for _, it := range f.Items {
			if it.Key != corev1.DockerConfigJsonKey || !registryAuthFile.MatchString(it.Path) {
				return fmt.Errorf("compiler sandbox %s has a secret file other than registry credentials", sb.Name)
			}
		}
	}
	n := sb.Spec.Network
	switch {
	case len(n.Ports) > 0 || n.Ingress != nil:
		return fmt.Errorf("compiler sandbox %s exposes ports", sb.Name)
	case n.Mode != sandboxv1alpha1.SandboxNetworkRestricted && n.Mode != sandboxv1alpha1.SandboxNetworkOpen:
		return fmt.Errorf("compiler sandbox %s has network mode %q", sb.Name, n.Mode)
	case sb.Spec.NodeSelector[ArchitectureLabel] != rec.Spec.Architecture:
		return fmt.Errorf("compiler sandbox %s does not select %s nodes", sb.Name, rec.Spec.Architecture)
	}
	return nil
}

// IsCompilerShaped reports whether a sandbox looks like a compiler the cache
// manager creates: it runs compile mode with an allowed runtime and exports
// the compiled cache output. The cache manager deletes only such sandboxes,
// so labels alone never make it delete a replica or any other sandbox.
func IsCompilerShaped(sb *sandboxv1alpha1.SwiftSandbox, runtimes []string) bool {
	return slices.Contains(runtimes, sb.Spec.Image) &&
		slices.Equal(sb.Spec.Command, []string{runtimecontract.EntrypointPath}) &&
		len(sb.Spec.Args) > 0 && sb.Spec.Args[0] == runtimecontract.CompileCommand &&
		len(sb.Spec.Outputs) == 1 && sb.Spec.Outputs[0].Name == OutputName
}

func checkCompilerSpec(rec *cachev1alpha1.SpinCompiledCache, sb *sandboxv1alpha1.SwiftSandbox) error {
	switch {
	case sb.Spec.Image != rec.Spec.Runtime:
		return fmt.Errorf("sandbox %s runs %s, not the record's runtime", sb.Name, sb.Spec.Image)
	case !slices.Equal(sb.Spec.Command, []string{runtimecontract.EntrypointPath}) || !slices.Equal(sb.Spec.Args, CompilerArgs(rec.Spec)):
		return fmt.Errorf("sandbox %s does not run compile mode for the record's identity", sb.Name)
	case len(sb.Spec.Outputs) != 1 || sb.Spec.Outputs[0].Name != OutputName || sb.Spec.Outputs[0].Path != runtimecontract.CompileOutputDir ||
		sb.Spec.Outputs[0].Repository != rec.Spec.Repository:
		return fmt.Errorf("sandbox %s does not export the record's output", sb.Name)
	case sb.Spec.PoolRef != nil:
		return fmt.Errorf("sandbox %s checks out from a pool", sb.Name)
	}
	return nil
}

// ExportTag is the tag KubeSwift's export Job gives an exported output:
// <sandbox UID>.<output name>. Only that Job pushes it, so the signer
// resolves it to bind the digest to the compiler sandbox without trusting
// the sandbox's status.
func ExportTag(sb *sandboxv1alpha1.SwiftSandbox) string {
	return string(sb.UID) + "." + OutputName
}

// ExportedArtifact checks that sb is the record's compiler sandbox of the
// current attempt, that it ran compile mode for the record's identity, and
// that KubeSwift exported its output to the record's repository. It returns
// the exported reference (repository@sha256:...). The cache manager uses it
// to record the artifact and the signer to check it again before signing.
func ExportedArtifact(rec *cachev1alpha1.SpinCompiledCache, sb *sandboxv1alpha1.SwiftSandbox) (string, error) {
	c := rec.Status.Compile
	switch {
	case c == nil || c.Sandbox == nil:
		return "", fmt.Errorf("record %s has no compiler sandbox", rec.Name)
	case sb.Namespace != rec.Spec.Namespace || sb.Name != c.Sandbox.Name || sb.UID != c.Sandbox.UID:
		return "", fmt.Errorf("sandbox %s/%s (uid %s) is not the compiler sandbox the record names", sb.Namespace, sb.Name, sb.UID)
	case sb.Labels[LabelRecord] != rec.Name || sb.Labels[LabelComponent] != ComponentCompiler || sb.Labels[LabelToken] != c.Token:
		return "", fmt.Errorf("sandbox %s does not carry the record's compiler labels", sb.Name)
	}
	if err := checkCompilerSpec(rec, sb); err != nil {
		return "", err
	}
	switch {
	case sb.Status.Phase != sandboxv1alpha1.SwiftSandboxCompleted:
		return "", fmt.Errorf("sandbox %s is %s, not Completed", sb.Name, sb.Status.Phase)
	}
	for _, o := range sb.Status.Outputs {
		if o.Name != OutputName {
			continue
		}
		if o.State != sandboxv1alpha1.SandboxOutputExported || o.Digest == "" {
			return "", fmt.Errorf("output %s is %s", o.Name, o.State)
		}
		ref := rec.Spec.Repository + "@" + o.Digest
		if o.Repository != rec.Spec.Repository || o.Reference != ref {
			return "", fmt.Errorf("output %s was exported to %s, not the record's repository", o.Name, o.Reference)
		}
		if _, err := PinnedReference(ref); err != nil {
			return "", fmt.Errorf("output %s: %w", o.Name, err)
		}
		return ref, nil
	}
	return "", fmt.Errorf("sandbox %s reports no output %s", sb.Name, OutputName)
}

// CompileFailure is why a compiler sandbox failed, as a reason and message.
func CompileFailure(sb *sandboxv1alpha1.SwiftSandbox) (string, string) {
	for _, o := range sb.Status.Outputs {
		if o.Name == OutputName && (o.State == sandboxv1alpha1.SandboxOutputFailed || o.State == sandboxv1alpha1.SandboxOutputSkipped) && o.Reason != "" {
			return o.Reason, firstNonEmpty(o.Message, "the output was not exported")
		}
	}
	for _, c := range sb.Status.Conditions {
		if c.Type == sandboxv1alpha1.SwiftSandboxConditionGuestRunning && c.Status == metav1.ConditionFalse && c.Reason != "" {
			return c.Reason, firstNonEmpty(c.Message, "the compiler failed")
		}
	}
	if sb.Status.ExitCode != nil && *sb.Status.ExitCode != 0 {
		return "CompilerExited", fmt.Sprintf("compile mode exited %d; see the sandbox console", *sb.Status.ExitCode)
	}
	return "CompilerFailed", firstNonEmpty(sb.Status.Message, "the compiler sandbox failed")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
