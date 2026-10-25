package cachemanager

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/localapp"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
	"github.com/kubeswift-io/kubeswift-spin/internal/scheme"
)

var (
	laRuntime = "ghcr.io/kubeswift-io/kubeswift-spin-runtime@sha256:" + strings.Repeat("c", 64)
	laImage   = "registry.example.com/apps/hello@sha256:" + strings.Repeat("a", 64)
)

func laNamespace(enabled bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}}
	if enabled {
		ns.Labels = map[string]string{localapp.NamespaceLabel: localapp.NamespaceEnabled}
	}
	return ns
}

func laExecutor() *spinv1alpha1.SpinAppExecutor {
	return &spinv1alpha1.SpinAppExecutor{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "kubeswift",
		Labels: map[string]string{executor.ManagedByLabel: executor.ManagedByValue},
		Annotations: map[string]string{executor.AnnLocalApplication: executor.LocalApplicationEnabled,
			executor.AnnSandboxPool: "warm", executor.AnnRuntimeImage: laRuntime}}}
}

func laApp(name, image string) *spinv1alpha1.SpinApp {
	return &spinv1alpha1.SpinApp{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: name, CreationTimestamp: metav1.Now()},
		Spec: spinv1alpha1.SpinAppSpec{Image: image, Executor: "kubeswift", Replicas: 1}}
}

func laReconciler(t *testing.T, now *time.Time, objs ...client.Object) (*ApplicationReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme.New()).WithObjects(objs...).
		WithStatusSubresource(&sandboxv1alpha1.SwiftArtifactPlacement{}).Build()
	return &ApplicationReconciler{
		Client:           c,
		Defaults:         executor.Defaults{RuntimeImage: laRuntime, DefaultCPU: resource.MustParse("1"), DefaultMemory: resource.MustParse("256Mi")},
		Settings:         localapp.Settings{Enabled: true, Runtimes: []string{laRuntime}, MaxPlacements: 8, MaxNewPerHour: 10, Retention: 24 * time.Hour},
		Detector:         capabilities.Static{LocalArtifacts: true, Ports: true, ReadinessProbe: true, PodMetadata: true, SecretFiles: true},
		PlacementsServed: true,
		Now:              func() time.Time { return *now },
	}, c
}

func laReconcile(t *testing.T, r *ApplicationReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team"}}); err != nil {
		t.Fatal(err)
	}
}

func laKey() localapp.Key {
	return localapp.Key{Namespace: "team", Pool: "warm", Ref: laImage}
}

func getPlacement(t *testing.T, c client.Client, name string) (*sandboxv1alpha1.SwiftArtifactPlacement, bool) {
	t.Helper()
	var pl sandboxv1alpha1.SwiftArtifactPlacement
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: name}, &pl)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return &pl, true
}

func TestApplicationPlacementLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	app := laApp("hello", laImage)
	r, c := laReconciler(t, &now, laNamespace(true), laExecutor(), app)

	laReconcile(t, r)
	k := laKey()
	pl, ok := getPlacement(t, c, k.PlacementName())
	if !ok || !localapp.Owned(pl, k) || pl.Spec.Artifacts[0].Layout != "oci" || pl.Spec.Artifacts[0].Ref != laImage {
		t.Fatalf("placement not created as owned: %+v", pl)
	}
	// Idempotent.
	rv := pl.ResourceVersion
	laReconcile(t, r)
	if pl2, _ := getPlacement(t, c, k.PlacementName()); pl2.ResourceVersion != rv {
		t.Fatal("second reconcile changed the placement")
	}

	// The SpinApp goes away: marked, retained, then deleted after retention.
	if err := c.Delete(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	laReconcile(t, r)
	pl, ok = getPlacement(t, c, k.PlacementName())
	if !ok || pl.Annotations[localapp.AnnUnwantedSince] == "" {
		t.Fatalf("unwanted placement not marked: %+v", pl)
	}
	now = now.Add(23 * time.Hour)
	laReconcile(t, r)
	if _, ok := getPlacement(t, c, k.PlacementName()); !ok {
		t.Fatal("deleted before the retention ended")
	}
	// Wanted again within retention: the mark is cleared.
	app2 := laApp("hello", laImage)
	if err := c.Create(context.Background(), app2); err != nil {
		t.Fatal(err)
	}
	laReconcile(t, r)
	pl, _ = getPlacement(t, c, k.PlacementName())
	if _, marked := pl.Annotations[localapp.AnnUnwantedSince]; marked {
		t.Fatal("rollback did not clear the retention mark")
	}
	if err := c.Delete(context.Background(), app2); err != nil {
		t.Fatal(err)
	}
	laReconcile(t, r)
	now = now.Add(25 * time.Hour)
	laReconcile(t, r)
	if _, ok := getPlacement(t, c, k.PlacementName()); ok {
		t.Fatal("not deleted after the retention ended")
	}
}

func TestApplicationPlacementRepairsAndRespectsForeignObjects(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	k := laKey()
	// A foreign object at the placement's name.
	foreign := &sandboxv1alpha1.SwiftArtifactPlacement{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: k.PlacementName()},
		Spec: sandboxv1alpha1.SwiftArtifactPlacementSpec{PoolRef: corev1.LocalObjectReference{Name: "warm"},
			Artifacts: []sandboxv1alpha1.PlacementArtifact{{Name: "x", Ref: "other.example.com/x@sha256:" + strings.Repeat("d", 64)}}}}
	// A forged look-alike elsewhere: labels and annotation, wrong name.
	forged := localapp.Placement(k)
	forged.Name = "kss-app-forged"
	r, c := laReconciler(t, &now, laNamespace(true), laExecutor(), laApp("hello", laImage), foreign, forged)
	laReconcile(t, r)
	got, _ := getPlacement(t, c, k.PlacementName())
	if got.Spec.Artifacts[0].Name != "x" || got.Labels != nil {
		t.Fatalf("foreign placement modified: %+v", got)
	}
	if _, ok := getPlacement(t, c, "kss-app-forged"); !ok {
		t.Fatal("a placement the cache manager does not own was deleted")
	}

	// Drift in a non-key field of an owned placement is repaired.
	if err := c.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	drifted := localapp.Placement(k)
	drifted.Spec.Artifacts[0].Name = "renamed"
	if err := c.Create(context.Background(), drifted); err != nil {
		t.Fatal(err)
	}
	laReconcile(t, r)
	got, _ = getPlacement(t, c, k.PlacementName())
	if got.Spec.Artifacts[0].Name != localapp.ArtifactName {
		t.Fatalf("drift not repaired: %+v", got.Spec)
	}
}

func TestApplicationPlacementNotWanted(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for name, objs := range map[string][]client.Object{
		"namespace not enabled": {laNamespace(false), laExecutor(), laApp("hello", laImage)},
		"tag":                   {laNamespace(true), laExecutor(), laApp("hello", "registry.example.com/apps/hello:v1")},
		"scaled to zero": func() []client.Object {
			a := laApp("hello", laImage)
			a.Spec.Replicas = 0
			return []client.Object{laNamespace(true), laExecutor(), a}
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			r, c := laReconciler(t, &now, objs...)
			laReconcile(t, r)
			var pls sandboxv1alpha1.SwiftArtifactPlacementList
			if err := c.List(context.Background(), &pls); err != nil {
				t.Fatal(err)
			}
			if len(pls.Items) != 0 {
				t.Fatalf("placements created: %d", len(pls.Items))
			}
		})
	}
	// Feature off in the chart: an existing owned placement is marked
	// unwanted, never wanted again.
	r, c := laReconciler(t, &now, laNamespace(true), laExecutor(), laApp("hello", laImage), localapp.Placement(laKey()))
	r.Settings.Enabled = false
	laReconcile(t, r)
	pl, _ := getPlacement(t, c, laKey().PlacementName())
	if pl.Annotations[localapp.AnnUnwantedSince] == "" {
		t.Fatal("placement of a disabled feature not marked unwanted")
	}
}
