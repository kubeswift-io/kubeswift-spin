package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	"github.com/kubeswift-io/kubeswift-spin/internal/status"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

var ctx = context.Background()

func getSandbox(t *testing.T, ns, name string) *sandboxv1alpha1.SwiftSandbox {
	t.Helper()
	var sb sandboxv1alpha1.SwiftSandbox
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sb); err != nil {
		t.Fatal(err)
	}
	return &sb
}

func TestCreateTranslatesSpinApp(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	app := spinApp(ns, "hello", "kubeswift", 2)
	app.Spec.Variables = []spinv1alpha1.SpinVar{{Name: "greeting", Value: "hi"}}
	app.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("1G")}
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), app)

	expectSandboxes(t, ns, "hello-0", "hello-1")
	a := getApp(t, ns, "hello")
	sb := getSandbox(t, ns, "hello-0")

	if !metav1.IsControlledBy(sb, a) {
		t.Fatalf("sandbox not controlled by the SpinApp: %+v", sb.OwnerReferences)
	}
	if sb.Spec.Image != testRuntimeImage || sb.Spec.CPU != 1 || sb.Spec.Memory.String() != "954Mi" {
		t.Fatalf("spec %+v", sb.Spec)
	}
	if sb.Spec.Command[0] != runtimecontract.EntrypointPath || sb.Spec.Args[1] != "--from="+app.Spec.Image {
		t.Fatalf("command %v args %v", sb.Spec.Command, sb.Spec.Args)
	}
	if sb.Spec.Network.Mode != sandboxv1alpha1.SandboxNetworkRestricted {
		t.Fatalf("network %q", sb.Spec.Network.Mode)
	}
	if len(sb.Spec.Env) != 1 || sb.Spec.Env[0].Name != "SPIN_VARIABLE_GREETING" {
		t.Fatalf("env %+v", sb.Spec.Env)
	}
	for k, v := range map[string]string{
		translate.LabelManagedBy: translate.ManagedByValue, translate.LabelApp: "hello",
		translate.LabelOrdinal: "0", translate.LabelExecutor: "kubeswift", translate.LabelSpinKubeAppName: "hello",
	} {
		if sb.Labels[k] != v {
			t.Fatalf("label %s = %q, want %q", k, sb.Labels[k], v)
		}
	}

	a = expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionTrue, status.ReasonSandboxCreating)
	if a.Status.ActiveScheduler != "kubeswift" || a.Status.ReadyReplicas != 0 {
		t.Fatalf("status %+v", a.Status)
	}
	expectCondition(t, ns, "hello", status.TypeAvailable, metav1.ConditionFalse, status.ReasonSandboxCreating)
}

func TestRunningSandboxesAreNotReportedReady(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), spinApp(ns, "hello", "kubeswift", 2))
	expectSandboxes(t, ns, "hello-0", "hello-1")

	setPhase(t, ns, "hello-0", sandboxv1alpha1.SwiftSandboxMaterializing)
	expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionTrue, status.ReasonSandboxMaterializing)

	setPhase(t, ns, "hello-0", sandboxv1alpha1.SwiftSandboxRunning)
	setPhase(t, ns, "hello-1", sandboxv1alpha1.SwiftSandboxRunning)
	expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionTrue, status.ReasonSandboxRunning)
	a := expectCondition(t, ns, "hello", status.TypeAvailable, metav1.ConditionFalse, status.ReasonNetworkUnavailable)
	if a.Status.ReadyReplicas != 0 {
		t.Fatalf("readyReplicas = %d for unreachable sandboxes", a.Status.ReadyReplicas)
	}
}

func TestExposureDetectedButNotImplemented(t *testing.T) {
	startManager(t, harnessOpts{detector: capabilities.Static{Ports: true, ReadinessProbe: true, PodMetadata: true}})
	ns := newNamespace(t)
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), spinApp(ns, "hello", "kubeswift", 1))
	expectSandboxes(t, ns, "hello-0")
	setPhase(t, ns, "hello-0", sandboxv1alpha1.SwiftSandboxRunning)
	a := expectCondition(t, ns, "hello", status.TypeAvailable, metav1.ConditionFalse, status.ReasonNetworkUnavailable)
	if c := findCond(a, status.TypeAvailable); !strings.Contains(c.Message, "upgrade kubeswift-spin") {
		t.Fatalf("message %q", c.Message)
	}
}

func findCond(a *spinv1alpha1.SpinApp, typ string) *metav1.Condition {
	for i := range a.Status.Conditions {
		if a.Status.Conditions[i].Type == typ {
			return &a.Status.Conditions[i]
		}
	}
	return nil
}

func TestScaleUpAndDown(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), spinApp(ns, "hello", "kubeswift", 1))
	expectSandboxes(t, ns, "hello-0")
	uid0 := getSandbox(t, ns, "hello-0").UID

	updateApp(t, ns, "hello", func(a *spinv1alpha1.SpinApp) { a.Spec.Replicas = 3 })
	expectSandboxes(t, ns, "hello-0", "hello-1", "hello-2")
	if getSandbox(t, ns, "hello-0").UID != uid0 {
		t.Fatal("scale up replaced an existing replica")
	}

	updateApp(t, ns, "hello", func(a *spinv1alpha1.SpinApp) { a.Spec.Replicas = 1 })
	expectSandboxes(t, ns, "hello-0")
	simulateGC(t, ns)
	eventually(t, "scaled-down sandboxes gone", func() error {
		if n := len(listSandboxes(t, ns)); n != 1 {
			return fmt.Errorf("%d sandboxes", n)
		}
		return nil
	})
	if getSandbox(t, ns, "hello-0").UID != uid0 {
		t.Fatal("scale down replaced replica 0")
	}
}

func TestDeletedChildIsRecreated(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), spinApp(ns, "hello", "kubeswift", 1))
	expectSandboxes(t, ns, "hello-0")
	old := getSandbox(t, ns, "hello-0")
	if err := testClient.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	eventually(t, "replacement", func() error {
		var sb sandboxv1alpha1.SwiftSandbox
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "hello-0"}, &sb); err != nil {
			return err
		}
		if sb.UID == old.UID {
			return fmt.Errorf("still the old sandbox")
		}
		return nil
	})
}

func TestFailedChildIsReplacedAfterBackoff(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), spinApp(ns, "hello", "kubeswift", 1))
	expectSandboxes(t, ns, "hello-0")
	old := getSandbox(t, ns, "hello-0")

	setPhase(t, ns, "hello-0", sandboxv1alpha1.SwiftSandboxFailed, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionGuestRunning, Status: metav1.ConditionFalse,
		Reason: "WorkloadFailed", Message: "workload exited 1"})
	a := expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionFalse, status.ReasonSandboxFailed)
	if c := findCond(a, status.TypeProgressing); !strings.Contains(c.Message, "swiftctl sandbox logs hello-0") {
		t.Fatalf("message %q", c.Message)
	}

	eventually(t, "failed sandbox deleted after backoff", func() error {
		simulateGC(t, ns)
		var sb sandboxv1alpha1.SwiftSandbox
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "hello-0"}, &sb); err != nil {
			return err
		}
		if sb.UID == old.UID {
			return fmt.Errorf("not replaced yet")
		}
		return nil
	})
	expectEvent(t, ns, "hello", EventSandboxFailed)
}

func TestRuntimeImageFailure(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), spinApp(ns, "hello", "kubeswift", 1))
	expectSandboxes(t, ns, "hello-0")
	setPhase(t, ns, "hello-0", sandboxv1alpha1.SwiftSandboxFailed, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionGuestRunning, Status: metav1.ConditionFalse,
		Reason: "ImageResolveFailed", Message: "manifest unknown"})
	expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionFalse, status.ReasonRuntimeImageUnavailable)
}

func TestRollingReplacementOnImageChange(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), spinApp(ns, "hello", "kubeswift", 2))
	expectSandboxes(t, ns, "hello-0", "hello-1")
	setPhase(t, ns, "hello-0", sandboxv1alpha1.SwiftSandboxRunning)
	setPhase(t, ns, "hello-1", sandboxv1alpha1.SwiftSandboxRunning)
	oldRev := getSandbox(t, ns, "hello-0").Labels[translate.LabelRevision]
	uid0 := getSandbox(t, ns, "hello-0").UID

	updateApp(t, ns, "hello", func(a *spinv1alpha1.SpinApp) { a.Spec.Image = "ghcr.io/example/hello-http:v2" })

	// The highest ordinal goes first; replica 0 keeps running meanwhile.
	eventually(t, "hello-1 deleting", func() error {
		if getSandbox(t, ns, "hello-1").DeletionTimestamp.IsZero() {
			return fmt.Errorf("not deleting")
		}
		return nil
	})
	consistently(t, "hello-0 untouched while hello-1 is replaced", time.Second, func() error {
		sb := getSandbox(t, ns, "hello-0")
		if !sb.DeletionTimestamp.IsZero() || sb.UID != uid0 {
			return fmt.Errorf("hello-0 touched")
		}
		return nil
	})
	simulateGC(t, ns)
	eventually(t, "hello-1 recreated at new revision", func() error {
		var sb sandboxv1alpha1.SwiftSandbox
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "hello-1"}, &sb); err != nil {
			return err
		}
		if sb.Labels[translate.LabelRevision] == oldRev || !strings.Contains(strings.Join(sb.Spec.Args, " "), "hello-http:v2") {
			return fmt.Errorf("old revision")
		}
		return nil
	})
	expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionTrue, status.ReasonRollingUpdate)

	setPhase(t, ns, "hello-1", sandboxv1alpha1.SwiftSandboxRunning)
	eventually(t, "hello-0 replaced", func() error {
		simulateGC(t, ns)
		var sb sandboxv1alpha1.SwiftSandbox
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "hello-0"}, &sb); err != nil {
			return err
		}
		if sb.UID == uid0 {
			return fmt.Errorf("not replaced yet")
		}
		return nil
	})
}

func TestUnsupportedConfigurationIsReported(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	app := spinApp(ns, "hello", "kubeswift", 1)
	app.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	app.Spec.Variables = []spinv1alpha1.SpinVar{{Name: "database_url", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "url"}}}}
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), app)

	a := expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionFalse, status.ReasonUnsupportedConfiguration)
	msg := findCond(a, status.TypeProgressing).Message
	for _, want := range []string{"spec.volumes", `variable "database_url" uses secretKeyRef`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q lacks %q", msg, want)
		}
	}
	consistently(t, "no sandboxes for unsupported configuration", time.Second, func() error {
		if n := len(listSandboxes(t, ns)); n != 0 {
			return fmt.Errorf("%d sandboxes created", n)
		}
		return nil
	})
	expectEvent(t, ns, "hello", EventUnsupported)

	// Fixing the spec unblocks reconciliation.
	updateApp(t, ns, "hello", func(a *spinv1alpha1.SpinApp) { a.Spec.Volumes = nil; a.Spec.Variables = nil })
	expectSandboxes(t, ns, "hello-0")
}

func TestUnmanagedExecutorIsIgnored(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	foreign := &spinv1alpha1.SpinAppExecutor{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kubeswift"}, // same name, no label
		Spec:       spinv1alpha1.SpinAppExecutorSpec{CreateDeployment: false},
	}
	mustCreate(t, foreign, spinApp(ns, "hello", "kubeswift", 1))
	consistently(t, "nothing happens for an unmanaged executor", 1500*time.Millisecond, func() error {
		if n := len(listSandboxes(t, ns)); n != 0 {
			return fmt.Errorf("%d sandboxes", n)
		}
		if a := getApp(t, ns, "hello"); len(a.Status.Conditions) != 0 || a.Status.ActiveScheduler != "" {
			return fmt.Errorf("status written: %+v", a.Status)
		}
		return nil
	})

	// Labelling the executor hands the app to kubeswift-spin.
	eventually(t, "label executor", func() error {
		var e spinv1alpha1.SpinAppExecutor
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(foreign), &e); err != nil {
			return err
		}
		e.Labels = map[string]string{executor.ManagedByLabel: executor.ManagedByValue}
		return testClient.Update(ctx, &e)
	})
	expectSandboxes(t, ns, "hello-0")
}

func TestExecutorChangeCleansUp(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	other := &spinv1alpha1.SpinAppExecutor{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "containerd-shim-spin"},
		Spec:       spinv1alpha1.SpinAppExecutorSpec{CreateDeployment: true},
	}
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), other, spinApp(ns, "hello", "kubeswift", 2))
	expectSandboxes(t, ns, "hello-0", "hello-1")

	updateApp(t, ns, "hello", func(a *spinv1alpha1.SpinApp) { a.Spec.Executor = "containerd-shim-spin" })
	expectSandboxes(t, ns)
	expectEvent(t, ns, "hello", EventExecutorChanged)
}

func TestExecutorLabelRemovalCleansUp(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	exec := managedExecutor(ns, "kubeswift", nil)
	mustCreate(t, exec, spinApp(ns, "hello", "kubeswift", 1))
	expectSandboxes(t, ns, "hello-0")
	eventually(t, "unlabel executor", func() error {
		var e spinv1alpha1.SpinAppExecutor
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(exec), &e); err != nil {
			return err
		}
		delete(e.Labels, executor.ManagedByLabel)
		return testClient.Update(ctx, &e)
	})
	expectSandboxes(t, ns)
}

func TestExecutorDeletionKeepsWorkloads(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	exec := managedExecutor(ns, "kubeswift", nil)
	mustCreate(t, exec, spinApp(ns, "hello", "kubeswift", 1))
	expectSandboxes(t, ns, "hello-0")
	uid := getSandbox(t, ns, "hello-0").UID

	if err := testClient.Delete(ctx, exec); err != nil {
		t.Fatal(err)
	}
	expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionFalse, status.ReasonExecutorNotFound)
	if sb := getSandbox(t, ns, "hello-0"); sb.UID != uid || !sb.DeletionTimestamp.IsZero() {
		t.Fatal("sandbox removed because the executor disappeared")
	}

	// Recreating the executor resumes normal reconciliation.
	mustCreate(t, managedExecutor(ns, "kubeswift", nil))
	expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionTrue, status.ReasonSandboxCreating)
}

func TestInvalidExecutorBlocks(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	bad := managedExecutor(ns, "kubeswift", map[string]string{executor.AnnNetworkMode: "none", executor.Prefix + "typo": "x"})
	mustCreate(t, bad, spinApp(ns, "hello", "kubeswift", 1))
	a := expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionFalse, status.ReasonExecutorInvalid)
	msg := findCond(a, status.TypeProgressing).Message
	if !strings.Contains(msg, "network-mode=none") || !strings.Contains(msg, "unknown annotation") {
		t.Fatalf("message %q", msg)
	}
	if n := len(listSandboxes(t, ns)); n != 0 {
		t.Fatalf("%d sandboxes for an invalid executor", n)
	}
}

// A labelled executor with createDeployment: true is Spin Operator's to
// realize, and Spin Operator rewrites the status of its SpinApps. kubeswift-spin
// must report the problem without becoming a second status writer.
func TestCreateDeploymentExecutorStatusLeftToSpinOperator(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	bad := managedExecutor(ns, "kubeswift", nil)
	bad.Spec.CreateDeployment = true
	mustCreate(t, bad, spinApp(ns, "hello", "kubeswift", 1))
	expectEvent(t, ns, "hello", EventExecutorInvalid)
	consistently(t, "no status and no sandboxes", time.Second, func() error {
		if a := getApp(t, ns, "hello"); len(a.Status.Conditions) != 0 || a.Status.ActiveScheduler != "" {
			return fmt.Errorf("status written: %+v", a.Status)
		}
		if n := len(listSandboxes(t, ns)); n != 0 {
			return fmt.Errorf("%d sandboxes", n)
		}
		return nil
	})
}

func TestMultipleNamespacesAndProfiles(t *testing.T) {
	startManager(t, harnessOpts{})
	nsA, nsB := newNamespace(t), newNamespace(t)
	mustCreate(t,
		managedExecutor(nsA, "kubeswift", nil),
		managedExecutor(nsA, "kubeswift-open", map[string]string{executor.AnnNetworkMode: "open", executor.AnnDefaultMemory: "1Gi"}),
		managedExecutor(nsB, "kubeswift", nil),
		spinApp(nsA, "hello", "kubeswift", 1),
		spinApp(nsA, "outbound", "kubeswift-open", 1),
		spinApp(nsB, "hello", "kubeswift", 1),
	)
	expectSandboxes(t, nsA, "hello-0", "outbound-0")
	expectSandboxes(t, nsB, "hello-0")
	if m := getSandbox(t, nsA, "outbound-0").Spec; m.Network.Mode != sandboxv1alpha1.SandboxNetworkOpen || m.Memory.String() != "1Gi" {
		t.Fatalf("open profile not applied: %+v", m)
	}
	if m := getSandbox(t, nsA, "hello-0").Spec; m.Network.Mode != sandboxv1alpha1.SandboxNetworkRestricted {
		t.Fatalf("default profile not applied: %+v", m)
	}
	a, b := getSandbox(t, nsA, "hello-0"), getSandbox(t, nsB, "hello-0")
	if a.OwnerReferences[0].UID == b.OwnerReferences[0].UID {
		t.Fatal("same-named apps in different namespaces share an owner")
	}
}

func TestConflictingSandboxIsNeverAdopted(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	// A sandbox with the name kubeswift-spin wants and lookalike labels, but
	// no owner reference.
	squatter := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hello-0", Labels: translate.Labels("hello", "kubeswift", "deadbeef00", 0)},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: "ghcr.io/someone/else:v1", Memory: resource.MustParse("512Mi")},
	}
	mustCreate(t, squatter)
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), spinApp(ns, "hello", "kubeswift", 2))

	expectSandboxes(t, ns, "hello-0", "hello-1")
	a := expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionFalse, status.ReasonSandboxConflict)
	if !strings.Contains(findCond(a, status.TypeProgressing).Message, "hello-0 already exists") {
		t.Fatalf("message %+v", findCond(a, status.TypeProgressing))
	}
	consistently(t, "squatter untouched", time.Second, func() error {
		sb := getSandbox(t, ns, "hello-0")
		if sb.UID != squatter.UID || !sb.DeletionTimestamp.IsZero() || len(sb.OwnerReferences) != 0 {
			return fmt.Errorf("squatter modified: %+v", sb.ObjectMeta)
		}
		return nil
	})

	// Removing the squatter is noticed without any event on the SpinApp.
	if err := testClient.Delete(ctx, getSandbox(t, ns, "hello-0")); err != nil {
		t.Fatal(err)
	}
	app := getApp(t, ns, "hello")
	eventually(t, "hello-0 recreated and owned", func() error {
		var sb sandboxv1alpha1.SwiftSandbox
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "hello-0"}, &sb); err != nil {
			return err
		}
		if !metav1.IsControlledBy(&sb, app) {
			return fmt.Errorf("hello-0 not owned yet")
		}
		return nil
	})
	expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionTrue, status.ReasonSandboxCreating)
}

func TestStaleOwnedSandboxIsRemoved(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), spinApp(ns, "hello", "kubeswift", 1))
	expectSandboxes(t, ns, "hello-0")
	app := getApp(t, ns, "hello")
	stale := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hello-old", Labels: translate.Labels("hello", "kubeswift", "deadbeef00", 0)},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: testRuntimeImage, Memory: resource.MustParse("512Mi")},
	}
	if err := controllerutil.SetControllerReference(app, stale, testScheme); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, stale)
	expectSandboxes(t, ns, "hello-0")
}

func TestWarmPool(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	mustCreate(t,
		managedExecutor(ns, "kubeswift-warm", map[string]string{executor.AnnSandboxPool: "spin-pool"}),
		spinApp(ns, "hello", "kubeswift-warm", 1))
	a := expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionFalse, status.ReasonWarmPoolIncompatible)
	if !strings.Contains(findCond(a, status.TypeProgressing).Message, "does not exist") {
		t.Fatalf("message %+v", findCond(a, status.TypeProgressing))
	}

	pool := &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "spin-pool"},
		Spec:       sandboxv1alpha1.SwiftSandboxPoolSpec{Image: testRuntimeImage, CPU: 2, Memory: resource.MustParse("512Mi")},
	}
	mustCreate(t, pool)
	// The pool watch requeues the app; the reason stays the same but the
	// message now names the mismatching field.
	eventually(t, "cpu mismatch reported", func() error {
		c := findCond(getApp(t, ns, "hello"), status.TypeProgressing)
		if c == nil || c.Reason != status.ReasonWarmPoolIncompatible || !strings.Contains(c.Message, "cpu: sandbox needs") {
			return fmt.Errorf("condition %+v", c)
		}
		return nil
	})
	if n := len(listSandboxes(t, ns)); n != 0 {
		t.Fatalf("%d sandboxes created for an incompatible pool", n)
	}

	eventually(t, "fix pool", func() error {
		var p sandboxv1alpha1.SwiftSandboxPool
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(pool), &p); err != nil {
			return err
		}
		p.Spec.CPU = 1
		return testClient.Update(ctx, &p)
	})
	expectSandboxes(t, ns, "hello-0")
	if ref := getSandbox(t, ns, "hello-0").Spec.PoolRef; ref == nil || ref.Name != "spin-pool" {
		t.Fatalf("poolRef = %+v", ref)
	}
}

func TestReplicaLimit(t *testing.T) {
	startManager(t, harnessOpts{maxReplicas: 3})
	ns := newNamespace(t)
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), spinApp(ns, "hello", "kubeswift", 50))
	expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionFalse, status.ReasonUnsupportedConfiguration)
	if n := len(listSandboxes(t, ns)); n != 0 {
		t.Fatalf("%d sandboxes created above the replica limit", n)
	}
}

// TestControllerRestartDoesNotRecreate covers upgrades: a new controller
// process must adopt nothing new and replace nothing.
func TestControllerRestartDoesNotRecreate(t *testing.T) {
	h := startManager(t, harnessOpts{})
	ns := newNamespace(t)
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), spinApp(ns, "hello", "kubeswift", 2))
	expectSandboxes(t, ns, "hello-0", "hello-1")
	setPhase(t, ns, "hello-0", sandboxv1alpha1.SwiftSandboxRunning)
	setPhase(t, ns, "hello-1", sandboxv1alpha1.SwiftSandboxRunning)
	uids := map[string]string{}
	for _, sb := range listSandboxes(t, ns) {
		uids[sb.Name] = string(sb.UID)
	}
	h.stop()

	startManager(t, harnessOpts{})
	consistently(t, "sandboxes survive a controller restart", 1500*time.Millisecond, func() error {
		items := listSandboxes(t, ns)
		if len(items) != 2 {
			return fmt.Errorf("%d sandboxes", len(items))
		}
		for _, sb := range items {
			if uids[sb.Name] != string(sb.UID) || !sb.DeletionTimestamp.IsZero() {
				return fmt.Errorf("%s was replaced", sb.Name)
			}
		}
		return nil
	})
}

func TestStatusPreservesForeignConditions(t *testing.T) {
	startManager(t, harnessOpts{})
	ns := newNamespace(t)
	mustCreate(t, managedExecutor(ns, "kubeswift", nil), spinApp(ns, "hello", "kubeswift", 1))
	expectSandboxes(t, ns, "hello-0")
	eventually(t, "add foreign condition", func() error {
		a := getApp(t, ns, "hello")
		a.Status.Conditions = append(a.Status.Conditions, metav1.Condition{
			Type: "example.com/Audited", Status: metav1.ConditionTrue, Reason: "Checked", Message: "external", LastTransitionTime: metav1.Now()})
		return testClient.Status().Update(ctx, a)
	})
	setPhase(t, ns, "hello-0", sandboxv1alpha1.SwiftSandboxRunning)
	a := expectCondition(t, ns, "hello", status.TypeProgressing, metav1.ConditionTrue, status.ReasonSandboxRunning)
	if findCond(a, "example.com/Audited") == nil {
		t.Fatalf("foreign condition dropped: %+v", a.Status.Conditions)
	}
}

func expectEvent(t *testing.T, ns, app, reason string) {
	t.Helper()
	eventually(t, "event "+reason, func() error {
		var l eventsv1.EventList
		if err := testClient.List(ctx, &l, client.InNamespace(ns)); err != nil {
			return err
		}
		for _, e := range l.Items {
			if e.Regarding.Name == app && e.Reason == reason {
				return nil
			}
		}
		return fmt.Errorf("no %s event among %d events", reason, len(l.Items))
	})
}
