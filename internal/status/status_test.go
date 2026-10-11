package status

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeswift-io/kubeswift-spin/internal/rollout"
)

func si(ord int, rev string, h rollout.Health) Instance {
	return Instance{Instance: rollout.Instance{Name: fmt.Sprintf("app-%d", ord), Ordinal: ord, Revision: rev, Health: h}}
}

func TestRunningIsNotReady(t *testing.T) {
	r := Compute(Input{Generation: 3, Replicas: 2, Revision: "r1",
		Instances: []Instance{si(0, "r1", rollout.Running), si(1, "r1", rollout.Running)}})
	if r.ReadyReplicas != 0 {
		t.Fatalf("running sandboxes reported ready: %d", r.ReadyReplicas)
	}
	if r.Available.Status != metav1.ConditionFalse || r.Available.Reason != ReasonNetworkUnavailable {
		t.Fatalf("available = %+v", r.Available)
	}
	if !strings.Contains(r.Available.Message, "2 of 2 sandboxes running") ||
		!strings.Contains(r.Available.Message, "no inbound port exposure") {
		t.Fatalf("message %q", r.Available.Message)
	}
	if r.Progressing.Status != metav1.ConditionTrue || r.Progressing.Reason != ReasonSandboxRunning {
		t.Fatalf("progressing = %+v", r.Progressing)
	}
	if r.Available.ObservedGeneration != 3 || r.Progressing.ObservedGeneration != 3 {
		t.Fatal("observedGeneration not set")
	}
}

func TestRunningButNotReadyWithExposure(t *testing.T) {
	r := Compute(Input{Replicas: 2, Revision: "r1", Exposure: Exposure{Detected: true},
		Instances: []Instance{si(0, "r1", rollout.Running), si(1, "r1", rollout.Running)}})
	if r.ReadyReplicas != 0 || r.Available.Status != metav1.ConditionFalse || r.Available.Reason != ReasonApplicationNotReady ||
		!strings.Contains(r.Available.Message, "2 running sandboxes do not pass the readiness check") {
		t.Fatalf("available = %+v", r.Available)
	}
}

func TestReadyReplicasWhenObservable(t *testing.T) {
	r := Compute(Input{Replicas: 3, Revision: "r1", Exposure: Exposure{Detected: true},
		Instances: []Instance{si(0, "r1", rollout.Ready), si(1, "r1", rollout.Ready), si(2, "r1", rollout.Running)}})
	if r.ReadyReplicas != 2 || r.Available.Status != metav1.ConditionTrue || r.Available.Reason != ReasonApplicationReady {
		t.Fatalf("result %+v", r)
	}
	r = Compute(Input{Replicas: 3, Revision: "r1", Exposure: Exposure{Detected: true},
		Instances: []Instance{si(0, "r1", rollout.Ready), si(1, "r1", rollout.Running), si(2, "r1", rollout.Running)}})
	if r.Available.Status != metav1.ConditionFalse {
		t.Fatalf("1 of 3 ready reported available: %+v", r.Available)
	}
}

func TestProgressingReasons(t *testing.T) {
	mat := si(0, "r1", rollout.Pending)
	mat.Materializing = true
	waiting := si(0, "r1", rollout.Pending)
	waiting.WaitingReason, waiting.WaitingMessage = "KernelNotFound", "no SwiftKernel named sandbox"
	failedImg := si(0, "r1", rollout.Terminal)
	failedImg.FailureReason, failedImg.FailureMessage = "RootfsMaterializeFailed", "pull failed"
	failedApp := si(0, "r1", rollout.Terminal)
	failedApp.FailureReason, failedApp.FailureMessage = "WorkloadFailed", "workload exited 1"

	cases := []struct {
		name      string
		in        Input
		status    metav1.ConditionStatus
		reason    string
		msgSubstr string
	}{
		{"creating", Input{Replicas: 1, Revision: "r1"}, metav1.ConditionTrue, ReasonSandboxCreating, "0 of 1"},
		{"pending", Input{Replicas: 1, Revision: "r1", Instances: []Instance{si(0, "r1", rollout.Pending)}}, metav1.ConditionTrue, ReasonSandboxCreating, ""},
		{"materializing", Input{Replicas: 1, Revision: "r1", Instances: []Instance{mat}}, metav1.ConditionTrue, ReasonSandboxMaterializing, "materializing"},
		{"waiting on kernel", Input{Replicas: 1, Revision: "r1", Instances: []Instance{waiting}}, metav1.ConditionTrue, ReasonSandboxCreating, "KernelNotFound"},
		{"rolling", Input{Replicas: 2, Revision: "r2", Instances: []Instance{si(0, "r1", rollout.Running), si(1, "r2", rollout.Running)}}, metav1.ConditionTrue, ReasonRollingUpdate, "1 of 2 updated"},
		{"runtime image failure", Input{Replicas: 1, Revision: "r1", Instances: []Instance{failedImg}, RetryIn: "20s"}, metav1.ConditionFalse, ReasonRuntimeImageUnavailable, "replacing in 20s"},
		{"workload failure", Input{Replicas: 1, Revision: "r1", Instances: []Instance{failedApp}}, metav1.ConditionFalse, ReasonSandboxFailed, "sandbox logs app-0"},
		{"blocker", Input{Replicas: 1, Revision: "r1", Blocker: &Blocker{Reason: ReasonUnsupportedConfiguration, Message: "spec.volumes is not supported"}}, metav1.ConditionFalse, ReasonUnsupportedConfiguration, "spec.volumes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Compute(tc.in)
			if r.Progressing.Status != tc.status || r.Progressing.Reason != tc.reason || !strings.Contains(r.Progressing.Message, tc.msgSubstr) {
				t.Fatalf("progressing = %+v", r.Progressing)
			}
			if r.Available.Status != metav1.ConditionFalse {
				t.Fatalf("available = %+v", r.Available)
			}
		})
	}
}

func TestExcludesDeletingAndScaledDown(t *testing.T) {
	d := si(0, "r1", rollout.Running)
	d.Deleting = true
	r := Compute(Input{Replicas: 1, Revision: "r1", Instances: []Instance{d, si(1, "r1", rollout.Running)}})
	if r.Progressing.Reason != ReasonSandboxCreating || !strings.Contains(r.Progressing.Message, "0 of 1") {
		t.Fatalf("progressing = %+v", r.Progressing)
	}
}

func TestDetailIsBounded(t *testing.T) {
	f := si(0, "r1", rollout.Terminal)
	f.FailureReason, f.FailureMessage = "GuestFailed", strings.Repeat("x", 5000)
	r := Compute(Input{Replicas: 1, Revision: "r1", Instances: []Instance{f}})
	if len(r.Progressing.Message) > 400 {
		t.Fatalf("message not bounded: %d bytes", len(r.Progressing.Message))
	}
}

func TestApplyPreservesForeignConditions(t *testing.T) {
	conds := []metav1.Condition{{Type: "example.com/Custom", Status: metav1.ConditionTrue, Reason: "Set", Message: "by someone else"}}
	r := Compute(Input{Replicas: 1, Revision: "r1"})
	if !Apply(&conds, r) {
		t.Fatal("no change reported")
	}
	if len(conds) != 3 || conds[0].Type != "example.com/Custom" || conds[0].Message != "by someone else" {
		t.Fatalf("foreign condition not preserved: %+v", conds)
	}
	if Apply(&conds, r) {
		t.Fatal("re-applying identical status reported a change")
	}
}

func TestMessagesAreBounded(t *testing.T) {
	r := Compute(Input{Replicas: 1, Revision: "r1", Blocker: &Blocker{Reason: ReasonUnsupportedConfiguration, Message: strings.Repeat("x", 100000)}})
	if len(r.Progressing.Message) > maxMessage || !strings.HasSuffix(r.Progressing.Message, "(truncated)") {
		t.Fatalf("message not bounded: %d bytes", len(r.Progressing.Message))
	}
}

func TestSurgeReplicaCountsTowardReadyUpToReplicas(t *testing.T) {
	old := si(0, "r1", rollout.Ready)
	old.Deleting = true
	surge := si(1, "r2", rollout.Ready)
	surge.Surge = true
	r := Compute(Input{Replicas: 1, Revision: "r2", Exposure: Exposure{Detected: true},
		Instances: []Instance{old, surge}})
	if r.ReadyReplicas != 1 || r.Available.Status != metav1.ConditionTrue {
		t.Fatalf("ready %d available %+v", r.ReadyReplicas, r.Available)
	}
	// Never more than spec.replicas, even with every replica and the surge ready.
	r = Compute(Input{Replicas: 1, Revision: "r2", Exposure: Exposure{Detected: true},
		Instances: []Instance{si(0, "r1", rollout.Ready), surgeSI(1, "r2", rollout.Ready), si(2, "r2", rollout.Ready)}})
	if r.ReadyReplicas != 1 {
		t.Fatalf("ready %d", r.ReadyReplicas)
	}
	// A replica left at ordinal <replicas> by a scale down is not counted.
	r = Compute(Input{Replicas: 1, Revision: "r2", Exposure: Exposure{Detected: true},
		Instances: []Instance{si(0, "r2", rollout.Running), si(1, "r2", rollout.Ready)}})
	if r.ReadyReplicas != 0 {
		t.Fatalf("scale-down leftover counted: ready %d", r.ReadyReplicas)
	}
}

func surgeSI(ord int, rev string, h rollout.Health) Instance {
	i := si(ord, rev, h)
	i.Surge = true
	return i
}

func TestFailedSurgeIsReported(t *testing.T) {
	f := surgeSI(1, "r2", rollout.Terminal)
	f.FailureReason, f.FailureMessage = "GuestFailed", "spin exited"
	r := Compute(Input{Namespace: "apps", Replicas: 1, Revision: "r2", Exposure: Exposure{Detected: true}, RetryIn: "20s",
		Instances: []Instance{si(0, "r1", rollout.Ready), f}})
	if r.Progressing.Status != metav1.ConditionFalse || r.Progressing.Reason != ReasonSandboxFailed ||
		!strings.Contains(r.Progressing.Message, "app-1") || !strings.Contains(r.Progressing.Message, "replacing in 20s") {
		t.Fatalf("progressing %+v", r.Progressing)
	}
	if r.ReadyReplicas != 1 || r.Available.Status != metav1.ConditionTrue {
		t.Fatalf("old replica not counted: ready %d available %+v", r.ReadyReplicas, r.Available)
	}
}

// After a second spec change the old-revision surge may be the only replica
// serving; it still counts, as an outdated desired replica does.
func TestOutdatedSurgeStillCountsAsReady(t *testing.T) {
	res := Compute(Input{Namespace: "ns", Generation: 3, Replicas: 1, Revision: "r3", Exposure: Exposure{Detected: true},
		Instances: []Instance{si(0, "r3", rollout.Pending), surgeSI(1, "r2", rollout.Ready)}})
	if res.ReadyReplicas != 1 || res.Available.Status != metav1.ConditionTrue {
		t.Fatalf("ready %d available %s/%s", res.ReadyReplicas, res.Available.Status, res.Available.Reason)
	}
}
