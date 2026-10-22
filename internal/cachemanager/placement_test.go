package cachemanager

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

func trustedRecord(pools ...string) *cachev1alpha1.SpinCompiledCache {
	rec := exportedRecord("u1")
	rec.Status.Signature = &cachev1alpha1.SignatureStatus{Artifact: rec.Status.Compile.Artifact, SandboxUID: "u1", SignedAt: &metav1.Time{Time: time.Now()}}
	rec.Status.Pools = pools
	return rec
}

func listPlacements(t *testing.T, c client.Client) []sandboxv1alpha1.SwiftArtifactPlacement {
	t.Helper()
	var l sandboxv1alpha1.SwiftArtifactPlacementList
	if err := c.List(context.Background(), &l); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func TestPlacementPerPoolMatchesTheReplicaArtifact(t *testing.T) {
	rec := trustedRecord("warm-a", "warm-b")
	r, c := newRecordReconciler(t, time.Now(), rec)
	r.Delivery = compiledcache.Delivery{PullSecret: "cache-read", ColdFetchTimeout: 3 * time.Second}
	got := reconcileRecord(t, r, c)

	ps := listPlacements(t, c)
	if len(ps) != 2 {
		t.Fatalf("%d placements, want 2", len(ps))
	}
	id := compiledcache.IdentityOf(rec)
	replica := r.Delivery.ReplicaArtifact(id, rec.Status.Compile.Artifact, true)
	for _, p := range ps {
		if p.Namespace != "team-a" || p.Name != compiledcache.PlacementName(rec.Name, p.Spec.PoolRef.Name) || !ownPlacement(&p, rec.Name) {
			t.Fatalf("placement %s/%s labels %v", p.Namespace, p.Name, p.Labels)
		}
		a := p.Spec.Artifacts[0]
		// A checkout uses a prepared entry only for the same reference,
		// layout and claims.
		if len(p.Spec.Artifacts) != 1 || a.Ref != replica.Ref || a.Layout != replica.Layout || a.PullSecretRef.Name != "cache-read" ||
			!a.RequirePlatformVerification || len(a.Claims) != len(replica.Claims) || a.Claims[compiledcache.ClaimSource] != rec.Spec.Source {
			t.Fatalf("placement artifact %+v, replica %+v", a, replica)
		}
	}
	if len(got.Status.Placements) != 2 || got.Status.Placements[0].Pool != "warm-a" || got.Status.Placements[0].Prepared {
		t.Fatalf("placements status %+v", got.Status.Placements)
	}
	if c := apimeta.FindStatusCondition(got.Status.Conditions, cachev1alpha1.ConditionPlaced); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Placed = %+v before KubeSwift prepared anything", c)
	}
	if c := apimeta.FindStatusCondition(got.Status.Conditions, cachev1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v before placement", c)
	}
}

func TestPlacedOnceKubeSwiftPrepared(t *testing.T) {
	rec := trustedRecord("warm-a")
	r, c := newRecordReconciler(t, time.Now(), rec)
	reconcileRecord(t, r, c)

	ps := listPlacements(t, c)
	p := &ps[0]
	p.Status = sandboxv1alpha1.SwiftArtifactPlacementStatus{ObservedGeneration: p.Generation, TargetNodes: 2, PreparedNodes: 2,
		Conditions: []metav1.Condition{{Type: sandboxv1alpha1.PlacementConditionPrepared, Status: metav1.ConditionTrue, Reason: "Prepared",
			ObservedGeneration: p.Generation, LastTransitionTime: metav1.Now()}}}
	if err := c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got := reconcileRecord(t, r, c)
	if s := got.Status.Placements[0]; !s.Prepared || s.PreparedNodes != 2 || s.TargetNodes != 2 {
		t.Fatalf("placement status %+v", s)
	}
	if c := apimeta.FindStatusCondition(got.Status.Conditions, cachev1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v once every pool is prepared", c)
	}
}

func TestPlacementFailureIsReported(t *testing.T) {
	rec := trustedRecord("warm-a")
	r, c := newRecordReconciler(t, time.Now(), rec)
	reconcileRecord(t, r, c)
	ps := listPlacements(t, c)
	p := &ps[0]
	p.Status = sandboxv1alpha1.SwiftArtifactPlacementStatus{ObservedGeneration: p.Generation, TargetNodes: 1,
		Conditions: []metav1.Condition{{Type: sandboxv1alpha1.PlacementConditionPrepared, Status: metav1.ConditionFalse, Reason: "ClaimMismatch",
			Message: "signature lacks a claim", ObservedGeneration: p.Generation, LastTransitionTime: metav1.Now()}}}
	if err := c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got := reconcileRecord(t, r, c)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, cachev1alpha1.ConditionPlaced)
	if cond == nil || cond.Reason != compiledcache.ReasonPlacementFailed {
		t.Fatalf("Placed = %+v", cond)
	}
}

func TestPlacementsFollowPoolsAndTrust(t *testing.T) {
	rec := trustedRecord("warm-a", "warm-b")
	r, c := newRecordReconciler(t, time.Now(), rec)
	reconcileRecord(t, r, c)

	// A pool no consumer uses any more loses its placement.
	var cur cachev1alpha1.SpinCompiledCache
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(rec), &cur)
	cur.Status.Pools = []string{"warm-b"}
	if err := c.Status().Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	got := reconcileRecord(t, r, c)
	if ps := listPlacements(t, c); len(ps) != 1 || ps[0].Spec.PoolRef.Name != "warm-b" || len(got.Status.Placements) != 1 {
		t.Fatalf("placements %d, status %+v", len(ps), got.Status.Placements)
	}

	// A record that is no longer trusted places nothing.
	got.Status.Signature.Rejected = true
	if err := c.Status().Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = reconcileRecord(t, r, c)
	if ps := listPlacements(t, c); len(ps) != 0 || len(got.Status.Placements) != 0 {
		t.Fatalf("placements %d, status %+v", len(ps), got.Status.Placements)
	}
}

func TestPlacementsGoWithTheRecord(t *testing.T) {
	rec := trustedRecord("warm-a")
	r, c := newRecordReconciler(t, time.Now(), rec)
	reconcileRecord(t, r, c)
	if err := c.Delete(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "kss", Name: "spincc-1"}}); err != nil {
		t.Fatal(err)
	}
	if ps := listPlacements(t, c); len(ps) != 0 {
		t.Fatalf("%d placements outlived their record", len(ps))
	}
}

// A placement name held by an object the cache manager did not create is
// reported, never adopted, updated or deleted.
func TestForeignPlacementIsNotTouched(t *testing.T) {
	rec := trustedRecord("warm-a")
	foreign := &sandboxv1alpha1.SwiftArtifactPlacement{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a",
		Name: compiledcache.PlacementName(rec.Name, "warm-a")}}
	foreign.Spec.PoolRef.Name = "warm-a"
	foreign.Spec.Artifacts = []sandboxv1alpha1.PlacementArtifact{{Name: "x", Ref: "registry.example.com/x@" + digestA}}
	r, c := newRecordReconciler(t, time.Now(), rec, foreign)
	got := reconcileRecord(t, r, c)
	var p sandboxv1alpha1.SwiftArtifactPlacement
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &p); err != nil || p.Spec.Artifacts[0].Name != "x" {
		t.Fatalf("foreign placement changed: %v %+v", err, p.Spec)
	}
	if s := got.Status.Placements[0]; s.Reason != ReasonPlacementConflict {
		t.Fatalf("status %+v", s)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, cachev1alpha1.ConditionPlaced); cond == nil || cond.Reason != compiledcache.ReasonPlacementFailed {
		t.Fatalf("Placed = %+v", cond)
	}
}
