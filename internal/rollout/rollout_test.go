package rollout

import (
	"fmt"
	"reflect"
	"testing"
)

func inst(ord int, rev string, h Health) Instance {
	return Instance{Name: fmt.Sprintf("app-%d", ord), Ordinal: ord, Revision: rev, Health: h}
}

func surgeInst(ord int, rev string, h Health) Instance {
	i := inst(ord, rev, h)
	i.Surge = true
	return i
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

	// A surge replica at ordinal 3 comes first; nothing is deleted yet.
	p := Compute(in)
	if !reflect.DeepEqual(p.Create, []int{3}) || len(p.Delete) != 0 {
		t.Fatalf("first step %+v", p)
	}
	in.Instances = append(in.Instances, surgeInst(3, "r2", Pending))
	if p := Compute(in); len(p.Create) != 0 || len(p.Delete) != 0 || !p.Waiting {
		t.Fatalf("deleted before the surge replica was available: %+v", p)
	}

	// With the surge replica available, one outdated replica is replaced.
	in.Instances[3].Health = Running
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-2": ReasonRollout}) {
		t.Fatalf("second step %v", got)
	}

	// While the replaced replica is coming back, nothing else is touched.
	in.Instances[2] = inst(2, "r2", Pending)
	if p := Compute(in); len(p.Delete) != 0 || len(p.Create) != 0 || !p.Waiting {
		t.Fatalf("rollout did not wait: %+v", p)
	}

	// Once it runs, the next replica is replaced.
	in.Instances[2] = inst(2, "r2", Running)
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-1": ReasonRollout}) {
		t.Fatalf("third step %v", got)
	}

	// When every desired replica is current, the surge replica goes.
	in.Instances[0], in.Instances[1] = inst(0, "r2", Running), inst(1, "r2", Running)
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-3": ReasonScaleDown}) {
		t.Fatalf("surge not removed: %v", got)
	}
}

func TestSingleReplicaRolloutKeepsOneAvailable(t *testing.T) {
	in := Input{Replicas: 1, Revision: "r2", ReadinessObservable: true, Instances: []Instance{inst(0, "r1", Ready)}}
	if p := Compute(in); !reflect.DeepEqual(p.Create, []int{1}) || len(p.Delete) != 0 {
		t.Fatalf("surge %+v", p)
	}
	in.Instances = append(in.Instances, surgeInst(1, "r2", Running))
	if p := Compute(in); len(p.Delete) != 0 {
		t.Fatalf("old replica deleted before the new one was ready: %+v", p)
	}
	in.Instances[1].Health = Ready
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-0": ReasonRollout}) {
		t.Fatalf("delete %v", got)
	}
	// The old replica is terminating: the surge replica stays.
	in.Instances[0].Deleting = true
	if p := Compute(in); len(p.Delete) != 0 || len(p.Create) != 0 || !p.Waiting {
		t.Fatalf("plan while the old replica terminates: %+v", p)
	}
	// It is gone: ordinal 0 is recreated, the surge replica stays.
	in.Instances = in.Instances[1:]
	if p := Compute(in); !reflect.DeepEqual(p.Create, []int{0}) || len(p.Delete) != 0 {
		t.Fatalf("recreate %+v", p)
	}
	in.Instances = append(in.Instances, inst(0, "r2", Running))
	if p := Compute(in); len(p.Delete) != 0 {
		t.Fatalf("surge removed before ordinal 0 was ready: %+v", p)
	}
	in.Instances[1].Health = Ready
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-1": ReasonScaleDown}) {
		t.Fatalf("surge not removed: %v", got)
	}
}

func TestBrokenSurgeKeepsTheOldReplicas(t *testing.T) {
	in := Input{Replicas: 2, Revision: "r2", ReadinessObservable: true, Instances: []Instance{
		inst(0, "r1", Ready), inst(1, "r1", Ready), surgeInst(2, "r2", Terminal)}}
	in.InBackoff = map[int]bool{2: true}
	p := Compute(in)
	if len(p.Delete) != 0 || len(p.Create) != 0 || !p.Waiting {
		t.Fatalf("plan %+v", p)
	}
	in.InBackoff = nil
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-2": ReasonFailed}) {
		t.Fatalf("failed surge not replaced: %v", got)
	}
}

func TestSurgeFromAnEarlierRevisionIsReplaced(t *testing.T) {
	in := Input{Replicas: 1, Revision: "r3", ReadinessObservable: true, Instances: []Instance{
		inst(0, "r1", Ready), surgeInst(1, "r2", Ready)}}
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-1": ReasonRollout}) {
		t.Fatalf("delete %v", got)
	}
}

func TestScaleToZeroHasNoSurge(t *testing.T) {
	p := Compute(Input{Replicas: 0, Revision: "r2", Instances: []Instance{inst(0, "r1", Ready)}})
	if len(p.Create) != 0 || !reflect.DeepEqual(deletions(p), map[string]Reason{"app-0": ReasonScaleDown}) {
		t.Fatalf("plan %+v", p)
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
		inst(0, "r1", Ready), inst(1, "r2", Running), surgeInst(2, "r2", Running)}}
	if p := Compute(in); len(p.Delete) != 0 || !p.Waiting {
		t.Fatalf("rollout advanced before readiness: %+v", p)
	}
	in.Instances[1].Health, in.Instances[2].Health = Ready, Ready
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-0": ReasonRollout}) {
		t.Fatalf("delete %v", got)
	}
}

func TestStaleInstanceIsRemoved(t *testing.T) {
	stale := inst(0, "r1", Running)
	stale.Name, stale.Stale = "app-renamed", true
	p := Compute(Input{Replicas: 2, Revision: "r1", Instances: []Instance{stale}})
	if !reflect.DeepEqual(deletions(p), map[string]Reason{"app-renamed": ReasonStale}) {
		t.Fatalf("delete %+v", p.Delete)
	}
	if !reflect.DeepEqual(p.Create, []int{0, 1}) {
		t.Fatalf("create %v", p.Create)
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

func TestReplicaLeftByScaleDownIsNotASurge(t *testing.T) {
	// Scale 2 -> 1 while ordinal 0 is still starting: ordinal 1 goes.
	p := Compute(Input{Replicas: 1, Revision: "r1", ReadinessObservable: true, Instances: []Instance{
		inst(0, "r1", Pending), inst(1, "r1", Ready)}})
	if !reflect.DeepEqual(deletions(p), map[string]Reason{"app-1": ReasonScaleDown}) {
		t.Fatalf("plan %+v", p)
	}
}
