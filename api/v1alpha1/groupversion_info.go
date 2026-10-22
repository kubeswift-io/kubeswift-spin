// Package v1alpha1 contains the spin.kubeswift.io API owned by kubeswift-spin.
//
// SpinCompiledCache is platform state, not a user API: application authors
// keep using SpinApp. Records live in the kubeswift-spin namespace, and only
// the kubeswift-spin cache manager and cache signer write them (ADR 0014).
//
// +kubebuilder:object:generate=true
// +groupName=spin.kubeswift.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the spin.kubeswift.io API group version.
var GroupVersion = schema.GroupVersion{Group: "spin.kubeswift.io", Version: "v1alpha1"}

// SpinCompiledCacheResource is the plural resource name, for discovery.
const SpinCompiledCacheResource = "spincompiledcaches"

// AddToScheme registers the types of this group.
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &SpinCompiledCache{}, &SpinCompiledCacheList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
