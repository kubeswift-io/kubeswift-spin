package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"

	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/compatibility"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/scheme"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

// The envtest suite runs the reconciler against a real API server loaded
// with the CRDs of the pinned spin-operator and KubeSwift modules, read from
// the Go module cache so they cannot drift from the Go types.
//
// envtest has no garbage collector and no KubeSwift controller. Tests act as
// both: they write SwiftSandbox status and remove the foregroundDeletion
// finalizer that the API server adds when kubeswift-spin deletes a sandbox.

const testRuntimeImage = "ghcr.io/kubeswift-io/kubeswift-spin-runtime:v0.0.0-test"

var (
	testCfg    *rest.Config
	testClient client.Client
	testScheme *runtime.Scheme
)

func moduleDir(mod string) (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", mod).Output()
	if err != nil {
		return "", fmt.Errorf("go list %s: %w", mod, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("KUBEBUILDER_ASSETS is not set; skipping envtest controller tests (run `make test`)")
		os.Exit(m.Run())
	}
	lvl := zapcore.ErrorLevel
	if os.Getenv("TEST_VERBOSE") != "" {
		lvl = zapcore.DebugLevel
	}
	ctrl.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.Level(lvl)))

	spinDir, err := moduleDir("github.com/spinkube/spin-operator")
	if err != nil {
		panic(err)
	}
	ksDir, err := moduleDir("github.com/kubeswift-io/kubeswift")
	if err != nil {
		panic(err)
	}
	env := &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths: []string{
				filepath.Join(spinDir, "config", "crd", "bases"),
				filepath.Join(ksDir, "config", "crd", "bases", "sandbox.kubeswift.io_swiftsandboxes.yaml"),
				filepath.Join(ksDir, "config", "crd", "bases", "sandbox.kubeswift.io_swiftsandboxpools.yaml"),
			},
			ErrorIfPathMissing: true,
		},
	}
	testCfg, err = env.Start()
	if err != nil {
		panic(err)
	}
	testScheme = scheme.New()
	testClient, err = client.New(testCfg, client.Options{Scheme: testScheme})
	if err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

// harness runs one controller manager for a test.
type harness struct {
	t      *testing.T
	cancel context.CancelFunc
	done   sync.WaitGroup
	r      *SpinAppReconciler
}

type harnessOpts struct {
	detector    capabilities.SandboxDetector
	maxReplicas int32
}

func startManager(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest not available")
	}
	mgr, err := ctrl.NewManager(testCfg, ctrl.Options{
		Scheme:  testScheme,
		Cache:   CacheOptions(nil),
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Each test starts its own manager in the same process.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.detector == nil {
		o.detector = capabilities.Static{}
	}
	if o.maxReplicas == 0 {
		o.maxReplicas = 10
	}
	b := NewBackoff()
	b.Base, b.Max = 300*time.Millisecond, time.Second
	r := &SpinAppReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("kubeswift-spin"),
		Defaults: executor.Defaults{
			RuntimeImage:  testRuntimeImage,
			DefaultCPU:    resource.MustParse("1"),
			DefaultMemory: resource.MustParse("512Mi"),
		},
		Options:        compatibility.Options{MaxReplicas: o.maxReplicas, Resources: translate.DefaultResourcePolicy()},
		Detector:       o.detector,
		PoolsServed:    true,
		Backoff:        b,
		WaitingRequeue: 500 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.SetupWithManager(ctx, mgr); err != nil {
		cancel()
		t.Fatal(err)
	}
	h := &harness{t: t, cancel: cancel, r: r}
	h.done.Add(1)
	go func() {
		defer h.done.Done()
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("manager: %v", err)
		}
	}()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.cancel()
	h.done.Wait()
}

var nsCounter int

func newNamespace(t *testing.T) string {
	t.Helper()
	nsCounter++
	name := fmt.Sprintf("t%d-%s", nsCounter, strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())))
	if len(name) > 50 {
		name = name[:50]
	}
	name = strings.TrimRight(name, "-")
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := testClient.Create(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	return name
}

func managedExecutor(ns, name string, ann map[string]string) *spinv1alpha1.SpinAppExecutor {
	return &spinv1alpha1.SpinAppExecutor{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
			Labels:      map[string]string{executor.ManagedByLabel: executor.ManagedByValue},
			Annotations: ann},
		Spec: spinv1alpha1.SpinAppExecutorSpec{CreateDeployment: false},
	}
}

func spinApp(ns, name, exec string, replicas int32) *spinv1alpha1.SpinApp {
	return &spinv1alpha1.SpinApp{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: spinv1alpha1.SpinAppSpec{
			Executor: exec,
			Image:    "ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http:v0.1.0",
			Replicas: replicas,
		},
	}
}

func mustCreate(t *testing.T, objs ...client.Object) {
	t.Helper()
	for _, o := range objs {
		if err := testClient.Create(context.Background(), o); err != nil {
			t.Fatalf("create %T %s: %v", o, o.GetName(), err)
		}
	}
}

// eventually polls f until it returns nil.
func eventually(t *testing.T, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = f(); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %v", what, err)
}

// consistently checks that f keeps returning nil for d.
func consistently(t *testing.T, what string, d time.Duration, f func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := f(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func listSandboxes(t *testing.T, ns string) []sandboxv1alpha1.SwiftSandbox {
	t.Helper()
	var l sandboxv1alpha1.SwiftSandboxList
	if err := testClient.List(context.Background(), &l, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func liveNames(items []sandboxv1alpha1.SwiftSandbox) []string {
	var out []string
	for _, s := range items {
		if s.DeletionTimestamp.IsZero() {
			out = append(out, s.Name)
		}
	}
	return out
}

func expectSandboxes(t *testing.T, ns string, names ...string) {
	t.Helper()
	eventually(t, fmt.Sprintf("sandboxes %v", names), func() error {
		got := liveNames(listSandboxes(t, ns))
		if strings.Join(got, ",") != strings.Join(names, ",") {
			return fmt.Errorf("live sandboxes %v", got)
		}
		return nil
	})
}

// simulateGC removes the foregroundDeletion finalizer from deleting
// sandboxes, standing in for the garbage collector.
func simulateGC(t *testing.T, ns string) {
	t.Helper()
	for _, s := range listSandboxes(t, ns) {
		if s.DeletionTimestamp.IsZero() || len(s.Finalizers) == 0 {
			continue
		}
		patch := client.MergeFrom(s.DeepCopy())
		s.Finalizers = nil
		if err := testClient.Patch(context.Background(), &s, patch); err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
}

// setPhase stands in for the KubeSwift controller.
func setPhase(t *testing.T, ns, name string, phase sandboxv1alpha1.SwiftSandboxPhase, conds ...metav1.Condition) {
	t.Helper()
	eventually(t, "set phase of "+name, func() error {
		var sb sandboxv1alpha1.SwiftSandbox
		if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &sb); err != nil {
			return err
		}
		sb.Status.Phase = phase
		for i := range conds {
			conds[i].LastTransitionTime = metav1.Now()
		}
		sb.Status.Conditions = conds
		return testClient.Status().Update(context.Background(), &sb)
	})
}

func getApp(t *testing.T, ns, name string) *spinv1alpha1.SpinApp {
	t.Helper()
	var a spinv1alpha1.SpinApp
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &a); err != nil {
		t.Fatal(err)
	}
	return &a
}

func expectCondition(t *testing.T, ns, app, condType string, status metav1.ConditionStatus, reason string) *spinv1alpha1.SpinApp {
	t.Helper()
	var last *spinv1alpha1.SpinApp
	eventually(t, fmt.Sprintf("%s=%s/%s", condType, status, reason), func() error {
		last = getApp(t, ns, app)
		for _, c := range last.Status.Conditions {
			if c.Type == condType {
				if c.Status == status && c.Reason == reason && c.ObservedGeneration == last.Generation {
					return nil
				}
				return fmt.Errorf("condition is %s/%s (gen %d of %d): %s", c.Status, c.Reason, c.ObservedGeneration, last.Generation, c.Message)
			}
		}
		return fmt.Errorf("condition %s missing: %+v", condType, last.Status.Conditions)
	})
	return last
}

func updateApp(t *testing.T, ns, name string, mutate func(a *spinv1alpha1.SpinApp)) {
	t.Helper()
	eventually(t, "update SpinApp "+name, func() error {
		a := getApp(t, ns, name)
		mutate(a)
		return testClient.Update(context.Background(), a)
	})
}
