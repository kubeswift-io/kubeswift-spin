package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/metrics"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	_ = c.Write(&m)
	return m.GetCounter().GetValue()
}

func cachedReplica(uid types.UID, created time.Time) sandboxv1alpha1.SwiftSandbox {
	sb := sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "app-" + string(uid), UID: uid, CreationTimestamp: metav1.NewTime(created)}}
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: compiledcache.ArtifactName, Ref: "r@sha256:1"}}
	return sb
}

func TestCacheFailure(t *testing.T) {
	sb := cachedReplica("a", time.Now())
	if cacheFailure(&sb) != "" {
		t.Fatal("a running replica counted as failed")
	}
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxFailed
	sb.Status.Conditions = []metav1.Condition{{Type: sandboxv1alpha1.SwiftSandboxConditionGuestRunning, Status: metav1.ConditionFalse, Reason: "WorkloadFailed"}}
	if cacheFailure(&sb) != "" {
		t.Fatal("a workload failure blamed on the cache")
	}
	sb.Status.Conditions[0].Reason = "ArtifactResolveFailed"
	if cacheFailure(&sb) != "Other" && cacheFailure(&sb) != "ArtifactResolveFailed" {
		t.Fatalf("reason %q", cacheFailure(&sb))
	}
	plain := sb.DeepCopy()
	plain.Spec.Artifacts = nil
	if cacheFailure(plain) != "" {
		t.Fatal("a replica without the cache blamed on it")
	}
}

func TestCountAttachmentsOncePerReplica(t *testing.T) {
	now := time.Now()
	r := &SpinAppReconciler{Now: func() time.Time { return now }}
	old := cachedReplica("old", now.Add(-time.Hour))
	mounted := cachedReplica("m", now.Add(time.Second))
	mounted.Status.Artifacts = []sandboxv1alpha1.SandboxArtifactStatus{{Name: compiledcache.ArtifactName, State: sandboxv1alpha1.SandboxArtifactMounted, Reason: "Cached"}}
	old.Status.Artifacts = mounted.Status.Artifacts
	skipped := cachedReplica("s", now.Add(time.Second))
	skipped.Status.Artifacts = []sandboxv1alpha1.SandboxArtifactStatus{{Name: compiledcache.ArtifactName, State: sandboxv1alpha1.SandboxArtifactSkipped, Reason: "FetchTimeout"}}

	m0 := counterValue(metrics.CompiledCacheAttachments.WithLabelValues("mounted", "Cached"))
	s0 := counterValue(metrics.CompiledCacheAttachments.WithLabelValues("skipped", "FetchTimeout"))
	owned := []sandboxv1alpha1.SwiftSandbox{old, mounted, skipped}
	r.countAttachments("ns/app", owned)
	r.countAttachments("ns/app", owned)
	if d := counterValue(metrics.CompiledCacheAttachments.WithLabelValues("mounted", "Cached")) - m0; d != 1 {
		t.Fatalf("mounted counted %v times (the replica from before the start must not count)", d)
	}
	if d := counterValue(metrics.CompiledCacheAttachments.WithLabelValues("skipped", "FetchTimeout")) - s0; d != 1 {
		t.Fatalf("skipped counted %v times", d)
	}
	r.countAttachments("ns/app", nil)
	if len(r.attached) != 0 {
		t.Fatalf("gone replicas still remembered: %v", r.attached)
	}
}

func TestCacheHoldExpires(t *testing.T) {
	now := time.Now()
	r := &SpinAppReconciler{Now: func() time.Time { return now }}
	failed := cachedReplica("f", now)
	failed.Status.Phase = sandboxv1alpha1.SwiftSandboxFailed
	failed.Status.Artifacts = []sandboxv1alpha1.SandboxArtifactStatus{{Name: compiledcache.ArtifactName, State: sandboxv1alpha1.SandboxArtifactFailed, Reason: "ArtifactResolveFailed"}}
	r.countAttachments("ns/app", []sandboxv1alpha1.SwiftSandbox{failed})
	if _, held := r.cacheHold("ns/app", "r@sha256:1"); !held {
		t.Fatal("no hold after a delivery failure")
	}
	if _, held := r.cacheHold("ns/app", "r@sha256:2"); held {
		t.Fatal("a hold applied to another artifact")
	}
	now = now.Add(cacheHoldPeriod + time.Second)
	if _, held := r.cacheHold("ns/app", "r@sha256:1"); held {
		t.Fatal("the hold did not expire")
	}
	r.forgetAttachments("ns/app")
	if len(r.holds) != 0 {
		t.Fatal("forget kept the hold")
	}
}

func TestWarmReplicasGetTheCacheOnlyFromAPreparedPool(t *testing.T) {
	r := &SpinAppReconciler{CacheDelivery: compiledcache.Delivery{ColdFetchTimeout: time.Second}}
	id := compiledcache.Identity{Namespace: "ns", Source: "r/a@sha256:1", Runtime: "r/rt@sha256:2", Architecture: "amd64", Repository: "r/c/ns/wasmtime-cache"}
	cv := cacheView{known: true, cond: &metav1.Condition{}, id: &id, artifact: "r/c/ns/wasmtime-cache@sha256:3", preparedPools: map[string]bool{"ready": true}}
	warm := func(pool string) *sandboxv1alpha1.SwiftSandbox {
		sb := &sandboxv1alpha1.SwiftSandbox{}
		sb.Spec.PoolRef = &corev1.LocalObjectReference{Name: pool}
		return sb
	}
	if sb := warm("preparing"); r.decorateCache(sb, cv) != "unplaced" || len(sb.Spec.Artifacts) != 0 {
		t.Fatal("a warm replica got a cache its pool's nodes do not hold")
	}
	if sb := warm("ready"); r.decorateCache(sb, cv) != "hit" || len(sb.Spec.Artifacts) != 1 {
		t.Fatal("a warm replica of a prepared pool got no cache")
	}
}

func TestCacheStuck(t *testing.T) {
	now := time.Now()
	stuck := func() *sandboxv1alpha1.SwiftSandbox {
		sb := &sandboxv1alpha1.SwiftSandbox{}
		sb.Spec.PoolRef = &corev1.LocalObjectReference{Name: "warm"}
		sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: compiledcache.ArtifactName}}
		sb.Status.Phase = sandboxv1alpha1.SwiftSandboxRunning
		sb.Status.Artifacts = []sandboxv1alpha1.SandboxArtifactStatus{{Name: compiledcache.ArtifactName, State: sandboxv1alpha1.SandboxArtifactSkipped, Reason: "NotOnNode"}}
		sb.Status.Conditions = []metav1.Condition{
			{Type: sandboxv1alpha1.SwiftSandboxConditionGuestRunning, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(now.Add(-2 * time.Minute))},
			{Type: sandboxv1alpha1.SwiftSandboxConditionWorkloadReady, Status: metav1.ConditionFalse, Reason: "Probing"},
		}
		return sb
	}
	if !cacheStuck(stuck(), now) {
		t.Fatal("a replica not ready two minutes after its cache was skipped is stuck")
	}
	cases := map[string]func(*sandboxv1alpha1.SwiftSandbox){
		"ready": func(sb *sandboxv1alpha1.SwiftSandbox) { sb.Status.Conditions[1].Status = metav1.ConditionTrue },
		"mounted": func(sb *sandboxv1alpha1.SwiftSandbox) {
			sb.Status.Artifacts[0].State = sandboxv1alpha1.SandboxArtifactMounted
		},
		"cold":         func(sb *sandboxv1alpha1.SwiftSandbox) { sb.Spec.PoolRef = nil },
		"no readiness": func(sb *sandboxv1alpha1.SwiftSandbox) { sb.Status.Conditions = sb.Status.Conditions[:1] },
		"recently checked": func(sb *sandboxv1alpha1.SwiftSandbox) {
			sb.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(-10 * time.Second))
		},
	}
	for name, mutate := range cases {
		sb := stuck()
		mutate(sb)
		if cacheStuck(sb, now) {
			t.Errorf("%s: counted as stuck", name)
		}
	}
	recent := stuck()
	recent.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(-10 * time.Second))
	if d := cacheStuckRecheck([]sandboxv1alpha1.SwiftSandbox{*recent}, now); d < 49*time.Second || d > 52*time.Second {
		t.Fatalf("recheck in %s, want about 51s", d)
	}

	r := &SpinAppReconciler{Now: func() time.Time { return now }}
	s := stuck()
	s.Spec.Artifacts[0].Ref = "r@sha256:1"
	if got := r.countAttachments("ns/app", []sandboxv1alpha1.SwiftSandbox{*s}); len(got) != 1 {
		t.Fatalf("stuck replicas %d", len(got))
	}
	if h, held := r.cacheHold("ns/app", "r@sha256:1"); !held || h.reason != reasonNotReadyAfterSkip {
		t.Fatalf("no hold after a stuck replica: %+v", h)
	}
}
