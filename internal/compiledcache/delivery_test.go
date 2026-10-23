package compiledcache

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"

	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

func deliveryIdentity() Identity {
	return Identity{Namespace: "team-a", Source: "registry.example.com/app@sha256:" + strings.Repeat("1", 64),
		Runtime: "registry.example.com/rt@sha256:" + strings.Repeat("2", 64), Architecture: "amd64",
		Repository: "registry.example.com/c/team-a/wasmtime-cache"}
}

func TestReplicaArtifact(t *testing.T) {
	d := Delivery{PullSecret: "cache-read", ColdFetchTimeout: 3 * time.Second}
	id := deliveryIdentity()
	art := id.Repository + "@sha256:" + strings.Repeat("3", 64)

	cold := d.ReplicaArtifact(id, art, false)
	if cold.FetchTimeout.Duration != 3*time.Second {
		t.Fatalf("cold fetchTimeout %s", cold.FetchTimeout.Duration)
	}
	// A warm-pool replica uses the cache only if its node holds it.
	warm := d.ReplicaArtifact(id, art, true)
	if warm.FetchTimeout == nil || warm.FetchTimeout.Duration != 0 {
		t.Fatalf("warm fetchTimeout %v", warm.FetchTimeout)
	}
	for _, a := range []sandboxv1alpha1.SandboxArtifact{cold, warm} {
		if a.Name != ArtifactName || a.Ref != art || a.MountPath != runtimecontract.CompiledCacheDir || a.Layout != ArtifactLayout ||
			!a.Optional || !a.RequirePlatformVerification || a.PullSecretRef.Name != "cache-read" {
			t.Fatalf("artifact %+v", a)
		}
		if len(a.Claims) != 5 || a.Claims[ClaimNamespace] != "team-a" || a.Claims[ClaimSource] != id.Source ||
			a.Claims[ClaimRuntime] != id.Runtime || a.Claims[ClaimArchitecture] != "amd64" || a.Claims[ClaimKind] != KindWasmtimeCache {
			t.Fatalf("claims %v", a.Claims)
		}
	}
	if a := (Delivery{}).ReplicaArtifact(id, art, true); a.PullSecretRef != nil {
		t.Fatal("a pull Secret was named without one configured")
	}
}

func TestPlacementArtifactMatchesTheReplica(t *testing.T) {
	d := Delivery{PullSecret: "cache-read", ColdFetchTimeout: time.Second}
	id := deliveryIdentity()
	art := id.Repository + "@sha256:" + strings.Repeat("3", 64)
	p, r := d.PlacementArtifact(id, art), d.ReplicaArtifact(id, art, true)
	if p.Name != r.Name || p.Ref != r.Ref || p.Layout != r.Layout || p.PullSecretRef.Name != r.PullSecretRef.Name ||
		p.RequirePlatformVerification != r.RequirePlatformVerification || len(p.Claims) != len(r.Claims) {
		t.Fatalf("placement %+v, replica %+v", p, r)
	}
	for k, v := range r.Claims {
		if p.Claims[k] != v {
			t.Fatalf("claim %s: %q, %q", k, p.Claims[k], v)
		}
	}
}

func TestDecorate(t *testing.T) {
	d := Delivery{ColdFetchTimeout: time.Second}
	id := deliveryIdentity()
	art := id.Repository + "@sha256:" + strings.Repeat("3", 64)
	sb := &sandboxv1alpha1.SwiftSandbox{}
	sb.Spec.PoolRef = &corev1.LocalObjectReference{Name: "warm"}
	sb.Spec.Env = []corev1.EnvVar{{Name: "SPIN_VARIABLE_X", Value: "1"}}
	d.Decorate(sb, id, art)
	if len(sb.Spec.Artifacts) != 1 || sb.Spec.Artifacts[0].FetchTimeout.Duration != 0 {
		t.Fatalf("artifacts %+v", sb.Spec.Artifacts)
	}
	env := map[string]string{}
	for _, e := range sb.Spec.Env {
		env[e.Name] = e.Value
	}
	if env["SPIN_VARIABLE_X"] != "1" || env[runtimecontract.CompiledCacheSourceEnv] != id.Source ||
		env[runtimecontract.CompiledCacheRuntimeEnv] != id.Runtime || env[runtimecontract.CompiledCacheArchitectureEnv] != "amd64" {
		t.Fatalf("env %v", env)
	}
}

func TestDeliveryValidate(t *testing.T) {
	for _, d := range []time.Duration{-time.Second, MaxColdFetchTimeout + time.Second} {
		if err := (Delivery{ColdFetchTimeout: d}).Validate(); err == nil {
			t.Fatalf("fetch timeout %s accepted", d)
		}
	}
	if err := (Delivery{ColdFetchTimeout: 0}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPlacementName(t *testing.T) {
	rec := "spincc-" + strings.Repeat("a", 32)
	a, b := PlacementName(rec, "warm-a"), PlacementName(rec, "warm-b")
	if a == b || !strings.HasPrefix(a, rec+"-") || len(a) > 63 || PlacementName(rec, "warm-a") != a {
		t.Fatalf("names %s %s", a, b)
	}
}

func TestTruncateKeepsRunes(t *testing.T) {
	s := strings.Repeat("a", 8) + "ééé"
	for n := 4; n < len(s); n++ {
		if got := Truncate(s, n); len(got) > n || !utf8.ValidString(got) {
			t.Fatalf("Truncate(%d) = %q", n, got)
		}
	}
}
