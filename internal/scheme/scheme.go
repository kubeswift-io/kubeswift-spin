// Package scheme builds the runtime.Scheme used by the controller and tests.
package scheme

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

// New returns a scheme with the core Kubernetes types, the SpinKube core API,
// and the subset of the KubeSwift sandbox API that kubeswift-spin uses.
func New() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(spinv1alpha1.AddToScheme(s))
	utilruntime.Must(sandboxapi.AddToScheme(s))
	return s
}
