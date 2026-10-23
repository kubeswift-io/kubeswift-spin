package controller

import (
	"testing"
	"time"

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
