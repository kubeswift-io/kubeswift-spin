package sandboxapi_test

// Contract test: kubeswift-spin declares its own subset of the KubeSwift
// sandbox API (so its binaries do not link KubeSwift's AGPL module). This
// test, which is never part of a distributed binary, compares that subset
// with the pinned upstream Go types and CRD schema.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	upstream "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"

	ours "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

// sharedTypes must be the identical upstream Kubernetes type on both sides.
var sharedTypes = map[reflect.Type]bool{
	reflect.TypeOf(resource.Quantity{}):              true,
	reflect.TypeOf(corev1.EnvVar{}):                  true,
	reflect.TypeOf(corev1.LocalObjectReference{}):    true,
	reflect.TypeOf(metav1.Condition{}):               true,
	reflect.TypeOf(metav1.ObjectMeta{}):              true,
	reflect.TypeOf(metav1.TypeMeta{}):                true,
	reflect.TypeOf(metav1.ListMeta{}):                true,
	reflect.TypeOf(corev1.Probe{}):                   true,
	reflect.TypeOf(networkingv1.NetworkPolicyPeer{}): true,
}

func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if f.Anonymous && strings.HasPrefix(tag, ",inline") {
		return ""
	}
	return strings.Split(tag, ",")[0]
}

func fieldsByJSON(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		out[jsonName(f)] = f
	}
	return out
}

// compare checks that every field of ours exists in up with the same JSON
// name, tag options and a compatible type, recursively.
func compare(t *testing.T, path string, ourT, upT reflect.Type) {
	t.Helper()
	if sharedTypes[ourT] || sharedTypes[upT] {
		if ourT != upT {
			t.Errorf("%s: type %s, upstream %s", path, ourT, upT)
		}
		return
	}
	if ourT.Kind() != upT.Kind() {
		t.Errorf("%s: kind %s, upstream %s", path, ourT.Kind(), upT.Kind())
		return
	}
	switch ourT.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map:
		if ourT.Kind() == reflect.Map && ourT.Key() != upT.Key() {
			t.Errorf("%s: map key %s, upstream %s", path, ourT.Key(), upT.Key())
		}
		compare(t, path+"[]", ourT.Elem(), upT.Elem())
	case reflect.Struct:
		up := fieldsByJSON(upT)
		for name, f := range fieldsByJSON(ourT) {
			uf, ok := up[name]
			if !ok {
				t.Errorf("%s.%s: not in upstream %s", path, name, upT.Name())
				continue
			}
			if f.Tag.Get("json") != uf.Tag.Get("json") {
				t.Errorf("%s.%s: json tag %q, upstream %q", path, name, f.Tag.Get("json"), uf.Tag.Get("json"))
			}
			compare(t, path+"."+name, f.Type, uf.Type)
		}
	}
}

func TestTypesMatchUpstream(t *testing.T) {
	pairs := []struct {
		name     string
		ours, up any
	}{
		{"SwiftSandbox", ours.SwiftSandbox{}, upstream.SwiftSandbox{}},
		{"SwiftSandboxList", ours.SwiftSandboxList{}, upstream.SwiftSandboxList{}},
		{"SwiftSandboxPool", ours.SwiftSandboxPool{}, upstream.SwiftSandboxPool{}},
		{"SwiftSandboxPoolList", ours.SwiftSandboxPoolList{}, upstream.SwiftSandboxPoolList{}},
	}
	for _, p := range pairs {
		compare(t, p.name, reflect.TypeOf(p.ours), reflect.TypeOf(p.up))
	}
}

func TestConstantsMatchUpstream(t *testing.T) {
	eq := func(name string, a, b string) {
		if a != b {
			t.Errorf("%s = %q, upstream %q", name, a, b)
		}
	}
	eq("group", ours.GroupVersion.Group, upstream.GroupName)
	eq("version", ours.GroupVersion.Version, upstream.Version)
	eq("network restricted", string(ours.SandboxNetworkRestricted), string(upstream.SandboxNetworkRestricted))
	eq("network open", string(ours.SandboxNetworkOpen), string(upstream.SandboxNetworkOpen))
	eq("network none", string(ours.SandboxNetworkNone), string(upstream.SandboxNetworkNone))
	eq("rootfs block", string(ours.SandboxRootfsBlock), string(upstream.SandboxRootfsBlock))
	eq("rootfs virtiofs", string(ours.SandboxRootfsVirtiofs), string(upstream.SandboxRootfsVirtiofs))
	eq("phase Pending", string(ours.SwiftSandboxPending), string(upstream.SwiftSandboxPending))
	eq("phase Materializing", string(ours.SwiftSandboxMaterializing), string(upstream.SwiftSandboxMaterializing))
	eq("phase Running", string(ours.SwiftSandboxRunning), string(upstream.SwiftSandboxRunning))
	eq("phase Completed", string(ours.SwiftSandboxCompleted), string(upstream.SwiftSandboxCompleted))
	eq("phase Failed", string(ours.SwiftSandboxFailed), string(upstream.SwiftSandboxFailed))
	eq("condition Resolved", ours.SwiftSandboxConditionResolved, upstream.SwiftSandboxConditionResolved)
	eq("condition GuestRunning", ours.SwiftSandboxConditionGuestRunning, upstream.SwiftSandboxConditionGuestRunning)
	eq("condition WorkloadReady", ours.SwiftSandboxConditionWorkloadReady, upstream.SwiftSandboxConditionWorkloadReady)
}

// TestRoundTrip encodes a fully populated sandbox with our types and decodes
// it with the upstream types: nothing may be lost or renamed.
func TestRoundTrip(t *testing.T) {
	sb := ours.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hello-0", Namespace: "apps"},
		Spec: ours.SwiftSandboxSpec{
			Image:              "ghcr.io/kubeswift-io/kubeswift-spin-runtime:spin-4.2.1-r1",
			ImagePullSecret:    "pull",
			VerifyKeySecretRef: &ours.SecretObjectReference{Name: "cosign"},
			CPU:                2,
			Memory:             resource.MustParse("954Mi"),
			Command:            []string{"/usr/local/bin/kubeswift-spin-entrypoint"},
			Args:               []string{"up", "--from=ghcr.io/x/app:v1"},
			Env:                []corev1.EnvVar{{Name: "SPIN_VARIABLE_X", Value: "y"}},
			Network: ours.SandboxNetwork{
				Mode:    ours.SandboxNetworkRestricted,
				Ports:   []ours.SandboxPort{{Name: "http-app", Port: 3000, Protocol: corev1.ProtocolTCP}},
				Ingress: &ours.SandboxIngress{From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}}},
				Egress: &ours.SandboxEgress{Allow: []ours.SandboxEgressRule{
					{Service: &ours.SandboxEgressService{Name: "llm", Namespace: "inference"}, Ports: []ours.SandboxEgressPort{{Port: 8000}}},
					{CIDR: "10.20.0.0/24"},
				}},
			},
			RootfsMode:       ours.SandboxRootfsVirtiofs,
			KernelProfileRef: &corev1.LocalObjectReference{Name: "sandbox"},
			NodeSelector:     map[string]string{"zone": "a"},
			PoolRef:          &corev1.LocalObjectReference{Name: "warm"},
			SecretFiles:      []ours.SandboxSecretFile{{SecretName: "s", Items: []ours.SandboxSecretFileItem{{Key: "k", Path: "/run/x", Mode: ptr(0o400)}}}},
			PodMetadata:      &ours.SandboxPodMetadata{Labels: map[string]string{"a": "b"}, Annotations: map[string]string{"c": "d"}},
			ReadinessProbe:   &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("http-app")}}},
			LivenessProbe:    &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http-app")}}},
		},
		Status: ours.SwiftSandboxStatus{Phase: ours.SwiftSandboxRunning, Message: "m",
			Conditions: []metav1.Condition{{Type: "GuestRunning", Status: metav1.ConditionTrue, Reason: "GuestRunning"}}},
	}
	b, err := json.Marshal(sb)
	if err != nil {
		t.Fatal(err)
	}
	var up upstream.SwiftSandbox
	if err := json.Unmarshal(b, &up); err != nil {
		t.Fatal(err)
	}
	upBytes, _ := json.Marshal(up)
	var back ours.SwiftSandbox
	if err := json.Unmarshal(upBytes, &back); err != nil {
		t.Fatal(err)
	}
	ob, _ := json.Marshal(back)
	if string(ob) != string(b) {
		t.Fatalf("round trip through upstream types changed the object:\n ours: %s\n back: %s", b, ob)
	}
}

func moduleDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/kubeswift-io/kubeswift").Output()
	if err != nil {
		t.Fatalf("locate KubeSwift module: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestRequiredFieldsDeclared checks that every field the CRD schema marks as
// required in spec is declared, since kubeswift-spin creates sandboxes.
func TestRequiredFieldsDeclared(t *testing.T) {
	for file, typ := range map[string]reflect.Type{
		"sandbox.kubeswift.io_swiftsandboxes.yaml":    reflect.TypeOf(ours.SwiftSandboxSpec{}),
		"sandbox.kubeswift.io_swiftsandboxpools.yaml": reflect.TypeOf(ours.SwiftSandboxPoolSpec{}),
	} {
		raw, err := os.ReadFile(filepath.Join(moduleDir(t), "config", "crd", "bases", file))
		if err != nil {
			t.Fatal(err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatal(err)
		}
		var spec apiextensionsv1.JSONSchemaProps
		for _, v := range crd.Spec.Versions {
			if v.Name == ours.GroupVersion.Version {
				spec = v.Schema.OpenAPIV3Schema.Properties["spec"]
			}
		}
		declared := fieldsByJSON(typ)
		for _, req := range spec.Required {
			if _, ok := declared[req]; !ok {
				t.Errorf("%s: required field spec.%s is not declared in %s", file, req, typ.Name())
			}
		}
		for name := range declared {
			if _, ok := spec.Properties[name]; !ok {
				t.Errorf("%s: declared field spec.%s is not in the CRD schema", file, name)
			}
		}
	}
}

func ptr(v int32) *int32 { return &v }
