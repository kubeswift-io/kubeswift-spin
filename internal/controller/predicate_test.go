package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"
)

func TestAppAnnotationsChangedPredicate(t *testing.T) {
	obj := func(ann map[string]string) *spinv1alpha1.SpinApp {
		return &spinv1alpha1.SpinApp{ObjectMeta: metav1.ObjectMeta{Annotations: ann}}
	}
	for _, c := range []struct {
		name     string
		old, new map[string]string
		want     bool
	}{
		{"prefixed added", nil, map[string]string{"spin.kubeswift.io/drain-period": "10s"}, true},
		{"prefixed changed", map[string]string{"spin.kubeswift.io/drain-period": "10s"}, map[string]string{"spin.kubeswift.io/drain-period": "20s"}, true},
		{"prefixed removed", map[string]string{"spin.kubeswift.io/drain-period": "10s"}, map[string]string{}, true},
		{"other changed", map[string]string{"example.com/x": "1"}, map[string]string{"example.com/x": "2"}, false},
		{"equal", map[string]string{"spin.kubeswift.io/drain-period": "10s"}, map[string]string{"spin.kubeswift.io/drain-period": "10s"}, false},
		{"nil and empty", nil, map[string]string{}, false},
	} {
		if got := appAnnotationsChanged.Update(event.UpdateEvent{ObjectOld: obj(c.old), ObjectNew: obj(c.new)}); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}
