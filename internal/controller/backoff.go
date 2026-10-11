package controller

import (
	"sync"
	"time"

	"github.com/kubeswift-io/kubeswift-spin/internal/rollout"
	"github.com/kubeswift-io/kubeswift-spin/internal/status"
)

const (
	backoffBase = 10 * time.Second
	backoffMax  = 5 * time.Minute
	// backoffStable is how long a replica must run without failing before
	// its failure count resets, as kubelet does for CrashLoopBackOff. A
	// guest is Running before Spin has even pulled the application, so
	// resetting on the first Running observation would keep the delay at
	// the base value for an application that always fails.
	backoffStable = 10 * time.Minute
)

type ordinalState struct {
	failures     int
	revision     string
	failedAt     time.Time
	lastUID      string
	notBefore    time.Time
	runningUID   string
	runningSince time.Time
}

// Backoff delays the replacement of a replica that keeps failing, the
// equivalent of CrashLoopBackOff. KubeSwift launcher pods never restart, so a
// Spin process that exits leaves a terminal sandbox that kubeswift-spin must
// replace; without a delay a broken application would churn microVMs.
//
// State is in memory. After a controller restart the first failure of each
// replica is replaced after the base delay again, which is acceptable.
type Backoff struct {
	// Base is the delay after the first failure; it doubles per failure up
	// to Max.
	Base, Max time.Duration
	// Stable is how long a replica must run before its failures are
	// forgotten.
	Stable time.Duration

	mu   sync.Mutex
	apps map[string]map[int]*ordinalState
	// rejected counts consecutive reconciles of an app whose SwiftSandbox
	// create the API server refused (quota or admission policy).
	rejected map[string]int
}

// NewBackoff returns an empty Backoff with the default delays.
func NewBackoff() *Backoff {
	return &Backoff{Base: backoffBase, Max: backoffMax, Stable: backoffStable, apps: map[string]map[int]*ordinalState{}, rejected: map[string]int{}}
}

func delayFor(failures int) time.Duration { return NewBackoff().delayFor(failures) }

func (b *Backoff) delayFor(failures int) time.Duration {
	d := b.Base
	for i := 1; i < failures && d < b.Max; i++ {
		d *= 2
	}
	if d > b.Max {
		d = b.Max
	}
	return d
}

func (b *Backoff) state(app string, ord int) *ordinalState {
	m, ok := b.apps[app]
	if !ok {
		m = map[int]*ordinalState{}
		b.apps[app] = m
	}
	s, ok := m[ord]
	if !ok {
		s = &ordinalState{}
		m[ord] = s
	}
	return s
}

// RecordFailure records that the sandbox with uid, at revision, failed. It
// returns true the first time a given sandbox is recorded. A failure at a
// different revision than the previous one starts the count over, so a fixed
// spec is not delayed by the failures of the broken one.
func (b *Backoff) RecordFailure(app string, ord int, uid, revision string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state(app, ord)
	if s.lastUID == uid {
		return false
	}
	if s.runningUID == uid && now.Sub(s.runningSince) >= b.Stable {
		// This sandbox ran long enough before failing: start over.
		s.failures = 0
	}
	if s.revision != revision {
		s.failures = 0
		s.revision = revision
	}
	s.runningUID, s.runningSince = "", time.Time{}
	s.lastUID = uid
	s.failedAt = now
	s.failures++
	s.notBefore = now.Add(b.delayFor(s.failures))
	return true
}

// RecordRunning notes that the sandbox with uid is running. Once it has run
// for Stable, the failure count of its ordinal resets.
func (b *Backoff) RecordRunning(app string, ord int, uid string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.apps[app]
	if !ok {
		return
	}
	s, ok := m[ord]
	if !ok {
		return
	}
	if s.runningUID != uid {
		s.runningUID, s.runningSince = uid, now
	}
	if now.Sub(s.runningSince) >= b.Stable {
		s.failures = 0
		s.notBefore = time.Time{}
	}
}

// Pending returns the ordinals whose failed replica must wait, and the
// shortest remaining wait (0 when none waits). A failed replica at a revision
// other than revision waits only Base after its failure: the spec changed, so
// the replacement runs something else and need not wait out the old
// revision's count, but a spec that changes often still cannot replace a
// failing replica faster than once per Base.
func (b *Backoff) Pending(app string, instances []status.Instance, revision string, now time.Time) (map[int]bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[int]bool{}
	var shortest time.Duration
	for _, inst := range instances {
		if inst.Health != rollout.Terminal || inst.Deleting {
			continue
		}
		m := b.apps[app]
		if m == nil {
			continue
		}
		s := m[inst.Ordinal]
		if s == nil {
			continue
		}
		until := s.notBefore
		if inst.Revision != revision {
			if capped := s.failedAt.Add(b.Base); capped.Before(until) {
				until = capped
			}
		}
		if !now.Before(until) {
			continue
		}
		out[inst.Ordinal] = true
		if wait := until.Sub(now); shortest == 0 || wait < shortest {
			shortest = wait
		}
	}
	return out, shortest
}

// Forget drops all state for an app.
func (b *Backoff) Forget(app string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.apps, app)
	delete(b.rejected, app)
}

// RecordRejection records that a create for app was refused and returns how
// long to wait before trying again (Base, doubling up to Max) and whether
// this is the first refusal in a row.
func (b *Backoff) RecordRejection(app string) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rejected == nil {
		b.rejected = map[string]int{}
	}
	b.rejected[app]++
	n := b.rejected[app]
	return b.delayFor(n), n == 1
}

// ClearRejection forgets earlier refusals once a create for app succeeds.
func (b *Backoff) ClearRejection(app string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.rejected, app)
}
