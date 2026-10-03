// Package rollout decides which replica sandboxes to create and delete. It is
// a pure function of the observed replicas so every transition is testable
// without an API server.
//
// The model is StatefulSet-like: replica i is always the sandbox named
// <app>-<i>. SwiftSandbox specs are immutable, so a configuration change is
// applied by deleting a replica and recreating it at the new revision, one
// available replica at a time.
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
	byOrdinal := map[int]Instance{}

	for _, inst := range in.Instances {
		switch {
		case inst.Deleting:
			p.Waiting = true
			byOrdinal[inst.Ordinal] = inst
		case inst.Stale:
			p.Delete = append(p.Delete, Deletion{inst.Name, inst.Ordinal, ReasonStale})
		case inst.Ordinal >= in.Replicas:
			p.Delete = append(p.Delete, Deletion{inst.Name, inst.Ordinal, ReasonScaleDown})
		default:
			byOrdinal[inst.Ordinal] = inst
		}
	}

	available := 0
	var outdatedAvailable []Instance
	for ord := 0; ord < in.Replicas; ord++ {
		inst, ok := byOrdinal[ord]
		switch {
		case !ok:
			p.Create = append(p.Create, ord)
		case inst.Deleting:
			// Recreated once the old object is gone.
		case inst.Health == Terminal:
			if in.InBackoff[ord] {
				p.Waiting = true
				continue
			}
			p.Delete = append(p.Delete, Deletion{inst.Name, ord, ReasonFailed})
		case inst.Revision != in.Revision:
			if Available(inst.Health, in.ReadinessObservable) {
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

	// Replace one available outdated replica only when every desired
	// replica is available, so a broken new revision stalls the rollout
	// after a single replica instead of taking the whole app down.
	if len(outdatedAvailable) > 0 {
		if available == in.Replicas && len(p.Create) == 0 && !p.Waiting && !hasReason(p.Delete, ReasonFailed, ReasonRollout) {
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
