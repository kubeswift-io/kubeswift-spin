// Package sandboxapi declares the part of the KubeSwift
// sandbox.kubeswift.io/v1alpha1 API that kubeswift-spin uses.
//
// kubeswift-spin is Apache-2.0 licensed and does not link KubeSwift's Go
// module, which is AGPL-3.0. Only the fields the controller sets or reads are
// declared. This is safe because kubeswift-spin never updates a
// SwiftSandbox or SwiftSandboxPool: it creates sandboxes, reads both kinds,
// and deletes sandboxes, so fields it does not declare are never written
// back.
//
// contract_test.go checks every declared field, type and constant against
// the pinned KubeSwift Go types and CRD schema, so this package cannot drift
// from upstream unnoticed.
//
// +kubebuilder:object:generate=true
// +groupName=sandbox.kubeswift.io
package sandboxapi

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the KubeSwift sandbox API group version.
var GroupVersion = schema.GroupVersion{Group: "sandbox.kubeswift.io", Version: "v1alpha1"}

// AddToScheme registers SwiftSandbox and SwiftSandboxPool.
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &SwiftSandbox{}, &SwiftSandboxList{}, &SwiftSandboxPool{}, &SwiftSandboxPoolList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// SecretObjectReference names a Secret in the object's namespace.
type SecretObjectReference struct {
	Name string `json:"name"`
}

// SandboxRootfsMode selects how the OCI rootfs is delivered to the guest.
type SandboxRootfsMode string

const (
	SandboxRootfsBlock    SandboxRootfsMode = "block"
	SandboxRootfsVirtiofs SandboxRootfsMode = "virtiofs"
)

// SandboxNetworkMode selects the sandbox connectivity posture.
type SandboxNetworkMode string

const (
	SandboxNetworkRestricted SandboxNetworkMode = "restricted"
	SandboxNetworkOpen       SandboxNetworkMode = "open"
	SandboxNetworkNone       SandboxNetworkMode = "none"
)

// SandboxNetwork is the sandbox networking policy.
type SandboxNetwork struct {
	Mode SandboxNetworkMode `json:"mode,omitempty"`
}

// SandboxModel is a read-only model artifact mounted into a sandbox. It is
// declared only so pool compatibility can detect pools that carry one.
type SandboxModel struct {
	ImageRef  string `json:"imageRef"`
	MountPath string `json:"mountPath,omitempty"`
}

// SwiftSandboxSpec is the subset of the sandbox spec kubeswift-spin sets.
type SwiftSandboxSpec struct {
	Image              string                       `json:"image"`
	ImagePullSecret    string                       `json:"imagePullSecret,omitempty"`
	VerifyKeySecretRef *SecretObjectReference       `json:"verifyKeySecretRef,omitempty"`
	CPU                int32                        `json:"cpu,omitempty"`
	Memory             resource.Quantity            `json:"memory"`
	Command            []string                     `json:"command,omitempty"`
	Args               []string                     `json:"args,omitempty"`
	Env                []corev1.EnvVar              `json:"env,omitempty"`
	Network            SandboxNetwork               `json:"network,omitempty"`
	RootfsMode         SandboxRootfsMode            `json:"rootfsMode,omitempty"`
	KernelProfileRef   *corev1.LocalObjectReference `json:"kernelProfileRef,omitempty"`
	NodeSelector       map[string]string            `json:"nodeSelector,omitempty"`
	PoolRef            *corev1.LocalObjectReference `json:"poolRef,omitempty"`
}

// SwiftSandboxPhase is the sandbox lifecycle phase.
type SwiftSandboxPhase string

const (
	SwiftSandboxPending       SwiftSandboxPhase = "Pending"
	SwiftSandboxMaterializing SwiftSandboxPhase = "Materializing"
	SwiftSandboxRunning       SwiftSandboxPhase = "Running"
	SwiftSandboxCompleted     SwiftSandboxPhase = "Completed"
	SwiftSandboxFailed        SwiftSandboxPhase = "Failed"
)

// Sandbox condition types read by kubeswift-spin.
const (
	SwiftSandboxConditionResolved     = "Resolved"
	SwiftSandboxConditionGuestRunning = "GuestRunning"
)

// SwiftSandboxStatus is the subset of the sandbox status kubeswift-spin reads.
type SwiftSandboxStatus struct {
	Phase      SwiftSandboxPhase  `json:"phase,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	Message    string             `json:"message,omitempty"`
}

// SwiftSandbox is a KubeSwift microVM sandbox.
//
// +kubebuilder:object:root=true
type SwiftSandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SwiftSandboxSpec   `json:"spec,omitempty"`
	Status            SwiftSandboxStatus `json:"status,omitempty"`
}

// SwiftSandboxList is a list of SwiftSandbox.
//
// +kubebuilder:object:root=true
type SwiftSandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftSandbox `json:"items"`
}

// SwiftSandboxPoolSpec is the subset of the pool spec kubeswift-spin reads to
// check warm-slot compatibility.
type SwiftSandboxPoolSpec struct {
	Image              string                       `json:"image"`
	VerifyKeySecretRef *SecretObjectReference       `json:"verifyKeySecretRef,omitempty"`
	RootfsMode         SandboxRootfsMode            `json:"rootfsMode,omitempty"`
	CPU                int32                        `json:"cpu,omitempty"`
	Memory             resource.Quantity            `json:"memory"`
	Network            SandboxNetwork               `json:"network,omitempty"`
	KernelProfileRef   *corev1.LocalObjectReference `json:"kernelProfileRef,omitempty"`
	NodeSelector       map[string]string            `json:"nodeSelector,omitempty"`
	GPUProfileRef      *corev1.LocalObjectReference `json:"gpuProfileRef,omitempty"`
	Model              *SandboxModel                `json:"model,omitempty"`
}

// SwiftSandboxPool is a KubeSwift warm pool of pre-booted sandboxes.
//
// +kubebuilder:object:root=true
type SwiftSandboxPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SwiftSandboxPoolSpec `json:"spec,omitempty"`
}

// SwiftSandboxPoolList is a list of SwiftSandboxPool.
//
// +kubebuilder:object:root=true
type SwiftSandboxPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftSandboxPool `json:"items"`
}
