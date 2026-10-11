// Package rollout decides which replica sandboxes to create and delete. It is
// a pure function of the observed replicas so every transition is testable
// without an API server.
//
// The model is StatefulSet-like: replica i is always the sandbox named
// <app>-<i>. SwiftSandbox specs are immutable, so a configuration change is
// applied by deleting a replica and recreating it at the new revision.
//
// A rollout keeps the desired number of replicas available (ADR 0013): it
// first adds one surge replica at ordinal <replicas> with the new revision,
// replaces an outdated replica only while the remaining available replicas
// still number <replicas>, and removes the surge replica once every desired
// ordinal runs the new revision and is available.
package rollout

import "sort"

// Health is the observed state of one replica, ordered from least to most
// healthy for the non-terminal states.
type Health int

const (
	// Pending covers a sandbox that exists but whose guest is not running
	// yet (KubeSwift phases "", Pending and Materializing).
	Pending Health = iota
	// Running means the microVM guest is up. It does not prove that Spin is
	// serving requests.
	Running
	// Ready means application readiness was verified. With the current
	// KubeSwift API this state is never reached; see
	// docs/upstream/kubeswift-sandbox-health-probes.md.
	Ready
	// Terminal means the sandbox reached Completed or Failed. A Spin server
	// never exits on its own, so both are treated as failures.
	Terminal
)

// Reason explains why a replica is deleted.
type Reason string

const (
	ReasonScaleDown Reason = "scale_down"
	ReasonRollout   Reason = "rollout"
	ReasonFailed    Reason = "failed"
	ReasonStale     Reason = "stale"
)

// Instance is one observed replica owned by the SpinApp.
type Instance struct {
	Name     string
	Ordinal  int
	Revision string
	Health   Health
	Deleting bool
	// Stale marks an owned sandbox whose name does not match its ordinal,
	// for example after a manual relabel. It is always removed.
	Stale bool
	// Surge marks the extra replica a rollout creates at ordinal Replicas.
	// A replica left at that ordinal by a scale down is not a surge one,
	// and a surge replica at a lower ordinal (after replicas was raised) is
	// replaced like an outdated one.
	Surge bool
}

// Input is the desired state and the observed replicas.
type Input struct {
	Replicas  int
	Revision  string
	Instances []Instance
	// ReadinessObservable is true when the installed KubeSwift can report
	// application readiness. When false, Running is the highest observable
	// health and is what gates rolling replacement.
	ReadinessObservable bool
	// InBackoff holds ordinals whose failed replica must not be replaced yet.
	// The caller keeps the wait short for a replica from an earlier revision.
	InBackoff map[int]bool
}

// Deletion is one replica to delete.
type Deletion struct {
	Name    string
	Ordinal int
	Reason  Reason
}

// Plan is the set of actions for one reconcile.
type Plan struct {
	Create []int
	Delete []Deletion
	// Waiting is true when work remains that this plan does not perform yet
	// (deletions in flight, backoff, or a rollout gated on availability).
	// The reconciler requeues as a safety net in that case.
	Waiting bool
}

// Available reports whether h counts as available.
func Available(h Health, readinessObservable bool) bool {
	if readinessObservable {
		return h == Ready
	}
	return h == Running || h == Ready
}

// Compute returns the plan for in.
func Compute(in Input) Plan {
	var p Plan
	surgeOrd := in.Replicas
	byOrdinal := map[int]Instance{}
	var surge *Instance
	// surgeBusy is set while the surge ordinal's name is taken by a sandbox
	// that is being deleted or is deleted by this plan; the surge replica is
	// created only once the name is free.
	surgeBusy := false
	// extra holds replicas above the desired count that a scale-down
	// removes, decided after the desired replicas are counted.
	var extra []Instance
	for _, inst := range in.Instances {
		switch {
		case inst.Stale && inst.Deleting:
			// Its ordinal label may match a real replica; never let it
			// stand in for one.
			p.Waiting = true
		case inst.Deleting:
			p.Waiting = true
			if inst.Ordinal == surgeOrd {
				surgeBusy = true
			} else {
				byOrdinal[inst.Ordinal] = inst
			}
		case inst.Stale:
			p.Delete = append(p.Delete, Deletion{inst.Name, inst.Ordinal, ReasonStale})
		case inst.Ordinal == surgeOrd && inst.Surge && in.Replicas > 0:
			s := inst
			surge = &s
		case inst.Ordinal >= in.Replicas:
			extra = append(extra, inst)
			if inst.Ordinal == surgeOrd {
				surgeBusy = true
			}
		default:
			byOrdinal[inst.Ordinal] = inst
		}
	}

	available := 0
	rolling := false   // a desired ordinal still runs an outdated revision
	allCurrent := true // every desired ordinal is current and available
	var outdatedAvailable []Instance
	for ord := 0; ord < in.Replicas; ord++ {
		inst, ok := byOrdinal[ord]
		// A surge replica that became a desired one when replicas was raised
		// is replaced like an outdated replica, so the surge label only ever
		// marks the extra replica of a rollout.
		outdated := ok && (inst.Revision != in.Revision || inst.Surge)
		if !ok || inst.Deleting || outdated || !Available(inst.Health, in.ReadinessObservable) {
			allCurrent = false
		}
		switch {
		case !ok:
			p.Create = append(p.Create, ord)
		case inst.Deleting:
			// Recreated once the old object is gone.
		case inst.Health == Terminal:
			// The backoff of a replica from an earlier revision is short
			// (see InBackoff), so a fixed spec is not held back.
			if in.InBackoff[ord] {
				p.Waiting = true
				continue
			}
			p.Delete = append(p.Delete, Deletion{inst.Name, ord, ReasonFailed})
		case outdated:
			if Available(inst.Health, in.ReadinessObservable) {
				rolling = true
				available++
				outdatedAvailable = append(outdatedAvailable, inst)
			} else {
				// An outdated replica that is not serving can be replaced
				// at once; waiting would only delay recovery.
				p.Delete = append(p.Delete, Deletion{inst.Name, ord, ReasonRollout})
			}
		default:
			if Available(inst.Health, in.ReadinessObservable) {
				available++
			}
		}
	}

	// The surge replica exists only while a rollout needs it: it is created
	// when a desired ordinal is outdated and kept until every desired
	// ordinal is current and available.
	wantSurge := in.Replicas > 0 && (rolling || (surge != nil && !allCurrent))
	surgeAvailable := 0
	switch {
	case surge == nil:
		if wantSurge && rolling && !surgeBusy {
			p.Create = append(p.Create, surgeOrd)
		}
	case !wantSurge:
		p.Delete = append(p.Delete, Deletion{surge.Name, surgeOrd, ReasonScaleDown})
	case surge.Health == Terminal:
		if in.InBackoff[surgeOrd] {
			p.Waiting = true
		} else {
			p.Delete = append(p.Delete, Deletion{surge.Name, surgeOrd, ReasonFailed})
		}
	case surge.Revision != in.Revision:
		// A surge replica from an earlier revision, after the spec changed
		// again during a rollout. Replace it, but while it serves keep it
		// until the desired replicas alone are enough, so a second change
		// never takes away the only available replica.
		if Available(surge.Health, in.ReadinessObservable) && available < in.Replicas {
			surgeAvailable = 1
			p.Waiting = true
		} else {
			p.Delete = append(p.Delete, Deletion{surge.Name, surgeOrd, ReasonRollout})
		}
	case Available(surge.Health, in.ReadinessObservable):
		surgeAvailable = 1
	}

	// A scale-down keeps available extra replicas, lowest ordinal first,
	// only while the desired replicas and the surge replica are fewer than
	// spec.replicas available, so scaling down never removes the replicas
	// that serve while the remaining ones start. The others go at once.
	need := in.Replicas - available - surgeAvailable
	sort.Slice(extra, func(i, j int) bool { return extra[i].Ordinal < extra[j].Ordinal })
	for _, inst := range extra {
		if need > 0 && inst.Health != Terminal && Available(inst.Health, in.ReadinessObservable) {
			need--
			p.Waiting = true
			continue
		}
		p.Delete = append(p.Delete, Deletion{inst.Name, inst.Ordinal, ReasonScaleDown})
	}

	// Replace one available outdated replica only when the others still
	// leave the desired number available, so a broken new revision stalls
	// the rollout without taking a replica away.
	if len(outdatedAvailable) > 0 {
		capacity := available + surgeAvailable
		if capacity-1 >= in.Replicas && len(p.Create) == 0 && !p.Waiting && !hasReason(p.Delete, ReasonFailed, ReasonRollout) {
			sort.Slice(outdatedAvailable, func(i, j int) bool { return outdatedAvailable[i].Ordinal > outdatedAvailable[j].Ordinal })
			victim := outdatedAvailable[0]
			p.Delete = append(p.Delete, Deletion{victim.Name, victim.Ordinal, ReasonRollout})
		} else {
			p.Waiting = true
		}
	}

	sort.Ints(p.Create)
	sort.Slice(p.Delete, func(i, j int) bool { return p.Delete[i].Ordinal > p.Delete[j].Ordinal })
	return p
}

func hasReason(ds []Deletion, reasons ...Reason) bool {
	for _, d := range ds {
		for _, r := range reasons {
			if d.Reason == r {
				return true
			}
		}
	}
	return false
}
