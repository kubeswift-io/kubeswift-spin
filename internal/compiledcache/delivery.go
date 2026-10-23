package compiledcache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

// ArtifactName is the name of the compiled cache in a replica's
// spec.artifacts and in a placement.
const ArtifactName = "wasmtime-cache"

// ArtifactLayout unpacks the artifact's layer into a tree, so the guest
// sees metadata.json and the Wasmtime cache directory.
const ArtifactLayout = "unpacked"

// ComponentPlacement labels the SwiftArtifactPlacements of the cache
// manager.
const ComponentPlacement = "placement"

// Fetch timeouts of a replica's optional cache artifact. A replica checked
// out of a warm pool uses the cache only if its node already holds it
// (a placement prepares the nodes), so it never waits. A cold replica gets
// the cache only when the platform sets a cold fetch timeout: KubeSwift
// verifies the signature with the registry on every cold start and, on a
// node without the entry, fetches it, all within the timeout counted from
// the sandbox's creation. On the KVM lab a first fetch did not finish in 3
// seconds, and for small applications the verification costs more than
// the compilation it saves.
const (
	DefaultColdFetchTimeout = 0
	MaxColdFetchTimeout     = 5 * time.Minute
)

// Delivery are the platform's settings for mounting signed caches.
type Delivery struct {
	// PullSecret names a docker-registry Secret, in every enabled namespace,
	// that can read that namespace's cache repository. Empty means the
	// replica's runtime pull Secret, and anonymous reads for placements.
	PullSecret string
	// ColdFetchTimeout bounds how long a replica that does not come from a
	// warm pool may wait for its cache; zero gives such replicas no cache.
	ColdFetchTimeout time.Duration
}

// Validate checks the settings.
func (d Delivery) Validate() error {
	if d.ColdFetchTimeout < 0 || d.ColdFetchTimeout > MaxColdFetchTimeout {
		return fmt.Errorf("compiled-cache fetch timeout must be between 0s and %s, got %s", MaxColdFetchTimeout, d.ColdFetchTimeout)
	}
	return nil
}

// ReplicaArtifact is the optional, platform-verified artifact a replica of
// the identity mounts. artifact is the record's trusted reference
// (TrustedArtifact). pooled is true for a replica checked out of a warm
// pool.
func (d Delivery) ReplicaArtifact(id Identity, artifact string, pooled bool) sandboxv1alpha1.SandboxArtifact {
	timeout := d.ColdFetchTimeout
	if pooled {
		timeout = 0
	}
	a := sandboxv1alpha1.SandboxArtifact{
		Name:                        ArtifactName,
		Ref:                         artifact,
		MountPath:                   runtimecontract.CompiledCacheDir,
		Layout:                      ArtifactLayout,
		Optional:                    true,
		FetchTimeout:                &metav1.Duration{Duration: timeout},
		Claims:                      id.ArtifactClaims(),
		RequirePlatformVerification: true,
	}
	if d.PullSecret != "" {
		a.PullSecretRef = &corev1.LocalObjectReference{Name: d.PullSecret}
	}
	return a
}

// PlacementArtifact is the artifact a placement prepares: the same
// reference, layout and claims a replica mounts, so a checkout finds the
// prepared entry.
func (d Delivery) PlacementArtifact(id Identity, artifact string) sandboxv1alpha1.PlacementArtifact {
	r := d.ReplicaArtifact(id, artifact, true)
	return sandboxv1alpha1.PlacementArtifact{
		Name:                        r.Name,
		Ref:                         r.Ref,
		Layout:                      r.Layout,
		PullSecretRef:               r.PullSecretRef,
		Claims:                      r.Claims,
		RequirePlatformVerification: r.RequirePlatformVerification,
	}
}

// Serves reports whether any replica of an executor can get a cache: one
// with a warm pool always can, one without only with a cold fetch timeout.
// The cache manager builds no cache for SpinApps it would not serve.
func (d Delivery) Serves(p *executor.Profile) bool {
	return p.SandboxPool != "" || d.ColdFetchTimeout > 0
}

// NoWarmPool is the Skip of a SpinApp whose executor Serves rules out.
func NoWarmPool(executorName string) *Skip {
	return &Skip{Reason: ReasonNoWarmPool,
		Message: fmt.Sprintf("executor %q has no sandbox pool, and the platform gives replicas that boot cold no compiled cache (chart value compiledCache.fetchTimeout is 0s); compiled caches serve warm-pool replicas", executorName)}
}

// Delivers reports whether a replica gets the cache: every replica checked
// out of a warm pool, and cold ones only with a cold fetch timeout.
func (d Delivery) Delivers(sb *sandboxv1alpha1.SwiftSandbox) bool {
	return sb.Spec.PoolRef != nil || d.ColdFetchTimeout > 0
}

// Decorate adds the compiled cache to one replica: the artifact and the
// variables that tell the entrypoint what the cache must have been built
// for. It is applied after the revision fingerprint is computed, so a cache
// becoming available, or going away, never replaces running replicas.
func (d Delivery) Decorate(sb *sandboxv1alpha1.SwiftSandbox, id Identity, artifact string) {
	sb.Spec.Artifacts = append(sb.Spec.Artifacts, d.ReplicaArtifact(id, artifact, sb.Spec.PoolRef != nil))
	sb.Spec.Env = append(sb.Spec.Env,
		corev1.EnvVar{Name: runtimecontract.CompiledCacheSourceEnv, Value: id.Source},
		corev1.EnvVar{Name: runtimecontract.CompiledCacheRuntimeEnv, Value: id.Runtime},
		corev1.EnvVar{Name: runtimecontract.CompiledCacheArchitectureEnv, Value: id.Architecture},
	)
}

// PlacementName is the deterministic name of the placement of record on
// pool, in the record's consumer namespace.
func PlacementName(record, pool string) string {
	h := sha256.Sum256([]byte(pool))
	return record + "-" + hex.EncodeToString(h[:])[:16]
}
