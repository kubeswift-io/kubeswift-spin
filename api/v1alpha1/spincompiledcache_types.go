package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Architectures a compiled cache can be built for.
const (
	ArchitectureAMD64 = "amd64"
	ArchitectureARM64 = "arm64"
)

// SpinCompiledCacheSpec is the identity of one compiled cache: the Wasmtime
// cache entries of one Spin application, compiled by one runtime image for
// one architecture, for SpinApps of one namespace. The cache manager writes
// it when it creates the record; it never changes.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type SpinCompiledCacheSpec struct {
	// Namespace is the namespace whose SpinApps use the cache. The compiler
	// runs there, and the signed claims bind the artifact to it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`

	// Source is the Spin application, pinned by digest (repository@sha256:...).
	// It is a signed claim value, so at most 256 characters.
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[a-f0-9]{64}$`
	Source string `json:"source"`

	// Runtime is the kubeswift-spin runtime image that compiles and later runs
	// the application, pinned by digest. Its Spin and Wasmtime versions and
	// engine configuration decide whether a cache entry can be used.
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[a-f0-9]{64}$`
	Runtime string `json:"runtime"`

	// Architecture is the CPU architecture the cache is compiled for.
	// +kubebuilder:validation:Enum=amd64;arm64
	Architecture string `json:"architecture"`

	// Repository receives the compiled artifact: registry host and path, no
	// tag or digest. A SwiftArtifactExportPolicy must allow it for Namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?(:[0-9]+)?(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)+$`
	Repository string `json:"repository"`
}

// SandboxReference identifies one SwiftSandbox by name and UID.
type SandboxReference struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// CompileStatus is the state of compilation, written by the cache manager.
type CompileStatus struct {
	// Attempt counts compiler sandboxes created for this record.
	// +optional
	Attempt int32 `json:"attempt,omitempty"`
	// Token identifies the current attempt. It is passed to the compiler
	// sandbox, so a sandbox the cache manager created but did not record
	// (for example because it restarted) is recognized and removed rather
	// than trusted.
	// +optional
	Token string `json:"token,omitempty"`
	// Sandbox is the compiler sandbox of the current attempt, recorded from
	// the create response. The signer signs only that sandbox's output.
	// +optional
	Sandbox *SandboxReference `json:"sandbox,omitempty"`
	// SpecHash is the SHA-256 of the compiler sandbox spec the cache manager
	// created, recorded with its UID; the signer compares the sandbox's spec
	// with it before signing.
	// +optional
	SpecHash string `json:"specHash,omitempty"`
	// StartedAt is when the current attempt's sandbox was created.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// FinishedAt is when it completed or failed.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// Artifact is the exported, unsigned artifact (repository@sha256:...).
	// +optional
	Artifact string `json:"artifact,omitempty"`
	// Size is the artifact's compressed size in bytes.
	// +optional
	Size int64 `json:"size,omitempty"`
	// Failures counts failed attempts since the last success; the next
	// attempt waits out a backoff that doubles with each.
	// +optional
	Failures int32 `json:"failures,omitempty"`
	// LastFailureTime is when an attempt last failed.
	// +optional
	LastFailureTime *metav1.Time `json:"lastFailureTime,omitempty"`
	// RetryAfter is the earliest time of the next attempt.
	// +optional
	RetryAfter *metav1.Time `json:"retryAfter,omitempty"`
	// Reason is a CamelCase word for the last failure.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message says what failed. It names objects and fields, never values.
	// +optional
	Message string `json:"message,omitempty"`
}

// SignatureStatus is the state of signing, written by the cache signer only.
type SignatureStatus struct {
	// Artifact is the artifact the signer checked and signed
	// (repository@sha256:...). It equals compile.artifact once signed.
	// +optional
	Artifact string `json:"artifact,omitempty"`
	// SandboxUID is the compiler sandbox whose output the signer judged. A
	// rejection applies to that attempt only: a later attempt that exports
	// the same bytes is judged again.
	// +optional
	SandboxUID types.UID `json:"sandboxUID,omitempty"`
	// KeyID identifies the signing key: the SHA-256 of its
	// SubjectPublicKeyInfo, as SwiftArtifactVerificationPolicy reports it.
	// +optional
	KeyID string `json:"keyID,omitempty"`
	// Signer is the signer's identity and version.
	// +optional
	Signer string `json:"signer,omitempty"`
	// SignedAt is when the artifact was signed.
	// +optional
	SignedAt *metav1.Time `json:"signedAt,omitempty"`
	// Rejected is true when the signer refused the artifact: it did not
	// pass the signer's checks and is never signed. The cache manager
	// compiles again after a backoff.
	// +optional
	Rejected bool `json:"rejected,omitempty"`
	// Failures counts failed signing attempts of Artifact.
	// +optional
	Failures int32 `json:"failures,omitempty"`
	// LastFailureTime is when signing last failed.
	// +optional
	LastFailureTime *metav1.Time `json:"lastFailureTime,omitempty"`
	// Reason is a CamelCase word for the last failure or rejection.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message says what failed.
	// +optional
	Message string `json:"message,omitempty"`
}

// PlacementStatus is one SwiftArtifactPlacement that prepares the artifact
// on the nodes of a SwiftSandboxPool in spec.namespace.
type PlacementStatus struct {
	// Pool is the SwiftSandboxPool.
	Pool string `json:"pool"`
	// Name is the SwiftArtifactPlacement.
	Name string `json:"name"`
	// Prepared is true when every node of the pool holds the artifact.
	Prepared bool `json:"prepared"`
	// PreparedNodes and TargetNodes are copied from the placement.
	// +optional
	PreparedNodes int32 `json:"preparedNodes"`
	// +optional
	TargetNodes int32 `json:"targetNodes"`
	// Reason is the placement's Prepared condition reason.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message is the placement's Prepared condition message.
	// +optional
	Message string `json:"message,omitempty"`
}

// Consumer is a SpinApp in spec.namespace that uses, or recently used, the
// cache.
type Consumer struct {
	// Name is the SpinApp.
	Name string `json:"name"`
	// Current is true while the SpinApp's image and runtime match this
	// record.
	Current bool `json:"current"`
	// Pool is the SwiftSandboxPool the SpinApp's executor checks out from,
	// if any.
	// +optional
	Pool string `json:"pool,omitempty"`
	// ReplacedAt is when the SpinApp moved to another image or runtime. The
	// most recently replaced record of a SpinApp is kept, so a rollback finds
	// its cache.
	// +optional
	ReplacedAt *metav1.Time `json:"replacedAt,omitempty"`
}

// MaxConsumers bounds status.consumers.
const MaxConsumers = 64

// SpinCompiledCacheStatus is written by the cache manager, except Signature,
// which only the cache signer writes. Each writer owns whole fields
// (consumers, pools and lastReferencedAt; compile, placements, conditions and
// observedGeneration; signature) and replaces them with a JSON merge patch
// that carries the resourceVersion it read and sends cleared fields of an
// object as null, so writers never overwrite each other and a reset field
// really is reset.
type SpinCompiledCacheStatus struct {
	// ObservedGeneration is the generation the conditions describe.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Compile is the state of compilation.
	// +optional
	Compile *CompileStatus `json:"compile,omitempty"`
	// Signature is the state of signing.
	// +optional
	Signature *SignatureStatus `json:"signature,omitempty"`
	// Placements prepare the signed artifact on warm-pool nodes.
	// +listType=map
	// +listMapKey=pool
	// +optional
	Placements []PlacementStatus `json:"placements,omitempty"`
	// Consumers are the SpinApps that use the cache.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Consumers []Consumer `json:"consumers,omitempty"`
	// Pools are the SwiftSandboxPools, in spec.namespace, that the current
	// consumers check out from; each gets a placement.
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Pools []string `json:"pools,omitempty"`
	// LastReferencedAt is when a SpinApp last used the cache as its current
	// image; records nobody references are deleted after a retention period.
	// +optional
	LastReferencedAt *metav1.Time `json:"lastReferencedAt,omitempty"`
	// Conditions: Compiled, Trusted, Placed and Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types of a SpinCompiledCache.
const (
	// ConditionCompiled is True once a compiler sandbox exported the cache.
	ConditionCompiled = "Compiled"
	// ConditionTrusted is True once the signer signed the exported artifact
	// with the bound claims. Only a trusted record is used by replicas.
	ConditionTrusted = "Trusted"
	// ConditionPlaced is True once every warm pool of the record's consumers
	// holds the artifact on every node; False with reason Progressing while
	// any does not.
	ConditionPlaced = "Placed"
	// ConditionReady is True when the record is Trusted and Placed.
	ConditionReady = "Ready"
)

// SpinCompiledCache records a compiled Wasmtime cache for one Spin
// application, runtime image, architecture and namespace. See ADR 0014.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=spincompiledcaches,scope=Namespaced,shortName=spincc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="For",type=string,JSONPath=`.spec.namespace`
// +kubebuilder:printcolumn:name="Arch",type=string,JSONPath=`.spec.architecture`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SpinCompiledCache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec   SpinCompiledCacheSpec   `json:"spec"`
	Status SpinCompiledCacheStatus `json:"status,omitempty"`
}

// SpinCompiledCacheList is a list of SpinCompiledCache.
//
// +kubebuilder:object:root=true
type SpinCompiledCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpinCompiledCache `json:"items"`
}
