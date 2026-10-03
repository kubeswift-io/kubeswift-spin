package executor

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

const img = "ghcr.io/kubeswift-io/kubeswift-spin-runtime:v0.1.0"

func defaults() Defaults {
	return Defaults{RuntimeImage: img, DefaultCPU: resource.MustParse("1"), DefaultMemory: resource.MustParse("512Mi")}
}

func managed(name string, ann map[string]string) *spinv1alpha1.SpinAppExecutor {
	return &spinv1alpha1.SpinAppExecutor{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps",
			Labels: map[string]string{ManagedByLabel: ManagedByValue}, Annotations: ann},
		Spec: spinv1alpha1.SpinAppExecutorSpec{CreateDeployment: false},
	}
}

func TestIsManaged(t *testing.T) {
	if IsManaged(nil) {
		t.Fatal("nil executor is managed")
	}
	e := managed("anything", nil)
	if !IsManaged(e) {
		t.Fatal("labelled executor not managed")
	}
	// The executor name alone never implies ownership.
	byName := &spinv1alpha1.SpinAppExecutor{ObjectMeta: metav1.ObjectMeta{Name: "kubeswift"}}
	if IsManaged(byName) {
		t.Fatal("executor named kubeswift without the label is managed")
	}
	wrong := managed("x", nil)
	wrong.Labels[ManagedByLabel] = "someone-else"
	if IsManaged(wrong) {
		t.Fatal("foreign managed-by value accepted")
	}
}

func TestParseDefaults(t *testing.T) {
	p, err := Parse(managed("kubeswift", nil), defaults())
	if err != nil {
		t.Fatal(err)
	}
	if p.ExecutorName != "kubeswift" || p.RuntimeImage != img || p.NetworkMode != sandboxv1alpha1.SandboxNetworkRestricted ||
		p.RootfsMode != "" || p.SandboxPool != "" || p.KernelProfile != "" || p.NodeSelector != nil ||
		p.DefaultCPU.String() != "1" || p.DefaultMemory.String() != "512Mi" || p.Otel != nil {
		t.Fatalf("unexpected profile %+v", p)
	}
}

func TestParseProfiles(t *testing.T) {
	warm := managed("kubeswift-warm", map[string]string{
		AnnSandboxPool:            "spin-pool",
		AnnRootfsMode:             "virtiofs",
		AnnKernelProfile:          "sandbox",
		AnnNodeSelector:           "kubeswift.io/kernel-node=true, topology.kubernetes.io/zone=a",
		AnnDefaultCPU:             "2",
		AnnDefaultMemory:          "1Gi",
		AnnRuntimeImagePullSecret: "ghcr-pull",
		AnnRuntimeImageVerifyKey:  "cosign-pub",
		AnnRuntimeImage:           "registry.example.com/spin-runtime@sha256:" + strings.Repeat("a", 64),
		AnnNetworkMode:            "open",
		"unrelated.example.com/x": "ignored because it is not our prefix",
	})
	p, err := Parse(warm, defaults())
	if err != nil {
		t.Fatal(err)
	}
	if p.SandboxPool != "spin-pool" || p.RootfsMode != "virtiofs" || p.KernelProfile != "sandbox" ||
		p.NodeSelector["topology.kubernetes.io/zone"] != "a" || p.NodeSelector["kubeswift.io/kernel-node"] != "true" ||
		p.DefaultCPU.String() != "2" || p.DefaultMemory.String() != "1Gi" || p.RuntimeImagePullSecret != "ghcr-pull" ||
		p.RuntimeImageVerifyKey != "cosign-pub" || !strings.HasPrefix(p.RuntimeImage, "registry.example.com/") ||
		p.NetworkMode != "open" {
		t.Fatalf("profile not parsed: %+v", p)
	}
}

func TestParseOtel(t *testing.T) {
	e := managed("kubeswift", nil)
	e.Spec.DeploymentConfig = &spinv1alpha1.ExecutorDeploymentConfig{
		InstallDefaultCACerts: true,
		Otel:                  &spinv1alpha1.OtelConfig{ExporterOtlpEndpoint: "http://otel:4318"},
	}
	p, err := Parse(e, defaults())
	if err != nil {
		t.Fatal(err)
	}
	if p.Otel == nil || p.Otel.ExporterOtlpEndpoint != "http://otel:4318" {
		t.Fatalf("otel not copied: %+v", p.Otel)
	}
	e.Spec.DeploymentConfig.Otel.ExporterOtlpEndpoint = "changed"
	if p.Otel.ExporterOtlpEndpoint != "http://otel:4318" {
		t.Fatal("otel aliases the executor")
	}
}

func TestParseRejects(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := map[string]struct {
		mutate func(e *spinv1alpha1.SpinAppExecutor)
		want   string
	}{
		"createDeployment true": {func(e *spinv1alpha1.SpinAppExecutor) { e.Spec.CreateDeployment = true }, "createDeployment must be false"},
		"runtimeClassName": {func(e *spinv1alpha1.SpinAppExecutor) {
			e.Spec.DeploymentConfig = &spinv1alpha1.ExecutorDeploymentConfig{RuntimeClassName: str("wasmtime-spin-v2")}
		}, "runtimeClassName is not applicable"},
		"spinImage": {func(e *spinv1alpha1.SpinAppExecutor) {
			e.Spec.DeploymentConfig = &spinv1alpha1.ExecutorDeploymentConfig{SpinImage: str("ghcr.io/x/spin:v1")}
		}, "spinImage is not applicable"},
		"caCertSecret": {func(e *spinv1alpha1.SpinAppExecutor) {
			e.Spec.DeploymentConfig = &spinv1alpha1.ExecutorDeploymentConfig{CACertSecret: "my-ca"}
		}, "caCertSecret is not supported"},
		"unknown annotation": {func(e *spinv1alpha1.SpinAppExecutor) {
			e.Annotations = map[string]string{Prefix + "network": "open"}
		}, "unknown annotation"},
		"network none":        {func(e *spinv1alpha1.SpinAppExecutor) { e.Annotations = map[string]string{AnnNetworkMode: "none"} }, "network-mode=none is not supported"},
		"network bogus":       {func(e *spinv1alpha1.SpinAppExecutor) { e.Annotations = map[string]string{AnnNetworkMode: "public"} }, "must be restricted or open"},
		"rootfs bogus":        {func(e *spinv1alpha1.SpinAppExecutor) { e.Annotations = map[string]string{AnnRootfsMode: "nfs"} }, "must be block or virtiofs"},
		"pool name":           {func(e *spinv1alpha1.SpinAppExecutor) { e.Annotations = map[string]string{AnnSandboxPool: "Not_A_Name"} }, "valid object name"},
		"node selector":       {func(e *spinv1alpha1.SpinAppExecutor) { e.Annotations = map[string]string{AnnNodeSelector: "novalue"} }, "not key=value"},
		"empty node selector": {func(e *spinv1alpha1.SpinAppExecutor) { e.Annotations = map[string]string{AnnNodeSelector: " , "} }, "no key=value entries"},
		"default cpu":         {func(e *spinv1alpha1.SpinAppExecutor) { e.Annotations = map[string]string{AnnDefaultCPU: "0"} }, "positive quantity"},
		"default memory":      {func(e *spinv1alpha1.SpinAppExecutor) { e.Annotations = map[string]string{AnnDefaultMemory: "lots"} }, "positive quantity"},
		"untagged runtime image": {func(e *spinv1alpha1.SpinAppExecutor) {
			e.Annotations = map[string]string{AnnRuntimeImage: "ghcr.io/kubeswift-io/kubeswift-spin-runtime"}
		}, "explicit tag or digest"},
		"registryless runtime image": {func(e *spinv1alpha1.SpinAppExecutor) {
			e.Annotations = map[string]string{AnnRuntimeImage: "spin-runtime:v1"}
		}, "include a registry"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := managed("kubeswift", nil)
			tc.mutate(e)
			_, err := Parse(e, defaults())
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want ValidationError, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestParseReportsAllProblems(t *testing.T) {
	e := managed("kubeswift", map[string]string{AnnNetworkMode: "none", AnnRootfsMode: "nfs"})
	e.Spec.CreateDeployment = true
	_, err := Parse(e, defaults())
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 3 {
		t.Fatalf("want 3 problems, got %v", err)
	}
}

func TestParseRequiresRuntimeImage(t *testing.T) {
	d := defaults()
	d.RuntimeImage = ""
	if _, err := Parse(managed("kubeswift", nil), d); err == nil || !strings.Contains(err.Error(), "no runtime image configured") {
		t.Fatalf("got %v", err)
	}
}
