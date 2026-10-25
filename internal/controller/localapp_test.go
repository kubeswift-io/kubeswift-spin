package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/localapp"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

const laAppRef = "registry.example.com/apps/hello@" + ccDigestA

func laSettings() localapp.Settings {
	return localapp.Settings{Enabled: true, Runtimes: []string{ccRuntime}, MaxPlacements: 8, MaxNewPerHour: 10, Retention: time.Hour}
}

// laNamespace creates a namespace an administrator enabled for local
// applications, with a warm pool matching the test executor.
func laNamespace(t *testing.T, enabled bool) string {
	t.Helper()
	ns := newNamespace(t)
	if enabled {
		patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, localapp.NamespaceLabel, localapp.NamespaceEnabled))
		if err := testClient.Patch(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, client.RawPatch(types.MergePatchType, patch)); err != nil {
			t.Fatal(err)
		}
	}
	mustCreate(t, &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "spin-pool"},
		Spec: sandboxv1alpha1.SwiftSandboxPoolSpec{Image: ccRuntime, CPU: 1, Memory: resource.MustParse("512Mi"),
			Network: sandboxv1alpha1.SandboxNetwork{Ports: []sandboxv1alpha1.SandboxPort{{Name: "http-app", Port: 3000}}}},
	})
	return ns
}

func laExecutor(ns string, enabled bool) *spinv1alpha1.SpinAppExecutor {
	ann := map[string]string{executor.AnnRuntimeImage: ccRuntime, executor.AnnSandboxPool: "spin-pool"}
	if enabled {
		ann[executor.AnnLocalApplication] = executor.LocalApplicationEnabled
	}
	return managedExecutor(ns, "kubeswift", ann)
}

func laApp(ns, name, image string) *spinv1alpha1.SpinApp {
	a := spinApp(ns, name, "kubeswift", 1)
	a.Spec.Image = image
	return a
}

// prepare marks the application placement Prepared, as KubeSwift would.
func preparePlacement(t *testing.T, ns string) {
	t.Helper()
	k := localapp.Key{Namespace: ns, Pool: "spin-pool", Ref: laAppRef}
	eventually(t, "placement prepared", func() error {
		var pl sandboxv1alpha1.SwiftArtifactPlacement
		if err := testClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: k.PlacementName()}, &pl); err != nil {
			return err
		}
		pl.Status.ObservedGeneration = pl.Generation
		pl.Status.TargetNodes, pl.Status.PreparedNodes = 1, 1
		pl.Status.Conditions = []metav1.Condition{{Type: sandboxv1alpha1.PlacementConditionPrepared, Status: metav1.ConditionTrue,
			Reason: "Prepared", ObservedGeneration: pl.Generation, LastTransitionTime: metav1.Now()}}
		return testClient.Status().Update(context.Background(), &pl)
	})
}

func TestLocalApplicationAttachesToNewWarmReplicas(t *testing.T) {
	startManager(t, harnessOpts{localApplication: laSettings(), cacheManager: true})
	ns := laNamespace(t, true)
	mustCreate(t, laExecutor(ns, true), laApp(ns, "hello", laAppRef))

	// The cache manager creates the placement; until KubeSwift prepares it,
	// replicas start from the registry.
	expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionFalse, localapp.ReasonPlacing)
	expectSandboxes(t, ns, "hello-0")
	first := getSandbox(t, ns, "hello-0")
	if first.Spec.PoolRef == nil || len(first.Spec.Artifacts) != 0 {
		t.Fatalf("first replica: pool %+v artifacts %+v", first.Spec.PoolRef, first.Spec.Artifacts)
	}

	preparePlacement(t, ns)
	expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionTrue, localapp.ReasonAttached)
	expectEvent(t, ns, "hello", EventLocalApplicationReady)
	updateApp(t, ns, "hello", func(a *spinv1alpha1.SpinApp) { a.Spec.Replicas = 2 })
	expectSandboxes(t, ns, "hello-0", "hello-1")
	second := getSandbox(t, ns, "hello-1")
	if got := getSandbox(t, ns, "hello-0"); got.UID != first.UID || len(got.Spec.Artifacts) != 0 {
		t.Fatal("the existing replica was replaced or changed")
	}
	if second.Labels[translate.LabelRevision] != first.Labels[translate.LabelRevision] {
		t.Fatal("the local application changed the revision")
	}
	if len(second.Spec.Artifacts) != 1 {
		t.Fatalf("artifacts %+v", second.Spec.Artifacts)
	}
	a := second.Spec.Artifacts[0]
	if a.Name != localapp.ArtifactName || a.Ref != laAppRef || a.Layout != "oci" || a.MountPath != runtimecontract.LocalAppDir ||
		!a.Optional || a.FetchTimeout == nil || a.FetchTimeout.Duration != 0 || a.PullSecretRef != nil || len(a.Claims) != 0 || a.RequirePlatformVerification {
		t.Fatalf("artifact %+v", a)
	}
	digests := 0
	for _, e := range second.Spec.Env {
		if e.Name == runtimecontract.LocalAppDigestEnv {
			digests++
			if e.Value != ccDigestA {
				t.Fatalf("digest %q", e.Value)
			}
		}
	}
	if digests != 1 {
		t.Fatalf("%d digest variables", digests)
	}
	// The arguments are unchanged: the fallback is today's start.
	if !strings.HasPrefix(strings.Join(second.Spec.Args, " "), "up --from="+laAppRef) {
		t.Fatalf("args %v", second.Spec.Args)
	}

	// Opting out removes the condition; new replicas carry nothing.
	updateApp(t, ns, "hello", func(a *spinv1alpha1.SpinApp) {
		a.Annotations = map[string]string{executor.AnnLocalApplication: executor.LocalApplicationDisabled}
		a.Spec.Replicas = 3
	})
	eventually(t, "condition removed", func() error {
		if c := findCond(getApp(t, ns, "hello"), localapp.TypeLocalApplication); c != nil {
			return fmt.Errorf("condition %+v", c)
		}
		return nil
	})
	expectSandboxes(t, ns, "hello-0", "hello-1", "hello-2")
	if n := len(getSandbox(t, ns, "hello-2").Spec.Artifacts); n != 0 {
		t.Fatalf("an opted-out replica carries %d artifacts", n)
	}
}

func TestLocalApplicationSkips(t *testing.T) {
	startManager(t, harnessOpts{localApplication: laSettings(), cacheManager: true})

	ns := laNamespace(t, true)
	mustCreate(t, laExecutor(ns, true), laApp(ns, "tagged", "registry.example.com/apps/hello:v1"))
	expectCondition(t, ns, "tagged", localapp.TypeLocalApplication, metav1.ConditionFalse, localapp.ReasonImageNotPinned)
	expectEvent(t, ns, "tagged", EventLocalApplicationSkipped)

	ns2 := laNamespace(t, false)
	mustCreate(t, laExecutor(ns2, true), laApp(ns2, "hello", laAppRef))
	expectCondition(t, ns2, "hello", localapp.TypeLocalApplication, metav1.ConditionFalse, localapp.ReasonNamespaceNotEnabled)

	// An executor that does not ask: no condition, no placement.
	ns3 := laNamespace(t, true)
	mustCreate(t, laExecutor(ns3, false), laApp(ns3, "hello", laAppRef))
	expectSandboxes(t, ns3, "hello-0")
	if c := findCond(getApp(t, ns3, "hello"), localapp.TypeLocalApplication); c != nil {
		t.Fatalf("condition on a SpinApp that did not ask: %+v", c)
	}
	var pls sandboxv1alpha1.SwiftArtifactPlacementList
	if err := testClient.List(context.Background(), &pls, client.InNamespace(ns3)); err != nil {
		t.Fatal(err)
	}
	if len(pls.Items) != 0 {
		t.Fatalf("placements for a SpinApp that did not ask: %d", len(pls.Items))
	}
}

func TestLocalApplicationNotConfigured(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := laNamespace(t, true)
	mustCreate(t, laExecutor(ns, true), laApp(ns, "hello", laAppRef))
	expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionFalse, localapp.ReasonNotConfigured)
	expectSandboxes(t, ns, "hello-0")
	if n := len(getSandbox(t, ns, "hello-0").Spec.Artifacts); n != 0 {
		t.Fatalf("artifacts without the feature: %d", n)
	}
}

// A replica that carried the local application and failed for an artifact
// reason starts a hold: replacements start from the registry.
func TestLocalApplicationHoldAfterFailure(t *testing.T) {
	startManager(t, harnessOpts{localApplication: laSettings(), cacheManager: true, localHold: 6 * time.Second})
	ns := laNamespace(t, true)
	mustCreate(t, laExecutor(ns, true))
	// Prepare the placement before the SpinApp's first replica exists.
	mustCreate(t, laApp(ns, "hello", laAppRef))
	preparePlacement(t, ns)
	expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionTrue, localapp.ReasonAttached)
	updateApp(t, ns, "hello", func(a *spinv1alpha1.SpinApp) { a.Spec.Replicas = 2 })
	expectSandboxes(t, ns, "hello-0", "hello-1")
	carrier := getSandbox(t, ns, "hello-1")
	if len(carrier.Spec.Artifacts) != 1 {
		t.Fatalf("hello-1 carries no artifact")
	}
	setPhase(t, ns, "hello-1", sandboxv1alpha1.SwiftSandboxFailed, metav1.Condition{Type: sandboxv1alpha1.SwiftSandboxConditionGuestRunning,
		Status: metav1.ConditionFalse, Reason: "ArtifactMaterializeFailed", Message: "artifact application: failed"})
	expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionFalse, localapp.ReasonDeliveryFailed)
	expectEvent(t, ns, "hello", EventLocalApplicationFailed)
	eventually(t, "a replacement without the artifact", func() error {
		simulateGC(t, ns)
		for _, sb := range listSandboxes(t, ns) {
			if sb.Name == "hello-1" && sb.UID != carrier.UID && sb.DeletionTimestamp.IsZero() {
				if len(sb.Spec.Artifacts) != 0 {
					return fmt.Errorf("replacement carries the artifact")
				}
				return nil
			}
		}
		return fmt.Errorf("no replacement yet")
	})
	// The hold ends by itself.
	expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionTrue, localapp.ReasonAttached)
}

// A replica that carried the local application but failed for its runtime
// image never ran the guest: no hold, replacements still carry it.
func TestLocalApplicationNoHoldForRuntimeImage(t *testing.T) {
	startManager(t, harnessOpts{localApplication: laSettings(), cacheManager: true, localHold: time.Hour})
	ns := laNamespace(t, true)
	mustCreate(t, laExecutor(ns, true), laApp(ns, "hello", laAppRef))
	preparePlacement(t, ns)
	expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionTrue, localapp.ReasonAttached)
	updateApp(t, ns, "hello", func(a *spinv1alpha1.SpinApp) { a.Spec.Replicas = 2 })
	expectSandboxes(t, ns, "hello-0", "hello-1")
	carrier := getSandbox(t, ns, "hello-1")
	if len(carrier.Spec.Artifacts) != 1 {
		t.Fatalf("hello-1 carries no artifact")
	}
	setPhase(t, ns, "hello-1", sandboxv1alpha1.SwiftSandboxFailed, metav1.Condition{Type: sandboxv1alpha1.SwiftSandboxConditionGuestRunning,
		Status: metav1.ConditionFalse, Reason: "ImageResolveFailed", Message: "pull failed"})
	eventually(t, "a replacement with the artifact", func() error {
		simulateGC(t, ns)
		for _, sb := range listSandboxes(t, ns) {
			if sb.Name == "hello-1" && sb.UID != carrier.UID && sb.DeletionTimestamp.IsZero() {
				if len(sb.Spec.Artifacts) != 1 {
					return fmt.Errorf("replacement carries no artifact")
				}
				return nil
			}
		}
		return fmt.Errorf("no replacement yet")
	})
	if c := findCond(getApp(t, ns, "hello"), localapp.TypeLocalApplication); c == nil || c.Reason != localapp.ReasonAttached {
		t.Fatalf("condition %+v", c)
	}
}

// A SpinApp that reported another False reason for a long time and then
// starts waiting for its placement reports Placing, not PlacementBlocked:
// the wait is measured from when the placement was first seen missing.
func TestLocalApplicationPlacingAfterLongSkip(t *testing.T) {
	// No cache manager: the placement never appears.
	startManager(t, harnessOpts{localApplication: laSettings()})
	ns := laNamespace(t, false)
	mustCreate(t, laExecutor(ns, true), laApp(ns, "hello", laAppRef))
	expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionFalse, localapp.ReasonNamespaceNotEnabled)
	// Age the condition as if it had been there for a day.
	eventually(t, "condition aged", func() error {
		app := getApp(t, ns, "hello")
		for i := range app.Status.Conditions {
			if app.Status.Conditions[i].Type == localapp.TypeLocalApplication {
				app.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-24 * time.Hour))
			}
		}
		return testClient.Status().Update(context.Background(), app)
	})
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, localapp.NamespaceLabel, localapp.NamespaceEnabled))
	if err := testClient.Patch(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatal(err)
	}
	expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionFalse, localapp.ReasonPlacing)
	// Further reconciles within two minutes keep Placing.
	for i := 0; i < 3; i++ {
		updateApp(t, ns, "hello", func(a *spinv1alpha1.SpinApp) {
			a.Annotations = map[string]string{"example.com/touch": fmt.Sprint(i)}
		})
		time.Sleep(time.Second)
		if c := findCond(getApp(t, ns, "hello"), localapp.TypeLocalApplication); c == nil || c.Reason != localapp.ReasonPlacing {
			t.Fatalf("reconcile %d: condition %+v", i, c)
		}
	}
}

// At the namespace limit, a SpinApp that starts wanting a placement later
// does not displace a prepared one and reports PlacementLimit, even when it
// is older.
func TestLocalApplicationPlacementLimit(t *testing.T) {
	s := laSettings()
	s.MaxPlacements = 1
	startManager(t, harnessOpts{localApplication: s, cacheManager: true})
	ns := laNamespace(t, true)
	old := laApp(ns, "old", "registry.example.com/apps/other@"+ccDigestB)
	old.Annotations = map[string]string{executor.AnnLocalApplication: executor.LocalApplicationDisabled}
	mustCreate(t, laExecutor(ns, true), old)
	time.Sleep(1100 * time.Millisecond) // a later creation timestamp
	mustCreate(t, laApp(ns, "hello", laAppRef))
	preparePlacement(t, ns)
	expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionTrue, localapp.ReasonAttached)
	updateApp(t, ns, "old", func(a *spinv1alpha1.SpinApp) { a.Annotations = nil })
	expectCondition(t, ns, "old", localapp.TypeLocalApplication, metav1.ConditionFalse, localapp.ReasonPlacementLimit)
	var pls sandboxv1alpha1.SwiftArtifactPlacementList
	if err := testClient.List(context.Background(), &pls, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(pls.Items) != 1 || pls.Items[0].Name != (localapp.Key{Namespace: ns, Pool: "spin-pool", Ref: laAppRef}).PlacementName() {
		t.Fatalf("placements %d", len(pls.Items))
	}
	if c := findCond(getApp(t, ns, "hello"), localapp.TypeLocalApplication); c == nil || c.Reason != localapp.ReasonAttached {
		t.Fatalf("the prepared placement was displaced: %+v", c)
	}
}

// A placement at the application placement's name that is for another
// application (a forged or edited spec), even prepared, is never trusted:
// PlacementBlocked, and new replicas carry no artifact.
func TestLocalApplicationForeignPlacementNotTrusted(t *testing.T) {
	// No cache manager, so nothing repairs or replaces the forged object.
	startManager(t, harnessOpts{localApplication: laSettings()})
	ns := laNamespace(t, true)
	k := localapp.Key{Namespace: ns, Pool: "spin-pool", Ref: laAppRef}
	forged := localapp.Placement(localapp.Key{Namespace: ns, Pool: "spin-pool", Ref: "registry.example.com/someone/else@" + ccDigestA})
	forged.Name = k.PlacementName()
	forged.Annotations[localapp.AnnKey] = k.Hash()
	mustCreate(t, forged)
	eventually(t, "forged placement prepared", func() error {
		var pl sandboxv1alpha1.SwiftArtifactPlacement
		if err := testClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: forged.Name}, &pl); err != nil {
			return err
		}
		pl.Status.ObservedGeneration = pl.Generation
		pl.Status.TargetNodes, pl.Status.PreparedNodes = 1, 1
		pl.Status.Conditions = []metav1.Condition{{Type: sandboxv1alpha1.PlacementConditionPrepared, Status: metav1.ConditionTrue,
			Reason: "Prepared", ObservedGeneration: pl.Generation, LastTransitionTime: metav1.Now()}}
		return testClient.Status().Update(context.Background(), &pl)
	})
	mustCreate(t, laExecutor(ns, true), laApp(ns, "hello", laAppRef))
	expectCondition(t, ns, "hello", localapp.TypeLocalApplication, metav1.ConditionFalse, localapp.ReasonPlacementBlocked)
	expectSandboxes(t, ns, "hello-0")
	if n := len(getSandbox(t, ns, "hello-0").Spec.Artifacts); n != 0 {
		t.Fatalf("a replica trusted a foreign placement: %d artifacts", n)
	}
}
