// Package compiledcache holds the pure logic of the trusted compiled
// Wasmtime cache (ADR 0014): the lookup identity of a SpinApp, the name of
// its SpinCompiledCache record, the signed claims, and the conditions
// reported on records and SpinApps.
//
// The controller, the cache manager and the cache signer all use this
// package, so the identity a replica asks for, the identity the compiler
// builds and the identity the signer signs cannot drift apart.
package compiledcache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
)

// Signed claim keys. The signer puts every one of them into the signature,
// and replicas require them through SwiftSandbox artifact claims, so a
// signature made for another namespace, application, runtime or
// architecture is never accepted. These are API: do not rename them.
const (
	ClaimKind         = executor.Prefix + "kind"
	ClaimNamespace    = executor.Prefix + "namespace"
	ClaimSource       = executor.Prefix + "source"
	ClaimRuntime      = executor.Prefix + "runtime"
	ClaimArchitecture = executor.Prefix + "architecture"
	ClaimCompiler     = executor.Prefix + "compiler"

	// KindWasmtimeCache is the value of ClaimKind.
	KindWasmtimeCache = "wasmtime-cache"
)

// Labels on objects the cache manager creates.
const (
	// LabelRecord names the SpinCompiledCache a compiler sandbox or a
	// placement belongs to.
	LabelRecord = executor.Prefix + "compiled-cache"
	// LabelNamespace is the consumer namespace of a record, so records of one
	// namespace can be listed.
	LabelNamespace = executor.Prefix + "for-namespace"
	// LabelComponent says which part of the feature created an object.
	LabelComponent = executor.Prefix + "component"
)

// ArchitectureLabel is the node label whose value in an executor's
// node selector decides the architecture of its compiled cache.
const ArchitectureLabel = "kubernetes.io/arch"

// RepositorySuffix is appended to <base>/<namespace> to form a namespace's
// compiled-cache repository.
const RepositorySuffix = "wasmtime-cache"

// Limits of the identity. KubeSwift accepts claim values of at most 256
// characters from a restricted set, and the record's repository field holds
// at most 255 characters.
const (
	maxClaimValue = 256
	maxNamespace  = 63
	maxRepository = 255
)

var (
	// repositoryPattern is a registry host with an optional port and an OCI
	// distribution repository path; spec.repository uses the same pattern.
	repositoryPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?(:[0-9]+)?(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)+$`)
	// claimValue is KubeSwift's claim value character set.
	claimValue = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]+$`)
)

// NamespaceLabel is the label an administrator sets on a Namespace, with
// value NamespaceEnabled, to let its SpinApps use compiled caches. The
// platform's export and verification policies select the same label. An
// administrator sets it only on namespaces that meet the prerequisites in
// docs/compiled-cache.md (no tenant exec into KubeSwift pods there).
const (
	NamespaceLabel   = executor.Prefix + "compiled-cache"
	NamespaceEnabled = "enabled"
)

// Settings are the platform's compiled-cache settings, from controller
// flags. The feature is configured only when Repository is set.
type Settings struct {
	// Namespace holds the SpinCompiledCache records (the kubeswift-spin
	// namespace).
	Namespace string
	// Repository is the base repository; each namespace's artifacts go to
	// <Repository>/<namespace>/wasmtime-cache.
	Repository string
	// Architecture is used for executors whose node selector does not name
	// kubernetes.io/arch.
	Architecture string
	// Runtimes are the runtime images, pinned by digest, that compiled
	// caches may be built and signed for: the platform's runtime image and
	// any it adds. A runtime an executor names that is not listed gets no
	// cache, so the signer never vouches for code from an image the
	// platform did not choose.
	Runtimes []string
}

// Configured reports whether the platform configured the feature.
func (s Settings) Configured() bool { return s.Repository != "" && s.Namespace != "" }

// Validate checks the settings.
func (s Settings) Validate() error {
	if s.Repository == "" {
		return nil
	}
	// KubeSwift matches export and verification policies against the
	// canonical repository, so the setting must already be canonical, and
	// every namespace's repository must fit the OCI path grammar and the
	// record's length limit.
	repo, err := name.NewRepository(s.Repository, name.StrictValidation)
	if err != nil || !repositoryPattern.MatchString(s.Repository) || repo.Name() != s.Repository {
		return fmt.Errorf("compiled-cache repository %q must be a lowercase registry host and OCI repository path, without tag or digest, such as registry.example.com/kubeswift-spin-cache", s.Repository)
	}
	if n := len(s.Repository) + len("/") + maxNamespace + len("/"+RepositorySuffix); n > maxRepository {
		return fmt.Errorf("compiled-cache repository %q is too long: with a 63-character namespace a cache repository would be %d characters, at most %d", s.Repository, n, maxRepository)
	}
	if len(s.Runtimes) == 0 {
		return fmt.Errorf("compiled caches need at least one allowed runtime image pinned by digest")
	}
	for _, r := range s.Runtimes {
		if c, err := PinnedReference(r); err != nil || c != r {
			return fmt.Errorf("allowed runtime %q must be a canonical repository@sha256:<digest> reference", r)
		}
	}
	switch s.Architecture {
	case cachev1alpha1.ArchitectureAMD64, cachev1alpha1.ArchitectureARM64:
	default:
		return fmt.Errorf("compiled-cache architecture must be %s or %s, got %q", cachev1alpha1.ArchitectureAMD64, cachev1alpha1.ArchitectureARM64, s.Architecture)
	}
	if s.Namespace == "" {
		return fmt.Errorf("compiled-cache records need a namespace (the controller's own)")
	}
	return nil
}

// Identity is the lookup identity of a compiled cache. Repository is where
// its artifact is exported: a record made for another repository (before
// the platform changed compiledCache.repository) is a different record.
type Identity struct {
	Namespace    string
	Source       string
	Runtime      string
	Architecture string
	Repository   string
}

// RecordName is the deterministic name of the identity's SpinCompiledCache.
// Every SpinApp with the same identity shares the record, and concurrent
// reconciles of several replicas, SpinApps or controller restarts converge
// on one record and one compilation.
func (i Identity) RecordName() string {
	h := sha256.New()
	for _, part := range []string{i.Namespace, i.Source, i.Runtime, i.Architecture, i.Repository} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "spincc-" + hex.EncodeToString(h.Sum(nil))[:32]
}

// NamespaceRepository is a namespace's compiled-cache repository.
func (s Settings) NamespaceRepository(namespace string) string {
	return s.Repository + "/" + namespace + "/" + RepositorySuffix
}

// Spec is the record spec for the identity.
func (i Identity) Spec() cachev1alpha1.SpinCompiledCacheSpec {
	return cachev1alpha1.SpinCompiledCacheSpec{
		Namespace:    i.Namespace,
		Source:       i.Source,
		Runtime:      i.Runtime,
		Architecture: i.Architecture,
		Repository:   i.Repository,
	}
}

// Matches reports whether rec is the record of this identity.
func (i Identity) Matches(rec *cachev1alpha1.SpinCompiledCache) bool {
	return rec != nil && rec.Spec == i.Spec()
}

// Claims are the signed claims of the identity's artifact. compiler is the
// signer's identity and version.
func (i Identity) Claims(compiler string) map[string]string {
	return map[string]string{
		ClaimKind:         KindWasmtimeCache,
		ClaimNamespace:    i.Namespace,
		ClaimSource:       i.Source,
		ClaimRuntime:      i.Runtime,
		ClaimArchitecture: i.Architecture,
		ClaimCompiler:     compiler,
	}
}

// ArtifactClaims are the claims a replica requires of the artifact it
// mounts (SwiftSandbox spec.artifacts[].claims). KubeSwift refuses the
// artifact unless a platform-verified signature carries every one of them,
// whatever the administrator's policy lists.
func (i Identity) ArtifactClaims() map[string]string {
	return map[string]string{
		ClaimKind:         KindWasmtimeCache,
		ClaimNamespace:    i.Namespace,
		ClaimSource:       i.Source,
		ClaimRuntime:      i.Runtime,
		ClaimArchitecture: i.Architecture,
	}
}

// Reasons a SpinApp gets no compiled cache, and the SpinApp CompiledCache
// condition reasons. They are API: do not rename them.
const (
	ReasonNotConfigured       = "NotConfigured"
	ReasonImageNotPinned      = "ImageNotPinned"
	ReasonRuntimeNotPinned    = "RuntimeNotPinned"
	ReasonUnsupported         = "Unsupported"
	ReasonRuntimeNotAllowed   = "RuntimeNotAllowed"
	ReasonNamespaceNotEnabled = "NamespaceNotEnabled"
	ReasonRequested           = "Requested"
	ReasonCompiling           = "Compiling"
	ReasonCompileFailed       = "CompileFailed"
	ReasonSigning             = "Signing"
	ReasonSignFailed          = "SignFailed"
	ReasonPlacing             = "Placing"
	ReasonPlacementFailed     = "PlacementFailed"
	ReasonReady               = "Ready"
)

// Skip says why a SpinApp that asked for a compiled cache cannot have one.
type Skip struct {
	Reason  string
	Message string
}

// Requested reports whether a SpinApp asks for a compiled cache: its
// executor enables it and the SpinApp does not opt out. An invalid SpinApp
// value is not a request (compatibility reports it).
func Requested(app *spinv1alpha1.SpinApp, p *executor.Profile) bool {
	if p == nil || !p.CompiledCache {
		return false
	}
	v, set := app.Annotations[executor.AnnCompiledCache]
	return !set || v == executor.CompiledCacheEnabled
}

// NamespaceEnabledFor reports whether a namespace with these labels may use
// compiled caches.
func NamespaceEnabledFor(labels map[string]string) bool {
	return labels[NamespaceLabel] == NamespaceEnabled
}

// Derive returns the lookup identity of a SpinApp that asked for a compiled
// cache, or why it cannot have one. Only digest-pinned application and
// runtime images have an identity: a tag can move, and the controller has no
// registry credentials to resolve one.
func Derive(app *spinv1alpha1.SpinApp, p *executor.Profile, s Settings) (*Identity, *Skip) {
	if !s.Configured() {
		return nil, &Skip{Reason: ReasonNotConfigured,
			Message: fmt.Sprintf("executor %q enables %s, but the platform has not configured compiled caches (chart value compiledCache.enabled)", p.ExecutorName, executor.AnnCompiledCache)}
	}
	source, err := PinnedReference(app.Spec.Image)
	if err != nil {
		return nil, &Skip{Reason: ReasonImageNotPinned,
			Message: "spec.image is not pinned by digest; only repository@sha256:... images get a compiled cache"}
	}
	runtime, err := PinnedReference(p.RuntimeImage)
	if err != nil {
		return nil, &Skip{Reason: ReasonRuntimeNotPinned,
			Message: fmt.Sprintf("the runtime image of executor %q is not pinned by digest", p.ExecutorName)}
	}
	if !slices.Contains(s.Runtimes, runtime) {
		return nil, &Skip{Reason: ReasonRuntimeNotAllowed,
			Message: fmt.Sprintf("executor %q uses a runtime image the platform does not build compiled caches for (chart values runtimeImage.digest and compiledCache.extraRuntimes)", p.ExecutorName)}
	}
	arch := s.Architecture
	if v, ok := p.NodeSelector[ArchitectureLabel]; ok {
		arch = v
	}
	switch arch {
	case cachev1alpha1.ArchitectureAMD64, cachev1alpha1.ArchitectureARM64:
	default:
		return nil, &Skip{Reason: ReasonUnsupported,
			Message: fmt.Sprintf("executor %q selects nodes with %s=%s; compiled caches are built for amd64 and arm64 only", p.ExecutorName, ArchitectureLabel, arch)}
	}
	for what, ref := range map[string]string{"spec.image": source, "the runtime image": runtime} {
		if len(ref) > maxClaimValue || !claimValue.MatchString(ref) {
			return nil, &Skip{Reason: ReasonUnsupported,
				Message: fmt.Sprintf("%s is %d characters; compiled caches need image references of at most %d", what, len(ref), maxClaimValue)}
		}
	}
	return &Identity{Namespace: app.Namespace, Source: source, Runtime: runtime, Architecture: arch,
		Repository: s.NamespaceRepository(app.Namespace)}, nil
}

// PinnedReference returns ref in canonical form, repository@sha256:<hex>,
// with any tag dropped. It fails unless ref names a sha256 digest.
func PinnedReference(ref string) (string, error) {
	d, err := name.NewDigest(ref, name.StrictValidation)
	if err != nil {
		if _, terr := name.ParseReference(ref, name.StrictValidation); terr == nil {
			return "", fmt.Errorf("%q has no digest", bounded(ref))
		}
		return "", fmt.Errorf("%q is not a valid image reference", bounded(ref))
	}
	if !strings.HasPrefix(d.DigestStr(), "sha256:") {
		return "", fmt.Errorf("%q does not use a sha256 digest", bounded(ref))
	}
	return d.Context().Name() + "@" + d.DigestStr(), nil
}

func bounded(v string) string {
	if len(v) > 96 {
		return v[:96] + "..."
	}
	return v
}

// Backoff is the wait before compile attempt failures+1: 1 minute doubling
// to at most 6 hours, so a malformed application is not compiled in a loop
// while a transient failure is retried soon.
func Backoff(failures int32) time.Duration {
	const base, ceiling = time.Minute, 6 * time.Hour
	if failures <= 0 {
		return 0
	}
	d := base
	for i := int32(1); i < failures; i++ {
		d *= 2
		if d >= ceiling {
			return ceiling
		}
	}
	return d
}
