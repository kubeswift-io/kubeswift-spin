package controller

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

// A placement younger than MinAge is kept and looked at again once it is
// old enough: an old controller still running beside a new cache manager
// (a rollout that enables the feature) must not delete what it creates.
func TestPlacementCleanupWaitsForMinAge(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	pl := localapp.Placement(localapp.Key{Namespace: "team", Pool: "warm", Ref: laAppRef})
	pl.CreationTimestamp = metav1.NewTime(now.Add(-time.Minute))
	c := fake.NewClientBuilder().WithScheme(scheme.New()).WithObjects(pl).Build()
	r := &PlacementCleanupReconciler{Client: c, Cleanup: PlacementCleanup{LocalApplications: true}, MinAge: 5 * time.Minute,
		Now: func() time.Time { return now }}
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(pl)}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != 4*time.Minute+time.Second {
		t.Fatalf("requeue after %v", res.RequeueAfter)
	}
	if err := c.Get(context.Background(), req.NamespacedName, &sandboxv1alpha1.SwiftArtifactPlacement{}); err != nil {
		t.Fatalf("young placement deleted: %v", err)
	}
	now = now.Add(4*time.Minute + time.Second)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), req.NamespacedName, &sandboxv1alpha1.SwiftArtifactPlacement{}); err == nil {
		t.Fatal("placement kept past MinAge")
	}
}

// The controller caches application placements in every configuration
// and compiled-cache placements only while compiled caches are disabled.
func TestPlacementSelector(t *testing.T) {
	app := map[string]string{translate.LabelManagedBy: translate.ManagedByValue, compiledcache.LabelComponent: localapp.ComponentPlacement}
	cache := map[string]string{translate.LabelManagedBy: translate.ManagedByValue, compiledcache.LabelComponent: compiledcache.ComponentPlacement}
	foreign := map[string]string{translate.LabelManagedBy: "someone-else", compiledcache.LabelComponent: localapp.ComponentPlacement}
	compiler := map[string]string{translate.LabelManagedBy: translate.ManagedByValue, compiledcache.LabelComponent: compiledcache.ComponentCompiler}
	for _, tc := range []struct {
		cleanup PlacementCleanup
		cache   bool
	}{
		{PlacementCleanup{}, false},
		{PlacementCleanup{LocalApplications: true}, false},
		{PlacementCleanup{CompiledCaches: true}, true},
		{PlacementCleanup{LocalApplications: true, CompiledCaches: true}, true},
	} {
		sel := PlacementSelector(tc.cleanup)
		if !sel.Matches(labels.Set(app)) || sel.Matches(labels.Set(foreign)) || sel.Matches(labels.Set(compiler)) {
			t.Fatalf("%+v: %s", tc.cleanup, sel)
		}
		if sel.Matches(labels.Set(cache)) != tc.cache {
			t.Fatalf("%+v: compiled-cache placements selected: %v", tc.cleanup, !tc.cache)
		}
	}
}

// A conflict (the placement changed or was replaced since the cache read)
// is looked at again; any other delete error is returned for a retry.
func TestPlacementCleanupDeleteErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		requeue bool
	}{
		"conflict": {apierrors.NewConflict(schema.GroupResource{Resource: "swiftartifactplacements"}, "p", nil), true},
		"server":   {apierrors.NewInternalError(context.DeadlineExceeded), false},
	} {
		t.Run(name, func(t *testing.T) {
			pl := localapp.Placement(localapp.Key{Namespace: "team", Pool: "warm", Ref: laAppRef})
			c := fake.NewClientBuilder().WithScheme(scheme.New()).WithObjects(pl).WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return tc.err },
			}).Build()
			r := &PlacementCleanupReconciler{Client: c, Cleanup: PlacementCleanup{LocalApplications: true}}
			res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(pl)})
			if tc.requeue && (err != nil || res.RequeueAfter == 0) {
				t.Fatalf("result %+v, error %v", res, err)
			}
			if !tc.requeue && err == nil {
				t.Fatal("error not returned")
			}
		})
	}
}
