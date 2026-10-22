package compiledcache

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

func compilerFixture(t *testing.T) (*cachev1alpha1.SpinCompiledCache, *sandboxv1alpha1.SwiftSandbox) {
	t.Helper()
	rt := settings.Runtimes[0]
	rec := &cachev1alpha1.SpinCompiledCache{}
	rec.Name, rec.Namespace = "spincc-1", "kubeswift-spin-system"
	rec.Spec = cachev1alpha1.SpinCompiledCacheSpec{Namespace: "team-a", Source: "ghcr.io/org/app@" + digestA, Runtime: rt,
		Architecture: "amd64", Repository: testRepo}
	a := app("ghcr.io/org/app@"+digestA, nil)
	a.Spec.Variables = []spinv1alpha1.SpinVar{
		{Name: "greeting", Value: "hi"},
		{Name: "token", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k"}}},
	}
	a.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "pull"}}
	p := profile(rt, true, map[string]string{"pool": "spin"})
	p.NetworkMode = sandboxv1alpha1.SandboxNetworkRestricted
	p.KernelProfile = "sandbox-6-6-17"
	p.RuntimeImagePullSecret = "rt-pull"
	sb := CompilerSandbox(rec, a, p, DefaultCompilerSettings(), "tok")
	sb.Name, sb.UID = "spin-compile-abc", "uid-1"
	h, err := SpecHash(toUnstructured(t, sb))
	if err != nil {
		t.Fatal(err)
	}
	rec.Status.Compile = &cachev1alpha1.CompileStatus{Attempt: 1, Token: "tok", Sandbox: &cachev1alpha1.SandboxReference{Name: sb.Name, UID: sb.UID}, SpecHash: h}
	return rec, sb
}

func TestCompilerSandbox(t *testing.T) {
	rec, sb := compilerFixture(t)
	s := sb.Spec
	if sb.Namespace != "team-a" || sb.GenerateName != "spin-compile-" || sb.Labels[LabelRecord] != rec.Name || sb.Labels[LabelToken] != "tok" {
		t.Errorf("metadata %+v", sb.ObjectMeta)
	}
	if s.Image != rec.Spec.Runtime || s.ImagePullSecret != "rt-pull" || s.KernelProfileRef == nil || s.KernelProfileRef.Name != "sandbox-6-6-17" {
		t.Errorf("image and kernel: %+v", s)
	}
	if strings.Join(s.Args, " ") != "compile --source="+rec.Spec.Source+" --runtime="+rec.Spec.Runtime+" --architecture=amd64" {
		t.Errorf("args %v", s.Args)
	}
	if s.NodeSelector[ArchitectureLabel] != "amd64" || s.NodeSelector["pool"] != "spin" {
		t.Errorf("node selector %v", s.NodeSelector)
	}
	// Never reachable and never selected by a Service.
	if len(s.Network.Ports) != 0 || s.PodMetadata != nil || s.ReadinessProbe != nil || s.PoolRef != nil {
		t.Errorf("a compiler must not be exposed or pooled: %+v", s)
	}
	// Literal variables and pull credentials only: no Secret-backed value.
	for _, e := range s.Env {
		if e.ValueFrom != nil {
			t.Errorf("secret-backed env %s", e.Name)
		}
	}
	if len(s.Env) != 2 || s.Env[0].Name != "SPIN_VARIABLE_GREETING" || s.Env[1].Name != runtimecontract.RegistryAuthFilesEnv {
		t.Errorf("env %+v", s.Env)
	}
	if len(s.SecretFiles) != 1 || s.SecretFiles[0].SecretName != "pull" {
		t.Errorf("secret files %+v", s.SecretFiles)
	}
	if len(s.Outputs) != 1 || s.Outputs[0].Path != runtimecontract.CompileOutputDir || s.Outputs[0].Repository != testRepo || s.Outputs[0].ArtifactType != ArtifactType {
		t.Errorf("outputs %+v", s.Outputs)
	}
	if s.Timeout == nil || s.TTL == nil {
		t.Error("a compiler must be time-bounded")
	}
}

func completed(sb *sandboxv1alpha1.SwiftSandbox, repo, digest string) {
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxCompleted
	sb.Status.Outputs = []sandboxv1alpha1.SandboxOutputStatus{{Name: OutputName, State: sandboxv1alpha1.SandboxOutputExported,
		Repository: repo, Reference: repo + "@" + digest, Digest: digest, Size: 1234}}
}

func TestExportedArtifact(t *testing.T) {
	rec, sb := compilerFixture(t)
	completed(sb, testRepo, digestB)
	ref, err := ExportedArtifact(rec, sb)
	if err != nil || ref != testRepo+"@"+digestB {
		t.Fatalf("%s %v", ref, err)
	}
	for name, mutate := range map[string]func(*cachev1alpha1.SpinCompiledCache, *sandboxv1alpha1.SwiftSandbox){
		"other uid":   func(_ *cachev1alpha1.SpinCompiledCache, s *sandboxv1alpha1.SwiftSandbox) { s.UID = "uid-2" },
		"other token": func(_ *cachev1alpha1.SpinCompiledCache, s *sandboxv1alpha1.SwiftSandbox) { s.Labels[LabelToken] = "x" },
		"other image": func(_ *cachev1alpha1.SpinCompiledCache, s *sandboxv1alpha1.SwiftSandbox) {
			s.Spec.Image = "ghcr.io/evil/rt@" + digestA
		},
		"other source": func(_ *cachev1alpha1.SpinCompiledCache, s *sandboxv1alpha1.SwiftSandbox) {
			s.Spec.Args[1] = "--source=ghcr.io/x@" + digestB
		},
		"other repository": func(_ *cachev1alpha1.SpinCompiledCache, s *sandboxv1alpha1.SwiftSandbox) {
			s.Spec.Outputs[0].Repository = "r/x"
		},
		"still running": func(_ *cachev1alpha1.SpinCompiledCache, s *sandboxv1alpha1.SwiftSandbox) {
			s.Status.Phase = sandboxv1alpha1.SwiftSandboxRunning
		},
		"not exported": func(_ *cachev1alpha1.SpinCompiledCache, s *sandboxv1alpha1.SwiftSandbox) {
			s.Status.Outputs[0].State = sandboxv1alpha1.SandboxOutputFailed
		},
		"exported elsewhere": func(_ *cachev1alpha1.SpinCompiledCache, s *sandboxv1alpha1.SwiftSandbox) {
			s.Status.Outputs[0].Repository = "registry.example.com/c/team-b/wasmtime-cache"
		},
	} {
		r2, s2 := rec.DeepCopy(), sb.DeepCopy()
		mutate(r2, s2)
		if _, err := ExportedArtifact(r2, s2); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func toUnstructured(t *testing.T, sb *sandboxv1alpha1.SwiftSandbox) *unstructured.Unstructured {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(sb)
	if err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: m}
}

func TestCheckCompiler(t *testing.T) {
	rec, sb := compilerFixture(t)
	if err := CheckCompiler(rec, toUnstructured(t, sb), settings.Runtimes); err != nil {
		t.Fatal(err)
	}
	// Labels are not evidence for the signer, and ttl may change.
	u := toUnstructured(t, sb)
	u.SetLabels(nil)
	if err := unstructured.SetNestedField(u.Object, "5m", "spec", "ttl"); err != nil {
		t.Fatal(err)
	}
	if err := CheckCompiler(rec, u, settings.Runtimes); err != nil {
		t.Errorf("labels and ttl must not matter: %v", err)
	}
	now := metav1.Now()
	for name, mutate := range map[string]func(u *unstructured.Unstructured){
		"other uid":     func(u *unstructured.Unstructured) { u.SetUID("uid-2") },
		"being deleted": func(u *unstructured.Unstructured) { u.SetDeletionTimestamp(&now) },
		"other cpu":     func(u *unstructured.Unstructured) { _ = unstructured.SetNestedField(u.Object, int64(8), "spec", "cpu") },
		// Fields kubeswift-spin's own types do not declare still change the
		// hash: an artifact mounted over the entrypoint is caught.
		"hidden artifact": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"name": "x", "ref": "evil@sha256:00", "mountPath": "/usr/local/bin"}}, "spec", "artifacts")
		},
	} {
		u := toUnstructured(t, sb)
		mutate(u)
		if err := CheckCompiler(rec, u, settings.Runtimes); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A spec the cache manager recorded, with fields a compiler never has,
	// is still refused: the check does not rely on the cache manager alone.
	for name, mutate := range map[string]func(*sandboxv1alpha1.SwiftSandbox){
		"secret env": func(sb *sandboxv1alpha1.SwiftSandbox) {
			sb.Spec.Env = append(sb.Spec.Env, corev1.EnvVar{Name: "SPIN_VARIABLE_X", ValueFrom: &corev1.EnvVarSource{}})
		},
		"other env": func(sb *sandboxv1alpha1.SwiftSandbox) {
			sb.Spec.Env = append(sb.Spec.Env, corev1.EnvVar{Name: "LD_PRELOAD", Value: "x"})
		},
		"exposed": func(sb *sandboxv1alpha1.SwiftSandbox) {
			sb.Spec.Network.Ports = []sandboxv1alpha1.SandboxPort{{Name: "p", Port: 1}}
		},
		"pooled":      func(sb *sandboxv1alpha1.SwiftSandbox) { sb.Spec.PoolRef = &corev1.LocalObjectReference{Name: "p"} },
		"other arch":  func(sb *sandboxv1alpha1.SwiftSandbox) { sb.Spec.NodeSelector[ArchitectureLabel] = "arm64" },
		"secret file": func(sb *sandboxv1alpha1.SwiftSandbox) { sb.Spec.SecretFiles[0].Items[0].Path = "/etc/shadow" },
		"probe":       func(sb *sandboxv1alpha1.SwiftSandbox) { sb.Spec.ReadinessProbe = &corev1.Probe{} },
	} {
		r2, s2 := rec.DeepCopy(), sb.DeepCopy()
		mutate(s2)
		u := toUnstructured(t, s2)
		h, err := SpecHash(u)
		if err != nil {
			t.Fatal(err)
		}
		r2.Status.Compile.SpecHash = h
		if err := CheckCompiler(r2, u, settings.Runtimes); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := CheckCompiler(rec, toUnstructured(t, sb), []string{"ghcr.io/other/rt@" + digestA}); err == nil {
		t.Error("a runtime that is no longer allowed was accepted")
	}
	r2 := rec.DeepCopy()
	r2.Status.Compile.SpecHash = ""
	if err := CheckCompiler(r2, toUnstructured(t, sb), settings.Runtimes); err == nil {
		t.Error("a record without a spec hash was accepted")
	}
	if ExportTag(sb) != "uid-1."+OutputName {
		t.Errorf("export tag %s", ExportTag(sb))
	}
	if !IsCompilerShaped(sb, settings.Runtimes) {
		t.Error("a compiler is compiler-shaped")
	}
	replica := sb.DeepCopy()
	replica.Spec.Args = []string{"up", "--from=x"}
	replica.Spec.Outputs = nil
	if IsCompilerShaped(replica, settings.Runtimes) {
		t.Error("a replica with compiler labels is not a compiler")
	}
}

func TestCompileFailure(t *testing.T) {
	sb := &sandboxv1alpha1.SwiftSandbox{}
	sb.Status.Outputs = []sandboxv1alpha1.SandboxOutputStatus{{Name: OutputName, State: sandboxv1alpha1.SandboxOutputFailed, Reason: "OutputDestinationDenied", Message: "no policy"}}
	if r, _ := CompileFailure(sb); r != "OutputDestinationDenied" {
		t.Errorf("reason %s", r)
	}
	code := int32(1)
	sb = &sandboxv1alpha1.SwiftSandbox{Status: sandboxv1alpha1.SwiftSandboxStatus{ExitCode: &code}}
	if r, _ := CompileFailure(sb); r != "CompilerExited" {
		t.Errorf("reason %s", r)
	}
	sb = &sandboxv1alpha1.SwiftSandbox{Status: sandboxv1alpha1.SwiftSandboxStatus{Conditions: []metav1.Condition{{Type: "GuestRunning", Status: metav1.ConditionFalse, Reason: "KernelUnsupported"}}}}
	if r, _ := CompileFailure(sb); r != "KernelUnsupported" {
		t.Errorf("reason %s", r)
	}
	if r, _ := CompileFailure(&sandboxv1alpha1.SwiftSandbox{}); r != "CompilerFailed" {
		t.Errorf("reason %s", r)
	}
}
