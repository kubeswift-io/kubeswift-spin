package translate

import (
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// PoolMismatches lists every way a SwiftSandboxPool's slot shape differs from
// the sandbox spec that would claim a slot.
//
// KubeSwift v0.15.1 checkout only compares image, network mode and the
// verification key, and falls back to a cold boot on a mismatch. It does not
// compare CPU, memory, rootfs mode, kernel or node selector, so a claimed
// slot could silently run with a different shape. kubeswift-spin therefore
// checks the full shape itself and refuses to use an incompatible pool.
func PoolMismatches(spec *sandboxv1alpha1.SwiftSandboxSpec, pool *sandboxv1alpha1.SwiftSandboxPool) []string {
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
	if refName(ps.KernelProfileRef) != refName(spec.KernelProfileRef) {
		add("kernelProfileRef", refName(spec.KernelProfileRef), refName(ps.KernelProfileRef))
	}
	if secretName(ps.VerifyKeySecretRef) != secretName(spec.VerifyKeySecretRef) {
		add("verifyKeySecretRef", secretName(spec.VerifyKeySecretRef), secretName(ps.VerifyKeySecretRef))
	}
	if !maps.Equal(ps.NodeSelector, spec.NodeSelector) {
		add("nodeSelector", fmt.Sprint(spec.NodeSelector), fmt.Sprint(ps.NodeSelector))
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

func refName(r *corev1.LocalObjectReference) string {
	if r == nil {
		return ""
	}
	return r.Name
}
