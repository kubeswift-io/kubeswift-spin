package capabilities

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
)

func fakeDiscovery(resources ...*metav1.APIResourceList) *fakediscovery.FakeDiscovery {
	return &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}
}

var (
	spinkube = &metav1.APIResourceList{GroupVersion: "core.spinkube.dev/v1alpha1", APIResources: []metav1.APIResource{
		{Name: "spinapps"}, {Name: "spinapps/status"}, {Name: "spinappexecutors"}}}
	sandbox = &metav1.APIResourceList{GroupVersion: "sandbox.kubeswift.io/v1alpha1", APIResources: []metav1.APIResource{
		{Name: "swiftsandboxes"}, {Name: "swiftsandboxpools"}}}
)

func TestCheckRequired(t *testing.T) {
	if err := CheckRequired(fakeDiscovery(spinkube, sandbox)); err != nil {
		t.Fatalf("all present: %v", err)
	}

	err := CheckRequired(fakeDiscovery(spinkube))
	if err == nil || !strings.Contains(err.Error(), "sandbox.kubeswift.io/v1alpha1") || !strings.Contains(err.Error(), "install KubeSwift") {
		t.Fatalf("missing KubeSwift: %v", err)
	}

	err = CheckRequired(fakeDiscovery(sandbox))
	if err == nil || !strings.Contains(err.Error(), "Spin Operator") {
		t.Fatalf("missing SpinKube: %v", err)
	}

	partial := &metav1.APIResourceList{GroupVersion: "core.spinkube.dev/v1alpha1", APIResources: []metav1.APIResource{{Name: "spinapps"}}}
	err = CheckRequired(fakeDiscovery(partial, sandbox))
	if err == nil || !strings.Contains(err.Error(), "spinappexecutors") || !strings.Contains(err.Error(), "spinapps/status") {
		t.Fatalf("missing executor resource: %v", err)
	}
}

func TestServed(t *testing.T) {
	ok, err := Served(fakeDiscovery(spinkube, sandbox), Pools)
	if err != nil || !ok {
		t.Fatalf("pools served: %v %v", ok, err)
	}
	noPools := &metav1.APIResourceList{GroupVersion: "sandbox.kubeswift.io/v1alpha1", APIResources: []metav1.APIResource{{Name: "swiftsandboxes"}}}
	ok, err = Served(fakeDiscovery(spinkube, noPools), Pools)
	if err != nil || ok {
		t.Fatalf("pools not served: %v %v", ok, err)
	}
}

const v0151Schema = `{"components":{"schemas":{"io.kubeswift.sandbox.v1alpha1.SwiftSandbox":{"properties":{"spec":{"properties":{
  "image":{"type":"string"},"network":{"properties":{"mode":{"type":"string"}}}}}}}}}}`

const proposedSchema = `{"components":{"schemas":{"io.kubeswift.sandbox.v1alpha1.SwiftSandbox":{"properties":{"spec":{"properties":{
  "network":{"properties":{"mode":{"type":"string"},"ports":{"type":"array"},"egress":{"type":"object"}}},
  "readinessProbe":{"type":"object"},"podMetadata":{"type":"object"},"secretFiles":{"type":"array"}}}}}}}}`

const v0162Schema = `{"components":{"schemas":{"io.kubeswift.sandbox.v1alpha1.SwiftSandbox":{"properties":{"spec":{"properties":{
  "network":{"properties":{"mode":{"type":"string"},"ports":{"type":"array"},"egress":{"type":"object"}}},
  "readinessProbe":{"type":"object"},"readinessStartup":{"type":"object"},"podMetadata":{"type":"object"},"secretFiles":{"type":"array"}}}}}}}}`

const v0163Schema = `{"components":{"schemas":{"io.kubeswift.sandbox.v1alpha1.SwiftSandbox":{"properties":{"spec":{"properties":{
  "network":{"properties":{"mode":{"type":"string"},"ports":{"type":"array"},"egress":{"type":"object"}}},
  "readinessProbe":{"type":"object"},"readinessStartup":{"type":"object"},"termination":{"type":"object"},"podMetadata":{"type":"object"},"secretFiles":{"type":"array"}}}}}}}}`

// v0191Schema has the trusted artifact (v0.18) and output (v0.19) fields.
const v0191Schema = `{"components":{"schemas":{"io.kubeswift.sandbox.v1alpha1.SwiftSandbox":{"properties":{"spec":{"properties":{
  "network":{"properties":{"mode":{"type":"string"},"ports":{"type":"array"},"egress":{"type":"object"}}},
  "readinessProbe":{"type":"object"},"readinessStartup":{"type":"object"},"termination":{"type":"object"},"podMetadata":{"type":"object"},"secretFiles":{"type":"array"},
  "artifacts":{"type":"array","items":{"properties":{"name":{},"ref":{},"mountPath":{},"layout":{},"optional":{},"fetchTimeout":{},"claims":{},"requirePlatformVerification":{}}}},
  "outputs":{"type":"array","items":{"properties":{"name":{},"path":{},"repository":{},"maxSize":{},"maxEntries":{},"artifactType":{},"annotations":{}}}}}}}}}}}`

// v0164Schema has artifacts without the trusted fields and no outputs.
const v0164Schema = `{"components":{"schemas":{"io.kubeswift.sandbox.v1alpha1.SwiftSandbox":{"properties":{"spec":{"properties":{
  "artifacts":{"type":"array","items":{"properties":{"name":{},"ref":{},"mountPath":{},"layout":{},"pullSecretRef":{},"verifyKeySecretRef":{}}}}}}}}}}}`

func TestParseSandboxSchemaCompiledCache(t *testing.T) {
	s, err := ParseSandboxSchema([]byte(v0191Schema))
	if err != nil {
		t.Fatal(err)
	}
	if !s.TrustedArtifacts || !s.Outputs || !s.CompiledCache() {
		// v0.19.1 to v0.19.3 publish the same schema: the fixes compiled
		// caches need from v0.19.3 are behavior, not fields.
		t.Fatalf("v0.19.1 schema not detected: %+v", s)
	}
	for name, doc := range map[string]string{"v0.16.3": v0163Schema, "v0.16.4": v0164Schema} {
		s, err := ParseSandboxSchema([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		if s.TrustedArtifacts || s.Outputs || s.CompiledCache() {
			t.Fatalf("%s schema reported compiled-cache features: %+v", name, s)
		}
	}
}

func TestParseSandboxSchema(t *testing.T) {
	s, err := ParseSandboxSchema([]byte(v0151Schema))
	if err != nil {
		t.Fatal(err)
	}
	if s.Ports || s.ReadinessProbe || s.PodMetadata || s.Exposure() || s.Secrets() || s.Egress {
		t.Fatalf("v0.15.1 schema reported features: %+v", s)
	}
	s, err = ParseSandboxSchema([]byte(proposedSchema))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Exposure() || !s.Secrets() || !s.Egress || s.ReadinessStartup {
		t.Fatalf("v0.16.0 schema not detected: %+v", s)
	}
	s, err = ParseSandboxSchema([]byte(v0162Schema))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Exposure() || !s.ReadinessStartup || s.Termination {
		t.Fatalf("v0.16.2 schema not detected: %+v", s)
	}
	s, err = ParseSandboxSchema([]byte(v0163Schema))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Exposure() || !s.ReadinessStartup || !s.Termination {
		t.Fatalf("v0.16.3 schema not detected: %+v", s)
	}
	if _, err := ParseSandboxSchema([]byte(`{"components":{"schemas":{}}}`)); err == nil {
		t.Fatal("missing schema accepted")
	}
	if _, err := ParseSandboxSchema([]byte(`not json`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}
