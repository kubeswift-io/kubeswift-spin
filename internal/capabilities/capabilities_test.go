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
  "network":{"properties":{"mode":{"type":"string"},"ports":{"type":"array"}}},
  "readinessProbe":{"type":"object"},"podMetadata":{"type":"object"}}}}}}}}`

func TestParseSandboxSchema(t *testing.T) {
	s, err := ParseSandboxSchema([]byte(v0151Schema))
	if err != nil {
		t.Fatal(err)
	}
	if s.Ports || s.ReadinessProbe || s.PodMetadata || s.Exposure() {
		t.Fatalf("v0.15.1 schema reported features: %+v", s)
	}
	s, err = ParseSandboxSchema([]byte(proposedSchema))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Exposure() {
		t.Fatalf("proposed schema not detected: %+v", s)
	}
	if _, err := ParseSandboxSchema([]byte(`{"components":{"schemas":{}}}`)); err == nil {
		t.Fatal("missing schema accepted")
	}
	if _, err := ParseSandboxSchema([]byte(`not json`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}
