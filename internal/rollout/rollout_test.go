package rollout

import (
	"fmt"
	"reflect"
	"testing"
)

func inst(ord int, rev string, h Health) Instance {
	return Instance{Name: fmt.Sprintf("app-%d", ord), Ordinal: ord, Revision: rev, Health: h}
}

func deletions(p Plan) map[string]Reason {
	out := map[string]Reason{}
	for _, d := range p.Delete {
		out[d.Name] = d.Reason
	}
	return out
}

func TestInitialCreate(t *testing.T) {
	p := Compute(Input{Replicas: 3, Revision: "r1"})
	if !reflect.DeepEqual(p.Create, []int{0, 1, 2}) || len(p.Delete) != 0 {
		t.Fatalf("plan %+v", p)
	}
}

func TestSteadyStateIsIdempotent(t *testing.T) {
	in := Input{Replicas: 2, Revision: "r1", Instances: []Instance{inst(0, "r1", Running), inst(1, "r1", Running)}}
	for i := 0; i < 3; i++ {
		p := Compute(in)
		if len(p.Create) != 0 || len(p.Delete) != 0 || p.Waiting {
			t.Fatalf("steady state produced work: %+v", p)
		}
	}
}

func TestScaleUpCreatesOnlyMissing(t *testing.T) {
	p := Compute(Input{Replicas: 4, Revision: "r1", Instances: []Instance{inst(0, "r1", Running), inst(2, "r1", Pending)}})
	if !reflect.DeepEqual(p.Create, []int{1, 3}) {
		t.Fatalf("create %v", p.Create)
	}
}

func TestScaleDownDeletesHighestOrdinals(t *testing.T) {
	p := Compute(Input{Replicas: 1, Revision: "r1", Instances: []Instance{inst(0, "r1", Running), inst(1, "r1", Running), inst(2, "r1", Pending)}})
	if len(p.Create) != 0 {
		t.Fatalf("create %v", p.Create)
	}
	if !reflect.DeepEqual(deletions(p), map[string]Reason{"app-1": ReasonScaleDown, "app-2": ReasonScaleDown}) {
		t.Fatalf("delete %+v", p.Delete)
	}
	if p.Delete[0].Ordinal != 2 {
		t.Fatalf("deletions not ordered highest first: %+v", p.Delete)
	}
}

func TestDeletingInstanceIsWaitedFor(t *testing.T) {
	d := inst(0, "r0", Running)
	d.Deleting = true
	p := Compute(Input{Replicas: 1, Revision: "r1", Instances: []Instance{d}})
	if len(p.Create) != 0 || len(p.Delete) != 0 || !p.Waiting {
		t.Fatalf("plan %+v", p)
	}
}

func TestFailedInstanceIsReplacedUnlessInBackoff(t *testing.T) {
	in := Input{Replicas: 2, Revision: "r1", Instances: []Instance{inst(0, "r1", Running), inst(1, "r1", Terminal)}}
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-1": ReasonFailed}) {
		t.Fatalf("delete %v", got)
	}
	in.InBackoff = map[int]bool{1: true}
	p := Compute(in)
	if len(p.Delete) != 0 || !p.Waiting {
		t.Fatalf("backoff ignored: %+v", p)
	}
}

func TestRollingUpdateOneAtATime(t *testing.T) {
	in := Input{Replicas: 3, Revision: "r2", Instances: []Instance{
		inst(0, "r1", Running), inst(1, "r1", Running), inst(2, "r1", Running)}}
	p := Compute(in)
	if !reflect.DeepEqual(deletions(p), map[string]Reason{"app-2": ReasonRollout}) {
		t.Fatalf("first step %+v", p.Delete)
	}

	// While the replaced replica is coming back, nothing else is touched.
	in.Instances[2] = inst(2, "r2", Pending)
	p = Compute(in)
	if len(p.Delete) != 0 || len(p.Create) != 0 || !p.Waiting {
		t.Fatalf("rollout did not wait: %+v", p)
	}

	// Once it runs, the next replica is replaced.
	in.Instances[2] = inst(2, "r2", Running)
	p = Compute(in)
	if !reflect.DeepEqual(deletions(p), map[string]Reason{"app-1": ReasonRollout}) {
		t.Fatalf("second step %+v", p.Delete)
	}
}

func TestRolloutStallsOnBrokenRevision(t *testing.T) {
	in := Input{Replicas: 3, Revision: "r2", Instances: []Instance{
		inst(0, "r1", Running), inst(1, "r1", Running), inst(2, "r2", Terminal)}}
	in.InBackoff = map[int]bool{2: true}
	p := Compute(in)
	if len(p.Delete) != 0 {
		t.Fatalf("rollout continued past a broken replica: %+v", p.Delete)
	}
}

func TestUnavailableOutdatedReplicaIsReplacedAtOnce(t *testing.T) {
	in := Input{Replicas: 2, Revision: "r2", Instances: []Instance{inst(0, "r1", Pending), inst(1, "r1", Pending)}}
	got := deletions(Compute(in))
	if !reflect.DeepEqual(got, map[string]Reason{"app-0": ReasonRollout, "app-1": ReasonRollout}) {
		t.Fatalf("delete %v", got)
	}
}

func TestReadinessGatesRolloutWhenObservable(t *testing.T) {
	in := Input{Replicas: 2, Revision: "r2", ReadinessObservable: true, Instances: []Instance{
		inst(0, "r1", Ready), inst(1, "r2", Running)}}
	if p := Compute(in); len(p.Delete) != 0 || !p.Waiting {
		t.Fatalf("rollout advanced before readiness: %+v", p)
	}
	in.Instances[1].Health = Ready
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-0": ReasonRollout}) {
		t.Fatalf("delete %v", got)
	}
}

func TestStaleAndBlocked(t *testing.T) {
	stale := inst(0, "r1", Running)
	stale.Name, stale.Stale = "app-renamed", true
	p := Compute(Input{Replicas: 2, Revision: "r1", Instances: []Instance{stale}, Blocked: map[int]bool{1: true}})
	if !reflect.DeepEqual(deletions(p), map[string]Reason{"app-renamed": ReasonStale}) {
		t.Fatalf("delete %+v", p.Delete)
	}
	if !reflect.DeepEqual(p.Create, []int{0}) || !p.Waiting {
		t.Fatalf("create %v waiting %v", p.Create, p.Waiting)
	}
}

func TestScaleToZeroAfterAutoscalingIsNotSupported(t *testing.T) {
	// Replicas 0 is rejected by compatibility analysis; the planner itself
	// simply deletes everything.
	p := Compute(Input{Replicas: 0, Revision: "r1", Instances: []Instance{inst(0, "r1", Running)}})
	if len(p.Delete) != 1 {
		t.Fatalf("plan %+v", p)
	}
}
