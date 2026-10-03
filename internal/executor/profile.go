// Package executor decides whether a SpinAppExecutor belongs to kubeswift-spin
// and turns its metadata into an execution profile.
//
// SpinAppExecutor has no extension fields, so a KubeSwift execution profile is
// expressed with labels and annotations under the spin.kubeswift.io/ prefix.
// Several executors (for example "kubeswift" and "kubeswift-open") can carry
// different profiles without any additional CRD.
package executor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"
)

const (
	// Prefix is the label and annotation key prefix owned by this project.
	Prefix = "spin.kubeswift.io/"

	// ManagedByLabel marks a SpinAppExecutor as realized by kubeswift-spin.
	// The executor name alone is never used to infer ownership.
	ManagedByLabel = Prefix + "managed-by"
	// ManagedByValue is the only accepted value of ManagedByLabel.
	ManagedByValue = "kubeswift-spin"

	// AnnRuntimeImage overrides the controller's default runtime rootfs image.
	AnnRuntimeImage = Prefix + "runtime-image"
	// AnnRuntimeImagePullSecret names a docker-registry Secret that KubeSwift
	// uses to pull the runtime rootfs image. kubeswift-spin never reads it.
	AnnRuntimeImagePullSecret = Prefix + "runtime-image-pull-secret"
	// AnnRuntimeImageVerifyKey names a Secret holding a cosign public key that
	// KubeSwift uses to verify the runtime rootfs image before boot.
	AnnRuntimeImageVerifyKey = Prefix + "runtime-image-verify-key-secret"
	// AnnNetworkMode selects the SwiftSandbox network mode.
	AnnNetworkMode = Prefix + "network-mode"
	// AnnRootfsMode selects the SwiftSandbox rootfs delivery mode.
	AnnRootfsMode = Prefix + "rootfs-mode"
	// AnnKernelProfile names the SwiftKernel profile to boot.
	AnnKernelProfile = Prefix + "kernel-profile"
	// AnnSandboxPool names a SwiftSandboxPool used for warm checkout.
	AnnSandboxPool = Prefix + "sandbox-pool"
	// AnnNodeSelector is a comma-separated list of key=value node labels.
	AnnNodeSelector = Prefix + "node-selector"
	// AnnDefaultCPU is the CPU quantity used when a SpinApp sets none.
	AnnDefaultCPU = Prefix + "default-cpu"
	// AnnDefaultMemory is the memory quantity used when a SpinApp sets none.
	AnnDefaultMemory = Prefix + "default-memory"
)

// knownAnnotations is the complete set of profile annotations. Any other
// annotation under Prefix is rejected so a typo is reported instead of
// silently ignored.
var knownAnnotations = map[string]bool{
	AnnRuntimeImage:           true,
	AnnRuntimeImagePullSecret: true,
	AnnRuntimeImageVerifyKey:  true,
	AnnNetworkMode:            true,
	AnnRootfsMode:             true,
	AnnKernelProfile:          true,
	AnnSandboxPool:            true,
	AnnNodeSelector:           true,
	AnnDefaultCPU:             true,
	AnnDefaultMemory:          true,
}

// KnownAnnotations returns the supported profile annotation keys, sorted.
func KnownAnnotations() []string {
	keys := make([]string, 0, len(knownAnnotations))
	for k := range knownAnnotations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Defaults are controller-wide values used when an executor does not override
// them.
type Defaults struct {
	RuntimeImage  string
	DefaultCPU    resource.Quantity
	DefaultMemory resource.Quantity
}

// Profile is the validated execution profile of one SpinAppExecutor.
type Profile struct {
	ExecutorName string

	RuntimeImage           string
	RuntimeImagePullSecret string
	RuntimeImageVerifyKey  string

	NetworkMode   sandboxv1alpha1.SandboxNetworkMode
	RootfsMode    sandboxv1alpha1.SandboxRootfsMode // empty means the KubeSwift default
	KernelProfile string
	SandboxPool   string
	NodeSelector  map[string]string

	DefaultCPU    resource.Quantity
	DefaultMemory resource.Quantity

	// Otel is copied from spec.deploymentConfig.otel. Spin reads the standard
	// OTEL_EXPORTER_OTLP_* variables.
	Otel *spinv1alpha1.OtelConfig
}

// IsManaged reports whether kubeswift-spin owns the executor. Ownership is the
// explicit label, never the executor name.
func IsManaged(e *spinv1alpha1.SpinAppExecutor) bool {
	return e != nil && e.Labels[ManagedByLabel] == ManagedByValue
}

// ValidationError lists every problem found on an executor. Messages name
// fields and annotation keys, never Secret contents.
type ValidationError struct {
	Executor string
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("SpinAppExecutor %q is not a valid kubeswift-spin executor: %s",
		e.Executor, strings.Join(e.Problems, "; "))
}

// Parse validates a managed executor and returns its profile. It returns a
// *ValidationError describing all problems when the executor cannot be used.
func Parse(e *spinv1alpha1.SpinAppExecutor, d Defaults) (*Profile, error) {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	p := &Profile{
		ExecutorName:  e.Name,
		RuntimeImage:  d.RuntimeImage,
		NetworkMode:   sandboxv1alpha1.SandboxNetworkRestricted,
		DefaultCPU:    d.DefaultCPU,
		DefaultMemory: d.DefaultMemory,
	}

	if e.Spec.CreateDeployment {
		add("spec.createDeployment must be false: Spin Operator would also create a Deployment for every SpinApp")
	}
	if dc := e.Spec.DeploymentConfig; dc != nil {
		if dc.RuntimeClassName != nil {
			add("spec.deploymentConfig.runtimeClassName is not applicable: SpinApps run in KubeSwift sandboxes, not pods")
		}
		if dc.SpinImage != nil {
			add("spec.deploymentConfig.spinImage is not applicable: use the %s annotation to select the runtime rootfs image", AnnRuntimeImage)
		}
		if dc.CACertSecret != "" {
			add("spec.deploymentConfig.caCertSecret is not supported: KubeSwift SwiftSandbox has no secure file or secret projection (see docs/upstream/kubeswift-sandbox-secret-projection.md)")
		}
		// installDefaultCACerts needs no action: the runtime image always
		// ships a CA bundle. See docs/executor-contract.md.
		p.Otel = dc.Otel.DeepCopy()
	}

	for k := range e.Annotations {
		if strings.HasPrefix(k, Prefix) && !knownAnnotations[k] {
			add("unknown annotation %q (supported: %s)", k, strings.Join(KnownAnnotations(), ", "))
		}
	}
	ann := e.Annotations

	if v, ok := ann[AnnRuntimeImage]; ok {
		p.RuntimeImage = v
	}
	if err := ValidateRuntimeImage(p.RuntimeImage); err != nil {
		add("runtime image: %v", err)
	}

	for key, dst := range map[string]*string{
		AnnRuntimeImagePullSecret: &p.RuntimeImagePullSecret,
		AnnRuntimeImageVerifyKey:  &p.RuntimeImageVerifyKey,
		AnnKernelProfile:          &p.KernelProfile,
		AnnSandboxPool:            &p.SandboxPool,
	} {
		v, ok := ann[key]
		if !ok {
			continue
		}
		if errs := validation.IsDNS1123Subdomain(v); len(errs) > 0 {
			add("annotation %s must be a valid object name: %s", key, strings.Join(errs, ", "))
			continue
		}
		*dst = v
	}

	if v, ok := ann[AnnNetworkMode]; ok {
		switch sandboxv1alpha1.SandboxNetworkMode(v) {
		case sandboxv1alpha1.SandboxNetworkRestricted, sandboxv1alpha1.SandboxNetworkOpen:
			p.NetworkMode = sandboxv1alpha1.SandboxNetworkMode(v)
		case sandboxv1alpha1.SandboxNetworkNone:
			add("annotation %s=none is not supported: Spin pulls the application artifact over the network when it starts (see docs/upstream/kubeswift-sandbox-artifact-projection.md)", AnnNetworkMode)
		default:
			add("annotation %s must be restricted or open, got %q", AnnNetworkMode, v)
		}
	}

	if v, ok := ann[AnnRootfsMode]; ok {
		switch sandboxv1alpha1.SandboxRootfsMode(v) {
		case sandboxv1alpha1.SandboxRootfsBlock, sandboxv1alpha1.SandboxRootfsVirtiofs:
			p.RootfsMode = sandboxv1alpha1.SandboxRootfsMode(v)
		default:
			add("annotation %s must be block or virtiofs, got %q", AnnRootfsMode, v)
		}
	}

	if v, ok := ann[AnnNodeSelector]; ok {
		sel, err := parseNodeSelector(v)
		if err != nil {
			add("annotation %s: %v", AnnNodeSelector, err)
		} else {
			p.NodeSelector = sel
		}
	}

	for key, dst := range map[string]*resource.Quantity{
		AnnDefaultCPU:    &p.DefaultCPU,
		AnnDefaultMemory: &p.DefaultMemory,
	} {
		v, ok := ann[key]
		if !ok {
			continue
		}
		q, err := resource.ParseQuantity(v)
		if err != nil || q.Sign() <= 0 {
			add("annotation %s must be a positive quantity, got %q", key, v)
			continue
		}
		*dst = q
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, &ValidationError{Executor: e.Name, Problems: problems}
	}
	return p, nil
}

// ValidateRuntimeImage requires a fully qualified reference with an explicit
// registry and an explicit tag or digest. A runtime image that silently
// resolves to ":latest" would make sandbox contents depend on push order.
func ValidateRuntimeImage(ref string) error {
	if ref == "" {
		return fmt.Errorf("no runtime image configured (set the controller --runtime-image flag or the %s annotation)", AnnRuntimeImage)
	}
	if _, err := name.ParseReference(ref, name.StrictValidation); err != nil {
		return fmt.Errorf("%q must include a registry and an explicit tag or digest: %v", ref, err)
	}
	return nil
}

func parseNodeSelector(v string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, val, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("entry %q is not key=value", pair)
		}
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return nil, fmt.Errorf("invalid label key %q: %s", k, strings.Join(errs, ", "))
		}
		if errs := validation.IsValidLabelValue(val); len(errs) > 0 {
			return nil, fmt.Errorf("invalid label value for %q: %s", k, strings.Join(errs, ", "))
		}
		out[k] = val
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no key=value entries")
	}
	return out, nil
}
