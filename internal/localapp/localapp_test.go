package localapp

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

var (
	digestA = "sha256:" + strings.Repeat("a", 64)
	digestB = "sha256:" + strings.Repeat("b", 64)
	runtime = "ghcr.io/kubeswift-io/kubeswift-spin-runtime@sha256:" + strings.Repeat("c", 64)
)

func settings() Settings {
	return Settings{Enabled: true, Runtimes: []string{runtime}, MaxPlacements: DefaultMaxPlacements,
		MaxNewPerHour: DefaultMaxNewPerHour, Retention: DefaultRetention}
}

func profile() *executor.Profile {
	return &executor.Profile{ExecutorName: "kubeswift", LocalApplication: true, SandboxPool: "warm", RuntimeImage: runtime}
}

func spinApp(image string) *spinv1alpha1.SpinApp {
	return &spinv1alpha1.SpinApp{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "hello"},
		Spec: spinv1alpha1.SpinAppSpec{Image: image}}
}

func input(mut func(*Input)) Input {
	in := Input{App: spinApp("registry.example.com/apps/hello@" + digestA), Profile: profile(),
		Settings: settings(), NamespaceEnabled: true, Supported: true}
	if mut != nil {
		mut(&in)
	}
	return in
}

func TestDecideEligible(t *testing.T) {
	k, skip := Decide(input(nil))
	if skip != nil {
		t.Fatalf("skip %+v", skip)
	}
	want := Key{Namespace: "team", Pool: "warm", Ref: "registry.example.com/apps/hello@" + digestA}
	if *k != want || k.Digest() != digestA {
		t.Fatalf("key %+v", k)
	}
	// A tag next to the digest is dropped; the repository is never changed.
	k, _ = Decide(input(func(in *Input) { in.App.Spec.Image = "registry.example.com/apps/hello:v1@" + digestA }))
	if k.Ref != "registry.example.com/apps/hello@"+digestA {
		t.Fatalf("canonical ref %s", k.Ref)
	}
	k, _ = Decide(input(func(in *Input) {
		in.App.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "pull"}}
	}))
	if k.PullSecret != "pull" {
		t.Fatalf("pull secret %+v", k)
	}
	// docker.io references are normalized before the registry list check.
	k, skip = Decide(input(func(in *Input) {
		in.App.Spec.Image = "docker.io/library/app@" + digestA
		in.Settings.Registries = []string{"index.docker.io"}
	}))
	if skip != nil || k.Ref != "index.docker.io/library/app@"+digestA {
		t.Fatalf("docker hub: %+v %+v", k, skip)
	}
}

func TestDecideReasons(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Input)
		want string
	}{
		{"not configured", func(in *Input) { in.Settings.Enabled = false }, ReasonNotConfigured},
		{"namespace", func(in *Input) { in.NamespaceEnabled = false }, ReasonNamespaceNotEnabled},
		{"unsupported", func(in *Input) { in.Supported = false }, ReasonUnsupported},
		{"no pool", func(in *Input) { in.Profile.SandboxPool = "" }, ReasonNoWarmPool},
		{"tag", func(in *Input) { in.App.Spec.Image = "registry.example.com/apps/hello:v1" }, ReasonImageNotPinned},
		{"uppercase digest", func(in *Input) { in.App.Spec.Image = "registry.example.com/a@sha256:" + strings.Repeat("A", 64) }, ReasonImageNotPinned},
		{"too long", func(in *Input) {
			in.App.Spec.Image = "registry.example.com/" + strings.Repeat("a", 500) + "@" + digestA
		}, ReasonImageNotPinned},
		{"localhost", func(in *Input) { in.App.Spec.Image = "localhost:5000/a@" + digestA }, ReasonRegistryNotEligible},
		{"private ip", func(in *Input) { in.App.Spec.Image = "10.1.2.3:5000/a@" + digestA }, ReasonRegistryNotEligible},
		{".localhost", func(in *Input) { in.App.Spec.Image = "registry.dev.localhost/a@" + digestA }, ReasonRegistryNotEligible},
		{"not listed", func(in *Input) { in.Settings.Registries = []string{"other.example.com"} }, ReasonRegistryNotEligible},
		{"two pull secrets", func(in *Input) {
			in.App.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "a"}, {Name: "b"}}
		}, ReasonPullSecrets},
		{"runtime credential would be used", func(in *Input) { in.Profile.RuntimeImagePullSecret = "rt" }, ReasonPullSecrets},
		{"components", func(in *Input) { in.App.Spec.Components = []string{"a"} }, ReasonComponentsSelected},
		{"runtime not allowed", func(in *Input) { in.Settings.Runtimes = []string{"other@" + digestB} }, ReasonRuntimeNotAllowed},
		{"runtime tag", func(in *Input) { in.Profile.RuntimeImage = "ghcr.io/x/runtime:v1" }, ReasonRuntimeNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k, skip := Decide(input(c.mut))
			if k != nil || skip == nil || skip.Reason != c.want {
				t.Fatalf("got %+v %+v, want %s", k, skip, c.want)
			}
		})
	}
}

func TestDecideNotRequested(t *testing.T) {
	for name, mut := range map[string]func(*Input){
		"executor off": func(in *Input) { in.Profile.LocalApplication = false },
		"opted out": func(in *Input) {
			in.App.Annotations = map[string]string{executor.AnnLocalApplication: executor.LocalApplicationDisabled}
		},
		"invalid value": func(in *Input) { in.App.Annotations = map[string]string{executor.AnnLocalApplication: "on"} },
	} {
		if k, skip := Decide(input(mut)); k != nil || skip != nil {
			t.Errorf("%s: %+v %+v", name, k, skip)
		}
	}
}

func TestPlaintextHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "localhost:5000": true, "reg.localhost": true, "reg.localhost:443": true,
		"reg.local": false, "e2e-registry.ns.svc.cluster.local:5000": false, "REG.LOCALHOST": true, "127.0.0.1:5000": true, "[::1]:5000": true, "10.0.0.1": true,
		"172.16.4.5:443": true, "192.168.1.1": true, "169.254.1.1": true, "[fd00::1]:5000": true, "0.0.0.0": true,
		"ghcr.io": false, "registry.example.com:5000": false, "8.8.8.8": false, "index.docker.io": false,
		"registry.localdomain": false, "local.example.com": false,
	} {
		if got := plaintextHost(host); got != want {
			t.Errorf("%s: %v, want %v", host, got, want)
		}
	}
}

func TestPlacementAndOwnership(t *testing.T) {
	k := Key{Namespace: "team", Pool: "warm", Ref: "registry.example.com/apps/hello@" + digestA, PullSecret: "pull"}
	if !strings.HasPrefix(k.PlacementName(), PlacementPrefix) || len(k.PlacementName()) != len(PlacementPrefix)+20 {
		t.Fatalf("name %s", k.PlacementName())
	}
	if k.Hash() == (Key{Namespace: "team", Pool: "warm", Ref: k.Ref}).Hash() {
		t.Fatal("pull secret not in the key")
	}
	pl := Placement(k)
	if !Owned(pl, k) {
		t.Fatal("own placement not owned")
	}
	// API defaulting or later fields do not orphan it.
	defaulted := pl.DeepCopy()
	defaulted.Spec.Artifacts[0].Layout = ""
	if !Owned(defaulted, k) {
		t.Fatal("defaulted layout orphans the placement")
	}
	for name, mut := range map[string]func(*sandboxv1alpha1.SwiftArtifactPlacement){
		"no label":      func(p *sandboxv1alpha1.SwiftArtifactPlacement) { delete(p.Labels, labelComponent) },
		"no annotation": func(p *sandboxv1alpha1.SwiftArtifactPlacement) { delete(p.Annotations, AnnKey) },
		"other ref": func(p *sandboxv1alpha1.SwiftArtifactPlacement) {
			p.Spec.Artifacts[0].Ref = "other.example.com/x@" + digestA
		},
		"other pool":   func(p *sandboxv1alpha1.SwiftArtifactPlacement) { p.Spec.PoolRef.Name = "cold" },
		"other secret": func(p *sandboxv1alpha1.SwiftArtifactPlacement) { p.Spec.Artifacts[0].PullSecretRef = nil },
		"claims": func(p *sandboxv1alpha1.SwiftArtifactPlacement) {
			p.Spec.Artifacts[0].Claims = map[string]string{"a": "b"}
		},
		"two artifacts": func(p *sandboxv1alpha1.SwiftArtifactPlacement) {
			p.Spec.Artifacts = append(p.Spec.Artifacts, p.Spec.Artifacts[0])
		},
		"other name": func(p *sandboxv1alpha1.SwiftArtifactPlacement) { p.Name = "kss-app-x" },
		"unpacked":   func(p *sandboxv1alpha1.SwiftArtifactPlacement) { p.Spec.Artifacts[0].Layout = "unpacked" },
		"verify claimed": func(p *sandboxv1alpha1.SwiftArtifactPlacement) {
			p.Spec.Artifacts[0].RequirePlatformVerification = true
		},
	} {
		c := pl.DeepCopy()
		mut(c)
		if Owned(c, k) {
			t.Errorf("%s: still owned", name)
		}
	}
	// Prepared needs the current generation.
	pl.Generation = 2
	pl.Status.ObservedGeneration = 2
	pl.Status.Conditions = []metav1.Condition{{Type: sandboxv1alpha1.PlacementConditionPrepared, Status: metav1.ConditionTrue, ObservedGeneration: 2}}
	if !Prepared(pl, k) {
		t.Fatal("prepared placement not prepared")
	}
	pl.Status.ObservedGeneration = 1
	if Prepared(pl, k) {
		t.Fatal("stale status counted as prepared")
	}
}

func TestDecorate(t *testing.T) {
	k := Key{Namespace: "team", Pool: "warm", Ref: "registry.example.com/apps/hello@" + digestA}
	sb := &sandboxv1alpha1.SwiftSandbox{}
	Decorate(sb, k)
	a := sb.Spec.Artifacts[0]
	if a.Name != ArtifactName || a.Ref != k.Ref || a.MountPath != runtimecontract.LocalAppDir || a.Layout != "oci" ||
		!a.Optional || a.FetchTimeout == nil || a.FetchTimeout.Duration != 0 || a.PullSecretRef != nil ||
		len(a.Claims) != 0 || a.RequirePlatformVerification {
		t.Fatalf("artifact %+v", a)
	}
	if e := sb.Spec.Env[0]; e.Name != runtimecontract.LocalAppDigestEnv || e.Value != digestA {
		t.Fatalf("env %+v", sb.Spec.Env)
	}
}

func TestSelect(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	key := func(i int) Key {
		return Key{Namespace: "team", Pool: "warm", Ref: "registry.example.com/a@sha256:" + strings.Repeat(string(rune('a'+i)), 64)}
	}
	s := settings()
	s.MaxPlacements = 3
	s.MaxNewPerHour = 2
	old := now.Add(-2 * time.Hour)

	// Order: oldest SpinApp first; the hourly limit stops new placements.
	wants := []Want{{key(0), now.Add(-time.Minute), "b"}, {key(1), now.Add(-time.Hour), "a"}, {key(2), now.Add(-time.Second), "c"}}
	sel := Select(wants, nil, now, s)
	if len(sel.Keep) != 2 || sel.Keep[0] != key(1) || sel.Keep[1] != key(0) || len(sel.Limited) != 1 || sel.Limited[0] != key(2) {
		t.Fatalf("hourly: %+v", sel)
	}
	// Existing placements need no new room; the count limit applies.
	existing := []Existing{{Key: key(0), Created: old}, {Key: key(1), Created: old}, {Key: key(2), Created: old}}
	wants = append(wants, Want{key(3), now, "d"})
	sel = Select(wants, existing, now, s)
	if len(sel.Keep) != 3 || len(sel.Limited) != 1 || sel.Limited[0] != key(3) {
		t.Fatalf("count: %+v", sel)
	}
	// An unwanted placement is marked and retained while there is room.
	sel = Select([]Want{{key(0), old, "a"}}, []Existing{{Key: key(0), Created: old}, {Key: key(1), Created: old}}, now, s)
	if len(sel.NewlyUnwanted) != 1 || sel.NewlyUnwanted[0] != key(1) || len(sel.Retain) != 1 || len(sel.Delete) != 0 {
		t.Fatalf("retain: %+v", sel)
	}
	// Retention over: deleted.
	since := now.Add(-25 * time.Hour)
	sel = Select(nil, []Existing{{Key: key(1), Created: old, UnwantedSince: &since}}, now, s)
	if len(sel.Delete) != 1 || len(sel.Retain) != 0 {
		t.Fatalf("expired: %+v", sel)
	}
	// Retained placements yield to wanted ones at the limit.
	recently := now.Add(-time.Hour)
	sel = Select([]Want{{key(0), old, "a"}, {key(1), old, "b"}, {key(2), old, "c"}},
		[]Existing{{Key: key(0), Created: old}, {Key: key(1), Created: old}, {Key: key(2), Created: old}, {Key: key(3), Created: old, UnwantedSince: &recently}},
		now, s)
	if len(sel.Keep) != 3 || len(sel.Delete) != 1 || sel.Delete[0] != key(3) {
		t.Fatalf("yield: %+v", sel)
	}
	// Deterministic: the same input gives the same output.
	a, b := Select(wants, existing, now, s), Select(wants, existing, now, s)
	if len(a.Keep) != len(b.Keep) || a.Keep[0] != b.Keep[0] {
		t.Fatal("not deterministic")
	}
}

func TestSettingsValidate(t *testing.T) {
	if err := (Settings{}).Validate(); err != nil {
		t.Fatalf("disabled: %v", err)
	}
	if err := settings().Validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for name, mut := range map[string]func(*Settings){
		"no runtimes": func(s *Settings) { s.Runtimes = nil },
		"tag runtime": func(s *Settings) { s.Runtimes = []string{"ghcr.io/x:v1"} },
		"zero limit":  func(s *Settings) { s.MaxPlacements = 0 },
	} {
		s := settings()
		mut(&s)
		if s.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
