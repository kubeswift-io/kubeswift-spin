package translate

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"
)

// goldenRevision is the revision of goldenApp under testProfile. If this test
// fails, the rendered SwiftSandbox spec changed: every running replica will
// be replaced when users upgrade. That can be legitimate (a translation
// change or a dependency bump that alters JSON encoding of the upstream
// types), but it must be intentional: update the value and add an upgrade
// note to CHANGELOG.md.
const goldenRevision = "56ab95466e"

func goldenApp() *spinv1alpha1.SpinApp {
	a := testApp()
	a.Spec.Components = []string{"api"}
	a.Spec.Variables = []spinv1alpha1.SpinVar{{Name: "greeting", Value: "hi"}}
	a.Spec.InvocationLimits = map[string]string{"memory": "64Mi"}
	a.Spec.Resources.Limits = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("500m"),
		corev1.ResourceMemory: resource.MustParse("1Gi"),
	}
	a.Spec.RuntimeConfig.KeyValueStores = []spinv1alpha1.KeyValueStoreConfig{{Name: "default", Type: "spin"}}
	return a
}

func TestGoldenRevision(t *testing.T) {
	tm, err := BuildTemplate(goldenApp(), testProfile(), DefaultResourcePolicy(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	if tm.Revision != goldenRevision {
		t.Fatalf("revision changed from %s to %s: the rendered sandbox spec changed and upgrades would replace every replica; "+
			"if intended, update goldenRevision and add an upgrade note to CHANGELOG.md", goldenRevision, tm.Revision)
	}
}

// goldenRevisionV16 pins the revision of goldenApp on a KubeSwift with
// every v0.16.0 sandbox feature.
const goldenRevisionV16 = "9750b02d06"

func TestGoldenRevisionV16(t *testing.T) {
	tm, err := BuildTemplate(goldenApp(), testProfile(), DefaultResourcePolicy(), v16)
	if err != nil {
		t.Fatal(err)
	}
	if tm.Revision != goldenRevisionV16 {
		t.Fatalf("revision changed from %s to %s: the rendered sandbox spec changed and upgrades would replace every replica; "+
			"if intended, update goldenRevisionV16 and add an upgrade note to CHANGELOG.md", goldenRevisionV16, tm.Revision)
	}
}
