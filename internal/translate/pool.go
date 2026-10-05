package translate

import (
	"fmt"
	"maps"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

// PoolMismatches lists every way a SwiftSandboxPool's slot shape differs from
// the sandbox spec that would claim a slot.
//
// KubeSwift v0.16.0 compares the full shape at checkout and boots cold on a
// mismatch; v0.15.1 compared only image, network mode and verification key
// and could hand out a slot of a different shape. kubeswift-spin checks the
// shape itself on every version and refuses an incompatible pool instead of
// falling back silently (ADR 0007).
func PoolMismatches(spec *sandboxv1alpha1.SwiftSandboxSpec, namespace string, pool *sandboxv1alpha1.SwiftSandboxPool) []string {
	ps := &pool.Spec
	var out []string
	add := func(field, want, got string) {
		out = append(out, fmt.Sprintf("%s: sandbox needs %q, pool has %q", field, want, got))
	}
	if ps.Image != spec.Image {
		add("image", spec.Image, ps.Image)
	}
	if orDefaultCPU(ps.CPU) != orDefaultCPU(spec.CPU) {
		add("cpu", fmt.Sprint(orDefaultCPU(spec.CPU)), fmt.Sprint(orDefaultCPU(ps.CPU)))
	}
	if ps.Memory.Cmp(spec.Memory) != 0 {
		add("memory", spec.Memory.String(), ps.Memory.String())
	}
	if orDefaultNet(ps.Network.Mode) != orDefaultNet(spec.Network.Mode) {
		add("network.mode", string(orDefaultNet(spec.Network.Mode)), string(orDefaultNet(ps.Network.Mode)))
	}
	if orDefaultRootfs(ps.RootfsMode) != orDefaultRootfs(spec.RootfsMode) {
		add("rootfsMode", string(orDefaultRootfs(spec.RootfsMode)), string(orDefaultRootfs(ps.RootfsMode)))
	}
	if kernelName(ps.KernelProfileRef) != kernelName(spec.KernelProfileRef) {
		add("kernelProfileRef", kernelName(spec.KernelProfileRef), kernelName(ps.KernelProfileRef))
	}
	if secretName(ps.VerifyKeySecretRef) != secretName(spec.VerifyKeySecretRef) {
		add("verifyKeySecretRef", secretName(spec.VerifyKeySecretRef), secretName(ps.VerifyKeySecretRef))
	}
	if !maps.Equal(ps.NodeSelector, spec.NodeSelector) {
		add("nodeSelector", fmt.Sprint(spec.NodeSelector), fmt.Sprint(ps.NodeSelector))
	}
	// KubeSwift v0.16.0 compares exposed ports and the egress allowlist at
	// checkout too: a slot booted without them cannot be given them.
	if a, b := portsKey(spec.Network.Ports), portsKey(ps.Network.Ports); a != b {
		add("network.ports", a, b)
	}
	if a, b := egressKey(spec.Network.Egress, namespace), egressKey(ps.Network.Egress, pool.Namespace); a != b {
		add("network.egress", a, b)
	}
	if ps.GPUProfileRef != nil {
		out = append(out, "gpuProfileRef: pool slots hold a GPU, which a Spin sandbox does not use")
	}
	if ps.Model != nil {
		out = append(out, "model: pool slots mount a model artifact, which a Spin sandbox does not use")
	}
	return out
}

func orDefaultCPU(c int32) int32 {
	if c == 0 {
		return 1
	}
	return c
}

func orDefaultNet(m sandboxv1alpha1.SandboxNetworkMode) sandboxv1alpha1.SandboxNetworkMode {
	if m == "" {
		return sandboxv1alpha1.SandboxNetworkRestricted
	}
	return m
}

func orDefaultRootfs(m sandboxv1alpha1.SandboxRootfsMode) sandboxv1alpha1.SandboxRootfsMode {
	if m == "" {
		return sandboxv1alpha1.SandboxRootfsBlock
	}
	return m
}

func secretName(r *sandboxv1alpha1.SecretObjectReference) string {
	if r == nil {
		return ""
	}
	return r.Name
}

// defaultKernelProfile is the SwiftKernel KubeSwift boots when
// kernelProfileRef is unset (KubeSwift v0.15.1).
const defaultKernelProfile = "sandbox"

func kernelName(r *corev1.LocalObjectReference) string {
	if n := refName(r); n != "" {
		return n
	}
	return defaultKernelProfile
}

func refName(r *corev1.LocalObjectReference) string {
	if r == nil {
		return ""
	}
	return r.Name
}

func portsKey(ports []sandboxv1alpha1.SandboxPort) string {
	var out []string
	for _, p := range ports {
		proto := string(p.Protocol)
		if proto == "" {
			proto = "TCP"
		}
		out = append(out, fmt.Sprintf("%s=%d/%s", p.Name, p.Port, proto))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// egressKey normalizes an egress allowlist; a Service without a namespace is
// in the namespace of the object that declares it.
func egressKey(e *sandboxv1alpha1.SandboxEgress, namespace string) string {
	if e == nil {
		return ""
	}
	var out []string
	for _, r := range e.Allow {
		var ports []string
		for _, p := range r.Ports {
			proto := string(p.Protocol)
			if proto == "" {
				proto = "TCP"
			}
			ports = append(ports, fmt.Sprintf("%d/%s", p.Port, proto))
		}
		sort.Strings(ports)
		dest := "cidr " + r.CIDR
		if r.Service != nil {
			ns := r.Service.Namespace
			if ns == "" {
				ns = namespace
			}
			dest = "service " + ns + "/" + r.Service.Name
		}
		out = append(out, dest+" "+strings.Join(ports, ","))
	}
	sort.Strings(out)
	return strings.Join(out, "; ")
}
