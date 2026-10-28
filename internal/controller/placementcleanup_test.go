package controller

import (
	"context"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/localapp"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
	"github.com/kubeswift-io/kubeswift-spin/internal/scheme"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

// cachePlacement is a compiled-cache placement as the cache manager creates
// it (internal/cachemanager tests check that compiledcache.IsPlacement
// accepts the real ones).
func cachePlacement(ns, record, pool string) *sandboxv1alpha1.SwiftArtifactPlacement {
	pl := &sandboxv1alpha1.SwiftArtifactPlacement{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: compiledcache.PlacementName(record, pool),
		Labels: map[string]string{translate.LabelManagedBy: translate.ManagedByValue, compiledcache.LabelComponent: compiledcache.ComponentPlacement,
			compiledcache.LabelRecord: record},
		Annotations: map[string]string{compiledcache.AnnRecordUID: "rec-uid"}}}
	pl.Spec.PoolRef.Name = pool
	pl.Spec.Artifacts = []sandboxv1alpha1.PlacementArtifact{{Name: compiledcache.ArtifactName,
		Ref: "registry.example.com/cache/" + ns + "/wasmtime-cache@" + ccDigestB, Layout: compiledcache.ArtifactLayout, RequirePlatformVerification: true}}
	return pl
}

// poolNotFound gives a placement the state KubeSwift reports once its pool
// is gone.
func poolNotFound(pl *sandboxv1alpha1.SwiftArtifactPlacement) *sandboxv1alpha1.SwiftArtifactPlacement {
	pl.Status.Conditions = []metav1.Condition{{Type: sandboxv1alpha1.PlacementConditionPrepared, Status: metav1.ConditionFalse,
		Reason: "PoolNotFound", Message: "SwiftSandboxPool " + pl.Spec.PoolRef.Name + " not found", LastTransitionTime: metav1.Now()}}
	return pl
}

// cleanupObjects are kubeswift-spin's placements of both features, a
// v0.3.0 orphan whose pool is gone, and placements kubeswift-spin did not
// create, some of them carrying its labels.
func cleanupObjects() (app, orphan, cache []string, foreign []string, objs []client.Object) {
	k := localapp.Key{Namespace: "team", Pool: "warm", Ref: laAppRef}
	appPl := localapp.Placement(k)
	orphanPl := poolNotFound(localapp.Placement(localapp.Key{Namespace: "team", Pool: "deleted", Ref: laAppRef}))
	cachePl := cachePlacement("team", "spincc-1", "warm")

	unlabelled := localapp.Placement(localapp.Key{Namespace: "team", Pool: "other", Ref: laAppRef})
	unlabelled.Labels = nil
	renamed := localapp.Placement(k) // the right marks under another name
	renamed.Name = "kss-app-forged"
	wrongKey := localapp.Placement(localapp.Key{Namespace: "team", Pool: "warm2", Ref: laAppRef})
	wrongKey.Annotations[localapp.AnnKey] = k.Hash() // the key of another placement
	otherRef := localapp.Placement(localapp.Key{Namespace: "team", Pool: "warm3", Ref: laAppRef})
	otherRef.Spec.Artifacts[0].Ref = "registry.example.com/apps/other@" + ccDigestB // spec no longer matches the key
	otherManager := localapp.Placement(localapp.Key{Namespace: "team", Pool: "warm4", Ref: laAppRef})
	otherManager.Labels[translate.LabelManagedBy] = "someone-else"
	noUID := cachePlacement("team", "spincc-2", "warm")
	delete(noUID.Annotations, compiledcache.AnnRecordUID)
	cacheRenamed := cachePlacement("team", "spincc-3", "warm")
	cacheRenamed.Name = "spincc-3-forged"
	cacheOtherPool := cachePlacement("team", "spincc-4", "warm")
	cacheOtherPool.Spec.PoolRef.Name = "elsewhere" // the name is derived from another pool

	for _, pl := range []*sandboxv1alpha1.SwiftArtifactPlacement{appPl, orphanPl, cachePl, unlabelled, renamed, wrongKey, otherRef, otherManager, noUID, cacheRenamed, cacheOtherPool} {
		objs = append(objs, pl)
	}
	foreign = []string{unlabelled.Name, renamed.Name, wrongKey.Name, otherRef.Name, otherManager.Name, noUID.Name, cacheRenamed.Name, cacheOtherPool.Name}
	return []string{appPl.Name}, []string{orphanPl.Name}, []string{cachePl.Name}, foreign, objs
}

func TestPlacementCleanupDeletesOnlyDisabledFeaturesPlacements(t *testing.T) {
	app, orphan, cache, foreign, _ := cleanupObjects()
	all := append(append(append(append([]string{}, app...), orphan...), cache...), foreign...)
	without := func(drop ...[]string) []string {
		gone := map[string]bool{}
		for _, d := range drop {
			for _, n := range d {
				gone[n] = true
			}
		}
		var out []string
		for _, n := range all {
			if !gone[n] {
				out = append(out, n)
			}
		}
		return out
	}
	for name, tc := range map[string]struct {
		cleanup PlacementCleanup
		left    []string
	}{
		"local applications disabled": {PlacementCleanup{LocalApplications: true}, without(app, orphan)},
		"compiled caches disabled":    {PlacementCleanup{CompiledCaches: true}, without(cache)},
		"both disabled":               {PlacementCleanup{LocalApplications: true, CompiledCaches: true}, without(app, orphan, cache)},
		"both enabled":                {PlacementCleanup{}, all},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, _, objs := cleanupObjects()
			c := fake.NewClientBuilder().WithScheme(scheme.New()).WithObjects(objs...).
				WithStatusSubresource(&sandboxv1alpha1.SwiftArtifactPlacement{}).Build()
			r := &PlacementCleanupReconciler{Client: c, Cleanup: tc.cleanup}
			for _, o := range objs {
				// Twice: a second event for a deleted placement is a no-op.
				for range 2 {
					if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(o)}); err != nil {
						t.Fatal(err)
					}
				}
			}
			var l sandboxv1alpha1.SwiftArtifactPlacementList
			if err := c.List(context.Background(), &l); err != nil {
				t.Fatal(err)
			}
			var left []string
			for _, pl := range l.Items {
				left = append(left, pl.Name)
			}
			sort.Strings(left)
			want := append([]string{}, tc.left...)
			sort.Strings(want)
			if strings.Join(left, ",") != strings.Join(want, ",") {
				t.Fatalf("left %v, want %v", left, want)
			}
		})
	}
}

func TestPlacementCleanupComponents(t *testing.T) {
	if got := (PlacementCleanup{}).Components(); len(got) != 0 || (PlacementCleanup{}).Any() {
		t.Fatalf("both enabled: %v", got)
	}
	got := PlacementCleanup{LocalApplications: true, CompiledCaches: true}.Components()
	if strings.Join(got, ",") != localapp.ComponentPlacement+","+compiledcache.ComponentPlacement {
		t.Fatalf("both disabled: %v", got)
	}
}

func TestPlacementCleanupIgnoresMissingPlacement(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.New()).Build()
	r := &PlacementCleanupReconciler{Client: c, Cleanup: PlacementCleanup{LocalApplications: true, CompiledCaches: true}}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team", Name: "gone"}}); err != nil {
		t.Fatal(err)
	}
}
