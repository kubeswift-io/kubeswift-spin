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

func TestScaleDownKeepsAServingReplicaUntilTheRestAreReady(t *testing.T) {
	// Scale 2 -> 1 while ordinal 0 is still starting: ordinal 1 serves
	// until ordinal 0 is ready, and is not treated as a surge replica.
	in := Input{Replicas: 1, Revision: "r1", ReadinessObservable: true, Instances: []Instance{
		inst(0, "r1", Pending), inst(1, "r1", Ready)}}
	if p := Compute(in); len(p.Delete) != 0 || len(p.Create) != 0 || !p.Waiting {
		t.Fatalf("plan %+v", p)
	}
	in.Instances[0].Health = Ready
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-1": ReasonScaleDown}) {
		t.Fatalf("delete %v", got)
	}
	// A replica that is not serving goes at once.
	in.Instances = []Instance{inst(0, "r1", Pending), inst(1, "r1", Pending)}
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-1": ReasonScaleDown}) {
		t.Fatalf("delete %v", got)
	}
}

// Scaling down at the end of a rollout, while ordinal 0 restarts at the new
// revision, keeps one serving replica (reliability review S2).
func TestScaleDownMidRolloutKeepsAvailability(t *testing.T) {
	in := Input{Replicas: 1, Revision: "r2", ReadinessObservable: true, Instances: []Instance{
		inst(0, "r2", Pending), inst(1, "r2", Ready), surgeInst(2, "r2", Ready)}}
	p := Compute(in)
	if got := deletions(p); !reflect.DeepEqual(got, map[string]Reason{"app-2": ReasonScaleDown}) || len(p.Create) != 0 {
		t.Fatalf("plan %+v", p)
	}
	// With more desired replicas: keep only as many as are missing.
	in = Input{Replicas: 2, Revision: "r2", ReadinessObservable: true, Instances: []Instance{
		inst(0, "r2", Pending), inst(1, "r2", Ready), inst(2, "r2", Ready), inst(3, "r2", Ready), inst(4, "r2", Terminal)}}
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-3": ReasonScaleDown, "app-4": ReasonScaleDown}) {
		t.Fatalf("delete %v", got)
	}
}

// A second spec change while the old-revision surge is the only available
// replica keeps that surge until the desired replica is available again.
func TestFixForwardKeepsTheOnlyAvailableSurge(t *testing.T) {
	in := Input{Replicas: 1, Revision: "r3", ReadinessObservable: true, Instances: []Instance{
		inst(0, "r2", Pending), surgeInst(1, "r2", Ready)}}
	p := Compute(in)
	if got := deletions(p); !reflect.DeepEqual(got, map[string]Reason{"app-0": ReasonRollout}) || !p.Waiting {
		t.Fatalf("step 1: delete %v waiting %v", got, p.Waiting)
	}
	// app-0 recreated at r3, not ready yet: the r2 surge still serves.
	in.Instances = []Instance{inst(0, "r3", Pending), surgeInst(1, "r2", Ready)}
	if p := Compute(in); len(p.Delete) != 0 || len(p.Create) != 0 || !p.Waiting {
		t.Fatalf("step 2: %+v", p)
	}
	// app-0 ready at r3: the old surge goes, and no new one is needed.
	in.Instances = []Instance{inst(0, "r3", Ready), surgeInst(1, "r2", Ready)}
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-1": ReasonScaleDown}) {
		t.Fatalf("step 3: delete %v", got)
	}
}

func TestFixForwardWithTwoReplicasKeepsTwoAvailable(t *testing.T) {
	in := Input{Replicas: 2, Revision: "r3", ReadinessObservable: true, Instances: []Instance{
		inst(0, "r1", Ready), inst(1, "r2", Pending), surgeInst(2, "r2", Ready)}}
	p := Compute(in)
	if got := deletions(p); !reflect.DeepEqual(got, map[string]Reason{"app-1": ReasonRollout}) {
		t.Fatalf("delete %v", got)
	}
}

func TestBrokenSurgeFromAnEarlierRevisionHonorsItsBackoff(t *testing.T) {
	in := Input{Replicas: 1, Revision: "r3", ReadinessObservable: true, InBackoff: map[int]bool{1: true}, Instances: []Instance{
		inst(0, "r1", Ready), surgeInst(1, "r2", Terminal)}}
	if p := Compute(in); len(p.Delete) != 0 || len(p.Create) != 0 || !p.Waiting {
		t.Fatalf("in backoff: %+v", p)
	}
	in.InBackoff = nil
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-1": ReasonFailed}) {
		t.Fatalf("delete %v", got)
	}
}

// After replicas is raised during a rollout, the former surge replica is a
// desired one; it is replaced like an outdated replica so the label does not
// outlive the rollout.
func TestFormerSurgeBelowReplicasIsReplacedWithoutLosingCapacity(t *testing.T) {
	in := Input{Replicas: 2, Revision: "r2", ReadinessObservable: true, Instances: []Instance{
		inst(0, "r2", Ready), surgeInst(1, "r2", Ready)}}
	p := Compute(in)
	if len(p.Delete) != 0 || !reflect.DeepEqual(p.Create, []int{2}) {
		t.Fatalf("expected a new surge first: %+v", p)
	}
	in.Instances = append(in.Instances, surgeInst(2, "r2", Ready))
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-1": ReasonRollout}) {
		t.Fatalf("delete %v", got)
	}
	in.Instances = []Instance{inst(0, "r2", Ready), inst(1, "r2", Ready), surgeInst(2, "r2", Ready)}
	if got := deletions(Compute(in)); !reflect.DeepEqual(got, map[string]Reason{"app-2": ReasonScaleDown}) {
		t.Fatalf("delete %v", got)
	}
}

// A scale-down that frees the surge ordinal does not create the surge until
// the old sandbox with that name is gone.
func TestSurgeWaitsForItsNameToBeFree(t *testing.T) {
	in := Input{Replicas: 2, Revision: "r2", ReadinessObservable: true, Instances: []Instance{
		inst(0, "r1", Ready), inst(1, "r1", Ready), inst(2, "r1", Ready)}}
	p := Compute(in)
	if len(p.Create) != 0 || deletions(p)["app-2"] != ReasonScaleDown {
		t.Fatalf("step 1: %+v", p)
	}
	gone := inst(2, "r1", Ready)
	gone.Deleting = true
	in.Instances = []Instance{inst(0, "r1", Ready), inst(1, "r1", Ready), gone}
	if p := Compute(in); len(p.Create) != 0 || !p.Waiting {
		t.Fatalf("step 2: %+v", p)
	}
	in.Instances = in.Instances[:2]
	if p := Compute(in); !reflect.DeepEqual(p.Create, []int{2}) {
		t.Fatalf("step 3: %+v", p)
	}
}

func TestSurgeLabeledSandboxIsRemovedAtZeroReplicas(t *testing.T) {
	in := Input{Replicas: 0, Revision: "r2", ReadinessObservable: true, Instances: []Instance{
		inst(0, "r2", Ready), surgeInst(1, "r2", Ready)}}
	if got := deletions(Compute(in)); len(got) != 2 {
		t.Fatalf("delete %v", got)
	}
}

// A stale sandbox being deleted whose ordinal label matches a real replica
// does not hide that replica.
func TestDeletingStaleSandboxDoesNotMaskAReplica(t *testing.T) {
	stale := Instance{Name: "app-x", Ordinal: 0, Revision: "r1", Health: Ready, Stale: true, Deleting: true}
	in := Input{Replicas: 1, Revision: "r1", ReadinessObservable: true, Instances: []Instance{inst(0, "r1", Ready), stale}}
	if p := Compute(in); len(p.Create) != 0 || len(p.Delete) != 0 {
		t.Fatalf("plan %+v", p)
	}
	in.Instances = []Instance{stale}
	if p := Compute(in); !reflect.DeepEqual(p.Create, []int{0}) {
		t.Fatalf("missing replica not created: %+v", p)
	}
}

// After a scale-up, a former surge replica that failed is replaced after
// its backoff like any failed replica, and the new surge is created at the
// new surge ordinal meanwhile.
func TestFailedFormerSurgeAfterScaleUpHonorsBackoff(t *testing.T) {
	in := Input{Replicas: 2, Revision: "r2", ReadinessObservable: true, InBackoff: map[int]bool{1: true}, Instances: []Instance{
		inst(0, "r1", Ready), surgeInst(1, "r2", Terminal)}}
	p := Compute(in)
	if len(p.Delete) != 0 || !reflect.DeepEqual(p.Create, []int{2}) || !p.Waiting {
		t.Fatalf("plan %+v", p)
	}
}
