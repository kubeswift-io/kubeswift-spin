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
)

type ordinalState struct {
	failures  int
	lastUID   string
	notBefore time.Time
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

	mu   sync.Mutex
	apps map[string]map[int]*ordinalState
}

// NewBackoff returns an empty Backoff with the default delays.
func NewBackoff() *Backoff {
	return &Backoff{Base: backoffBase, Max: backoffMax, apps: map[string]map[int]*ordinalState{}}
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

// RecordFailure records that the sandbox with uid failed. It returns true the
// first time a given sandbox is recorded.
func (b *Backoff) RecordFailure(app string, ord int, uid string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state(app, ord)
	if s.lastUID == uid {
		return false
	}
	s.lastUID = uid
	s.failures++
	s.notBefore = now.Add(b.delayFor(s.failures))
	return true
}

// RecordRunning resets the failure count once a replica runs again.
func (b *Backoff) RecordRunning(app string, ord int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m, ok := b.apps[app]; ok {
		if s, ok := m[ord]; ok {
			s.failures = 0
			s.notBefore = time.Time{}
		}
	}
}

// Pending returns the ordinals whose failed replica must wait, and the
// shortest remaining wait formatted for a status message.
func (b *Backoff) Pending(app string, instances []status.Instance, now time.Time) (map[int]bool, string) {
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
		if s == nil || !now.Before(s.notBefore) {
			continue
		}
		out[inst.Ordinal] = true
		if wait := s.notBefore.Sub(now); shortest == 0 || wait < shortest {
			shortest = wait
		}
	}
	if len(out) == 0 {
		return out, ""
	}
	return out, shortest.Round(time.Second).String()
}

// NextDelay returns how long to wait before the next backoff expires.
func (b *Backoff) NextDelay(app string, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	var shortest time.Duration
	for _, s := range b.apps[app] {
		if wait := s.notBefore.Sub(now); wait > 0 && (shortest == 0 || wait < shortest) {
			shortest = wait
		}
	}
	if shortest == 0 {
		shortest = b.Base
	}
	return shortest + 100*time.Millisecond
}

// Forget drops all state for an app.
func (b *Backoff) Forget(app string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.apps, app)
}
