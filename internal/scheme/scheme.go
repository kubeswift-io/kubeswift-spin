// Package scheme builds the runtime.Scheme used by the controller and tests.
package scheme

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"
)

// SandboxGroupVersion is the KubeSwift sandbox API group version. The upstream
// package exports the group and version as constants but no AddToScheme, so
// the kinds are registered here the same way KubeSwift registers them.
var SandboxGroupVersion = schema.GroupVersion{Group: sandboxv1alpha1.GroupName, Version: sandboxv1alpha1.Version}

// New returns a scheme with the core Kubernetes types, the SpinKube core API,
// and the KubeSwift sandbox API registered.
func New() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(spinv1alpha1.AddToScheme(s))
	s.AddKnownTypes(SandboxGroupVersion,
		&sandboxv1alpha1.SwiftSandbox{}, &sandboxv1alpha1.SwiftSandboxList{},
		&sandboxv1alpha1.SwiftSandboxPool{}, &sandboxv1alpha1.SwiftSandboxPoolList{})
	metav1.AddToGroupVersion(s, SandboxGroupVersion)
	return s
}
