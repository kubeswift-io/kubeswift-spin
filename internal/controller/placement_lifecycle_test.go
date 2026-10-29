package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/localapp"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

// startFeatures runs the controller and the cache manager with the given
// features enabled, as the chart would after a values change.
func startFeatures(t *testing.T, s compiledcache.Settings, local, cache bool) *harness {
	t.Helper()
	o := harnessOpts{cacheManager: true}
	if local {
		o.localApplication = laSettings()
	}
	if cache {
		o.compiledCache = s
	}
	return startManager(t, o)
}

// placements returns the live placements of ns by name, with their UIDs.
func placements(t *testing.T, ns string) map[string]types.UID {
	t.Helper()
	var l sandboxv1alpha1.SwiftArtifactPlacementList
	if err := testClient.List(context.Background(), &l, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	out := map[string]types.UID{}
	for _, pl := range l.Items {
		if pl.DeletionTimestamp.IsZero() {
			out[pl.Name] = pl.UID
		}
	}
	return out
}

func sameNames(got map[string]types.UID, want []string) error {
	var names []string
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	w := append([]string{}, want...)
	sort.Strings(w)
	if strings.Join(names, ",") != strings.Join(w, ",") {
		return fmt.Errorf("placements %v, want %v", names, w)
	}
	return nil
}

func expectPlacements(t *testing.T, ns string, want ...string) map[string]types.UID {
	t.Helper()
	var got map[string]types.UID
	eventually(t, fmt.Sprintf("placements %v", want), func() error {
		got = placements(t, ns)
		return sameNames(got, want)
	})
	return got
}

// keepPlacements checks that the placements stay the same objects.
func keepPlacements(t *testing.T, ns string, want map[string]types.UID) {
	t.Helper()
	consistently(t, "placements kept", 3*time.Second, func() error {
		got := placements(t, ns)
		for n, uid := range want {
			if got[n] != uid {
				return fmt.Errorf("placement %s replaced or deleted", n)
			}
		}
		return nil
	})
}

// trustRecord makes the namespace's only record trusted, as the compiler
// and the signer would, and returns its name. It waits for the cache
// manager to record its compile attempt and then completes that attempt: a
// status written before the attempt is recorded would be replaced by it.
func trustRecord(t *testing.T, s compiledcache.Settings, ns string) string {
	t.Helper()
	var name string
	var uid types.UID
	eventually(t, "one record with a recorded compile attempt", func() error {
		recs := listRecords(t, s.Namespace, ns)
		if len(recs) != 1 {
			return fmt.Errorf("%d records", len(recs))
		}
		name = recs[0].Name
		c := recs[0].Status.Compile
		if c == nil || c.Sandbox == nil {
			return fmt.Errorf("no compile attempt recorded")
		}
		uid = c.Sandbox.UID
		return nil
	})
	art := s.Repository + "/" + ns + "/wasmtime-cache@" + ccDigestB
	now := time.Now().UTC().Format(time.RFC3339)
	patchRecordStatus(t, s.Namespace, name, map[string]any{
		"compile":   map[string]any{"artifact": art, "finishedAt": now},
		"signature": map[string]any{"artifact": art, "keyID": "k1", "sandboxUID": string(uid), "signedAt": now},
	})
	return name
}

// TestPlacementLifecycleFeatureMatrix switches local applications and
// compiled caches on and off, one manager per chart configuration, for one
// SpinApp that asks for both on a warm pool. A placement exists exactly
// while its feature is enabled and wants it; disabling one feature never
// touches the other's placement; a restart keeps what is wanted.
func TestPlacementLifecycleFeatureMatrix(t *testing.T) {
	s := cacheSettings(t)
	ns := laNamespace(t, true)
	setNamespaceLabel(t, ns, compiledcache.NamespaceEnabled)
	mustCreate(t, managedExecutor(ns, "kubeswift", map[string]string{
		executor.AnnRuntimeImage: ccRuntime, executor.AnnSandboxPool: "spin-pool",
		executor.AnnLocalApplication: executor.LocalApplicationEnabled, executor.AnnCompiledCache: executor.CompiledCacheEnabled,
	}), laApp(ns, "hello", laAppRef))
	app := localapp.Key{Namespace: ns, Pool: "spin-pool", Ref: laAppRef}.PlacementName()

	step := func(name string, local, cache bool, f func()) {
		t.Logf("step: %s (local applications %v, compiled caches %v)", name, local, cache)
		h := startFeatures(t, s, local, cache)
		f()
		h.stop()
	}

	step("both off", false, false, func() { expectPlacements(t, ns) })

	var first types.UID
	step("local applications enabled", true, false, func() {
		first = expectPlacements(t, ns, app)[app]
		preparePlacement(t, ns)
		expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionTrue, localapp.ReasonAttached)
	})
	step("controller restart", true, false, func() {
		keepPlacements(t, ns, map[string]types.UID{app: first})
	})
	step("local applications disabled", false, false, func() { expectPlacements(t, ns) })
	step("local applications enabled again", true, false, func() {
		if expectPlacements(t, ns, app)[app] == first {
			t.Fatal("the placement was not recreated")
		}
		// A new placement: nothing of the old one's Prepared state remains.
		expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionFalse, localapp.ReasonPlacing)
	})

	var cache string
	step("compiled caches only", false, true, func() {
		cache = compiledcache.PlacementName(trustRecord(t, s, ns), "spin-pool")
		expectPlacements(t, ns, cache)
	})
	var both map[string]types.UID
	step("both enabled", true, true, func() { both = expectPlacements(t, ns, app, cache) })
	step("local applications disabled, compiled caches kept", false, true, func() {
		expectPlacements(t, ns, cache)
		keepPlacements(t, ns, map[string]types.UID{cache: both[cache]})
	})
	step("both enabled again", true, true, func() { both = expectPlacements(t, ns, app, cache) })
	step("compiled caches disabled, local applications kept", true, false, func() {
		expectPlacements(t, ns, app)
		keepPlacements(t, ns, map[string]types.UID{app: both[app]})
	})
	step("compiled caches enabled again", true, true, func() {
		// The record is still trusted, so its placement comes back.
		expectPlacements(t, ns, app, cache)
	})
	step("both disabled", false, false, func() { expectPlacements(t, ns) })
}

// TestPlacementCleanupOfLeftovers starts with placements that no component
// deleted: a v0.3.0 application placement whose pool is gone (PoolNotFound)
// and a compiled-cache placement of a record that no longer exists. With
// both features disabled they are deleted at start; objects kubeswift-spin
// did not create, even with its labels, are never deleted.
func TestPlacementCleanupOfLeftovers(t *testing.T) {
	ns := newNamespace(t)
	orphan := localapp.Placement(localapp.Key{Namespace: ns, Pool: "deleted-pool", Ref: laAppRef})
	stale := cachePlacement(ns, "spincc-gone", "deleted-pool")
	unlabelled := localapp.Placement(localapp.Key{Namespace: ns, Pool: "other", Ref: laAppRef})
	unlabelled.Labels = nil
	forged := localapp.Placement(localapp.Key{Namespace: ns, Pool: "spin-pool", Ref: laAppRef})
	forged.Name = "kss-app-forged"
	noUID := cachePlacement(ns, "spincc-x", "spin-pool")
	delete(noUID.Annotations, compiledcache.AnnRecordUID)
	mustCreate(t, orphan, stale, unlabelled, forged, noUID)
	pl := orphan.DeepCopy()
	eventually(t, "orphan reports PoolNotFound", func() error {
		if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(orphan), pl); err != nil {
			return err
		}
		return testClient.Status().Update(context.Background(), poolNotFound(pl))
	})

	startManager(t, harnessOpts{})
	expectPlacements(t, ns, unlabelled.Name, forged.Name, noUID.Name)
	consistently(t, "foreign placements kept", 3*time.Second, func() error {
		return sameNames(placements(t, ns), []string{unlabelled.Name, forged.Name, noUID.Name})
	})

	// One that appears while the controller runs goes too.
	late := localapp.Placement(localapp.Key{Namespace: ns, Pool: "late-pool", Ref: laAppRef})
	mustCreate(t, late)
	expectPlacements(t, ns, unlabelled.Name, forged.Name, noUID.Name)
}

// startCleanup runs only the placement cleanup of an old controller for
// which local applications are disabled.
func startCleanup(t *testing.T, minAge time.Duration) func() {
	t.Helper()
	mgr, err := ctrl.NewManager(testCfg, ctrl.Options{Scheme: testScheme, Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)}})
	if err != nil {
		t.Fatal(err)
	}
	r := &PlacementCleanupReconciler{Client: mgr.GetClient(), Cleanup: PlacementCleanup{LocalApplications: true}, MinAge: minAge}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("cleanup manager: %v", err)
		}
	}()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

// TestPlacementCleanupDuringRollout runs a new cache manager that wants an
// application placement beside an old controller that still cleans up
// after local applications, as while a rollout that enables the feature
// runs. Without a minimum age they delete and recreate the placement in a
// loop; with it the placement stays.
func TestPlacementCleanupDuringRollout(t *testing.T) {
	startManager(t, harnessOpts{localApplication: laSettings(), cacheManager: true})
	ns := laNamespace(t, true)
	mustCreate(t, laExecutor(ns, true), laApp(ns, "hello", laAppRef))
	app := localapp.Key{Namespace: ns, Pool: "spin-pool", Ref: laAppRef}.PlacementName()
	first := expectPlacements(t, ns, app)[app]

	stop := startCleanup(t, DefaultPlacementCleanupMinAge)
	keepPlacements(t, ns, map[string]types.UID{app: first})
	stop()

	// The loop the minimum age prevents.
	startCleanup(t, 0)
	eventually(t, "placement recreated by the cache manager after the cleanup deleted it", func() error {
		if uid := placements(t, ns)[app]; uid == "" || uid == first {
			return fmt.Errorf("placement %s uid %q", app, uid)
		}
		return nil
	})
}
