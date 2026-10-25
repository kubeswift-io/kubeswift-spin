// Package localapp decides, purely, whether a SpinApp's warm replicas start
// Spin from a KubeSwift-mounted copy of their own application artifact (ADR
// 0015), and describes the SwiftArtifactPlacement that prepares it and the
// artifact a replica mounts. The controller and the cache manager both use
// it, so they always agree.
package localapp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

// Names that are API: labels, annotations, the artifact name and the
// condition. Do not rename without an upgrade note.
const (
	// ArtifactName is the replica artifact and the placement artifact.
	ArtifactName = "application"
	// Layout mounts the artifact as an OCI image layout.
	Layout = "oci"
	// ComponentPlacement labels the application placements.
	ComponentPlacement = "application-placement"
	// AnnKey holds the full key hash of a placement.
	AnnKey = executor.Prefix + "application-key"
	// AnnUnwantedSince records when no SpinApp wanted a placement any more.
	AnnUnwantedSince = executor.Prefix + "unwanted-since"
	// NamespaceLabel, set to NamespaceEnabled by an administrator, lets a
	// namespace's SpinApps use local applications.
	NamespaceLabel   = executor.Prefix + "local-application"
	NamespaceEnabled = "enabled"
	// PlacementPrefix starts every application placement name.
	PlacementPrefix = "kss-app-"

	labelManagedBy = "app.kubernetes.io/managed-by"
	managedBy      = "kubeswift-spin"
	labelComponent = executor.Prefix + "component"
)

// TypeLocalApplication is the SpinApp condition.
const TypeLocalApplication = "LocalApplication"

// Reasons of the LocalApplication condition (API).
const (
	ReasonNotConfigured       = "NotConfigured"
	ReasonNamespaceNotEnabled = "NamespaceNotEnabled"
	ReasonUnsupported         = "Unsupported"
	ReasonImageNotPinned      = "ImageNotPinned"
	ReasonRegistryNotEligible = "RegistryNotEligible"
	ReasonPullSecrets         = "PullSecrets"
	ReasonComponentsSelected  = "ComponentsSelected"
	ReasonRuntimeNotAllowed   = "RuntimeNotAllowed"
	ReasonNoWarmPool          = "NoWarmPool"
	ReasonPlacing             = "Placing"
	ReasonPlacementFailed     = "PlacementFailed"
	ReasonPlacementLimit      = "PlacementLimit"
	ReasonPlacementBlocked    = "PlacementBlocked"
	ReasonDeliveryFailed      = "DeliveryFailed"
	ReasonAttached            = "Attached"
)

// Settings are the platform's (chart values localApplication.*).
type Settings struct {
	Enabled bool
	// Runtimes are the runtime images (repository@sha256) whose Spin and
	// converter were tested together.
	Runtimes []string
	// Registries, when not empty, are the only registry hosts eligible.
	Registries []string
	// MaxPlacements per namespace, retained ones included.
	MaxPlacements int
	// MaxNewPerHour is how many of a namespace's placements may have been
	// created within the last hour.
	MaxNewPerHour int
	// Retention keeps an unwanted placement, for rollbacks.
	Retention time.Duration
}

// Defaults of ADR 0015.
const (
	DefaultMaxPlacements = 8
	DefaultMaxNewPerHour = 10
	DefaultRetention     = 24 * time.Hour
)

// Validate checks the settings.
func (s Settings) Validate() error {
	if !s.Enabled {
		return nil
	}
	if len(s.Runtimes) == 0 {
		return fmt.Errorf("local applications need at least one allowed runtime image")
	}
	for _, r := range s.Runtimes {
		if c, err := compiledcache.PinnedReference(r); err != nil || c != r {
			return fmt.Errorf("local application runtime %q must be a canonical repository@sha256 reference", r)
		}
	}
	if s.MaxPlacements < 1 || s.MaxNewPerHour < 1 || s.Retention < 0 {
		return fmt.Errorf("local application limits must be positive")
	}
	for _, h := range s.Registries {
		reg, err := name.NewRegistry(h, name.StrictValidation)
		if err != nil || reg.RegistryStr() != h {
			want := h
			if err == nil {
				want = reg.RegistryStr()
			}
			return fmt.Errorf("local application registry %q must be a registry host as images normalize it (%q; Docker Hub is index.docker.io)", h, want)
		}
	}
	return nil
}

// Skip says why a SpinApp that asks for a local application cannot have one.
type Skip struct {
	Reason  string
	Message string
}

// Requested reports whether a SpinApp asks for a local application: its
// executor enables it and the SpinApp does not opt out.
func Requested(app *spinv1alpha1.SpinApp, p *executor.Profile) bool {
	if p == nil || !p.LocalApplication {
		return false
	}
	v, set := app.Annotations[executor.AnnLocalApplication]
	return !set || v == executor.LocalApplicationEnabled
}

// NamespaceEnabledFor reports whether a namespace with these labels may use
// local applications.
func NamespaceEnabledFor(labels map[string]string) bool {
	return labels[NamespaceLabel] == NamespaceEnabled
}

// Key identifies an application placement.
type Key struct {
	Namespace  string
	Pool       string
	Ref        string // canonical repository@sha256:<hex>
	PullSecret string // the SpinApp's image pull Secret, or ""
}

// Hash is the full SHA-256 of the key.
func (k Key) Hash() string {
	h := sha256.Sum256([]byte(strings.Join([]string{k.Namespace, k.Pool, k.Ref, k.PullSecret}, "\x00")))
	return hex.EncodeToString(h[:])
}

// PlacementName is the deterministic placement name of the key.
func (k Key) PlacementName() string { return PlacementPrefix + k.Hash()[:20] }

// Digest is the manifest digest of the key's reference.
func (k Key) Digest() string { return k.Ref[strings.LastIndexByte(k.Ref, '@')+1:] }

// Input is everything Decide reads.
type Input struct {
	App              *spinv1alpha1.SpinApp
	Profile          *executor.Profile
	Settings         Settings
	NamespaceEnabled bool
	// Supported reports that the installed KubeSwift serves optional OCI
	// layout artifacts with pull Secrets and placements.
	Supported bool
}

// imagePattern is KubeSwift's placement reference pattern.
var imagePattern = regexp.MustCompile(`^[^@\s]+@sha256:[a-f0-9]{64}$`)

// Decide returns the placement key of a SpinApp that asks for a local
// application, or why it cannot have one, or (nil, nil) when it does not ask
// (no condition then).
func Decide(in Input) (*Key, *Skip) {
	app, p, s := in.App, in.Profile, in.Settings
	if !Requested(app, p) {
		return nil, nil
	}
	skip := func(reason, format string, a ...any) (*Key, *Skip) {
		return nil, &Skip{Reason: reason, Message: fmt.Sprintf(format, a...)}
	}
	switch {
	case !s.Enabled:
		return skip(ReasonNotConfigured, "executor %q enables %s, but the platform has not enabled local applications (chart value localApplication.enabled)",
			p.ExecutorName, executor.AnnLocalApplication)
	case !in.NamespaceEnabled:
		return skip(ReasonNamespaceNotEnabled, "namespace %q is not enabled for local applications (namespace label %s=%s, set by an administrator)",
			app.Namespace, NamespaceLabel, NamespaceEnabled)
	case !in.Supported:
		return skip(ReasonUnsupported, "the installed KubeSwift does not serve optional OCI layout artifacts with placements (KubeSwift v0.19.3 or later is required)")
	case p.SandboxPool == "":
		return skip(ReasonNoWarmPool, "executor %q has no warm pool (annotation %s); local applications serve warm-pool replicas", p.ExecutorName, executor.AnnSandboxPool)
	}
	if len(app.Spec.Image) > 512 || !imagePattern.MatchString(app.Spec.Image) {
		return skip(ReasonImageNotPinned, "spec.image is not pinned by digest; only repository@sha256:... images can start from a local copy")
	}
	ref, err := compiledcache.PinnedReference(app.Spec.Image)
	if err != nil {
		return skip(ReasonImageNotPinned, "spec.image is not a valid digest-pinned reference")
	}
	host, err := registryHost(ref)
	if err != nil || plaintextHost(host) {
		return skip(ReasonRegistryNotEligible, "the registry of spec.image is one KubeSwift may contact over plain HTTP, or a local address (localhost, .localhost, or a loopback, private, link-local or unspecified IP address); new replicas start from the registry")
	}
	if len(s.Registries) > 0 && !slices.Contains(s.Registries, host) {
		return skip(ReasonRegistryNotEligible, "the registry of spec.image is not in the platform's list (chart value localApplication.registries)")
	}
	pull := ""
	switch {
	case len(app.Spec.ImagePullSecrets) > 1:
		return skip(ReasonPullSecrets, "spec.imagePullSecrets names more than one Secret; a local application is fetched with exactly one")
	case len(app.Spec.ImagePullSecrets) == 1:
		pull = app.Spec.ImagePullSecrets[0].Name
	case p.RuntimeImagePullSecret != "":
		return skip(ReasonPullSecrets, "the SpinApp has no image pull Secret, but executor %q sets %s, which KubeSwift would use to fetch the application",
			p.ExecutorName, executor.AnnRuntimeImagePullSecret)
	}
	if len(app.Spec.Components) > 0 {
		return skip(ReasonComponentsSelected, "spec.components selects components, which the local runner does not support; new replicas start from the registry")
	}
	runtime, err := compiledcache.PinnedReference(p.RuntimeImage)
	if err != nil || !slices.Contains(s.Runtimes, runtime) {
		return skip(ReasonRuntimeNotAllowed, "executor %q uses a runtime image the platform has not allowed for local applications (chart values runtimeImage.digest and localApplication.extraRuntimes)",
			p.ExecutorName)
	}
	return &Key{Namespace: app.Namespace, Pool: p.SandboxPool, Ref: ref, PullSecret: pull}, nil
}

// registryHost returns the registry host of a canonical reference, as
// go-containerregistry normalizes it (docker.io is index.docker.io).
func registryHost(ref string) (string, error) {
	r, err := name.ParseReference(ref, name.StrictValidation)
	if err != nil {
		return "", err
	}
	return r.Context().RegistryStr(), nil
}

// plaintextHost reports whether go-containerregistry (KubeSwift's fetcher)
// may use plain HTTP for a registry host, or the host is otherwise local:
// localhost, names under .localhost, and loopback, private, link-local and
// unspecified IP literals. Names under .local (in-cluster registries such as
// *.svc.cluster.local) are fetched over HTTPS and stay eligible; the
// platform can restrict hosts with Settings.Registries.
func plaintextHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	return false
}

// Artifact is the artifact a replica of the key mounts.
func Artifact(k Key) sandboxv1alpha1.SandboxArtifact {
	a := sandboxv1alpha1.SandboxArtifact{
		Name:         ArtifactName,
		Ref:          k.Ref,
		MountPath:    runtimecontract.LocalAppDir,
		Layout:       Layout,
		Optional:     true,
		FetchTimeout: &metav1.Duration{},
	}
	if k.PullSecret != "" {
		a.PullSecretRef = &corev1.LocalObjectReference{Name: k.PullSecret}
	}
	return a
}

// Decorate adds the local application to a new replica. It is applied after
// the revision fingerprint, so it never replaces running replicas.
func Decorate(sb *sandboxv1alpha1.SwiftSandbox, k Key) {
	sb.Spec.Artifacts = append(sb.Spec.Artifacts, Artifact(k))
	sb.Spec.Env = append(sb.Spec.Env, corev1.EnvVar{Name: runtimecontract.LocalAppDigestEnv, Value: k.Digest()})
}

// Placement is the placement the cache manager writes for the key.
func Placement(k Key) *sandboxv1alpha1.SwiftArtifactPlacement {
	a := Artifact(k)
	return &sandboxv1alpha1.SwiftArtifactPlacement{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   k.Namespace,
			Name:        k.PlacementName(),
			Labels:      map[string]string{labelManagedBy: managedBy, labelComponent: ComponentPlacement},
			Annotations: map[string]string{AnnKey: k.Hash()},
		},
		Spec: sandboxv1alpha1.SwiftArtifactPlacementSpec{
			PoolRef: corev1.LocalObjectReference{Name: k.Pool},
			Artifacts: []sandboxv1alpha1.PlacementArtifact{{
				Name: a.Name, Ref: a.Ref, Layout: a.Layout, PullSecretRef: a.PullSecretRef,
			}},
		},
	}
}

// Owned reports whether pl is the cache manager's placement for k: both
// labels, the key annotation, the key's name, and the key-bearing spec
// fields. Other spec fields may differ (API defaults, later versions); the
// cache manager brings them back.
func Owned(pl *sandboxv1alpha1.SwiftArtifactPlacement, k Key) bool {
	if pl == nil || pl.Namespace != k.Namespace || pl.Name != k.PlacementName() ||
		pl.Labels[labelManagedBy] != managedBy || pl.Labels[labelComponent] != ComponentPlacement ||
		pl.Annotations[AnnKey] != k.Hash() || pl.Spec.PoolRef.Name != k.Pool || len(pl.Spec.Artifacts) != 1 {
		return false
	}
	a := pl.Spec.Artifacts[0]
	pull := ""
	if a.PullSecretRef != nil {
		pull = a.PullSecretRef.Name
	}
	layout := a.Layout
	if layout == "" {
		layout = Layout // the API default
	}
	return a.Ref == k.Ref && layout == Layout && pull == k.PullSecret && len(a.Claims) == 0 && !a.RequirePlatformVerification
}

// Prepared reports whether an owned placement has prepared every target
// node, for its current generation.
func Prepared(pl *sandboxv1alpha1.SwiftArtifactPlacement, k Key) bool {
	if !Owned(pl, k) || pl.Status.ObservedGeneration != pl.Generation {
		return false
	}
	for _, c := range pl.Status.Conditions {
		if c.Type == sandboxv1alpha1.PlacementConditionPrepared {
			return c.Status == metav1.ConditionTrue && c.ObservedGeneration == pl.Generation
		}
	}
	return false
}

// WantsPlacement reports whether a SpinApp can want a placement at all: it
// is not being deleted and has replicas. The controller and the cache
// manager both use it.
func WantsPlacement(app *spinv1alpha1.SpinApp) bool {
	return app.DeletionTimestamp.IsZero() && app.Spec.Replicas > 0
}

// Want is one SpinApp's request for a key.
type Want struct {
	Key     Key
	Created time.Time // the SpinApp's creation
	App     string    // the SpinApp's name
}

// Existing is an owned placement's lifecycle state.
type Existing struct {
	Key           Key
	Created       time.Time
	UnwantedSince *time.Time
}

// Selection is what a namespace's placements should be.
type Selection struct {
	// Keep are the wanted keys that get (or keep) a placement.
	Keep []Key
	// Limited are wanted keys beyond a limit.
	Limited []Key
	// Retain are unwanted placements kept until their retention ends.
	Retain []Key
	// Delete are unwanted placements whose retention ended or that make
	// room for wanted ones.
	Delete []Key
	// NewlyUnwanted are placements to mark with AnnUnwantedSince now.
	NewlyUnwanted []Key
}

// Select decides a namespace's placements. It depends only on current
// objects, so the controller and the cache manager compute the same result.
//
// Wanted keys that already have a placement are served first, then wanted
// keys without one, each in a stable order (oldest SpinApp creation, then
// name), so a SpinApp that starts wanting a placement never displaces a
// prepared one. A placement created within the last hour is never deleted to
// make room: it keeps counting against the hourly limit, which therefore
// bounds how many placements a namespace creates per hour. Wanted keys beyond
// a limit are Limited, and their existing placements are deleted.
func Select(wants []Want, existing []Existing, now time.Time, s Settings) Selection {
	var sel Selection
	// One entry per key, ordered by its oldest wanting SpinApp.
	first := map[Key]Want{}
	for _, w := range wants {
		if f, ok := first[w.Key]; !ok || w.Created.Before(f.Created) || (w.Created.Equal(f.Created) && w.App < f.App) {
			first[w.Key] = w
		}
	}
	ordered := make([]Want, 0, len(first))
	for _, w := range first {
		ordered = append(ordered, w)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if !a.Created.Equal(b.Created) {
			return a.Created.Before(b.Created)
		}
		if a.App != b.App {
			return a.App < b.App
		}
		return a.Key.Hash() < b.Key.Hash()
	})
	exists := map[Key]Existing{}
	for _, e := range existing {
		exists[e.Key] = e
	}
	young := func(e Existing) bool { return now.Sub(e.Created) < time.Hour }
	recent := 0
	for _, e := range existing {
		if young(e) {
			recent++
		}
	}
	// Unwanted placements, the young ones first: they stay for the hour.
	var unwanted []Existing
	youngUnwanted := 0
	for _, e := range existing {
		if _, wanted := first[e.Key]; !wanted {
			unwanted = append(unwanted, e)
			if young(e) {
				youngUnwanted++
			}
		}
	}
	sort.Slice(unwanted, func(i, j int) bool {
		if young(unwanted[i]) != young(unwanted[j]) {
			return young(unwanted[i])
		}
		return unwanted[i].Key.Hash() < unwanted[j].Key.Hash()
	})
	// Wanted keys with a placement keep it within the count limit.
	for _, w := range ordered {
		if _, has := exists[w.Key]; !has {
			continue
		}
		if len(sel.Keep) < s.MaxPlacements {
			sel.Keep = append(sel.Keep, w.Key)
		} else {
			sel.Limited = append(sel.Limited, w.Key)
			sel.Delete = append(sel.Delete, w.Key)
		}
	}
	// Wanted keys without one need room beside the young unwanted
	// placements, and room in the hourly limit.
	for _, w := range ordered {
		if _, has := exists[w.Key]; has {
			continue
		}
		if len(sel.Keep)+youngUnwanted >= s.MaxPlacements || recent >= s.MaxNewPerHour {
			sel.Limited = append(sel.Limited, w.Key)
			continue
		}
		sel.Keep = append(sel.Keep, w.Key)
		recent++
	}
	// Unwanted placements: young ones are retained; older ones until their
	// retention ends, while the count limit has room after the wanted ones.
	room := s.MaxPlacements - len(sel.Keep)
	for _, e := range unwanted {
		retained := young(e) || (room > 0 && (e.UnwantedSince == nil || now.Sub(*e.UnwantedSince) < s.Retention))
		if !retained {
			sel.Delete = append(sel.Delete, e.Key)
			continue
		}
		if e.UnwantedSince == nil {
			sel.NewlyUnwanted = append(sel.NewlyUnwanted, e.Key)
		}
		sel.Retain = append(sel.Retain, e.Key)
		room--
	}
	return sel
}
