package controller

import (
	"fmt"
	"testing"
	"time"

	"github.com/kubeswift-io/kubeswift-spin/internal/rollout"
	"github.com/kubeswift-io/kubeswift-spin/internal/status"
)

func TestDelayFor(t *testing.T) {
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := delayFor(i + 1); got != w {
			t.Fatalf("delayFor(%d) = %s, want %s", i+1, got, w)
		}
	}
}

func TestBackoffLifecycle(t *testing.T) {
	b := NewBackoff()
	now := time.Unix(1000, 0)
	failed := []status.Instance{{Instance: rollout.Instance{Name: "a-0", Ordinal: 0, Health: rollout.Terminal}}}

	if !b.RecordFailure("ns/a", 0, "uid-1", now) {
		t.Fatal("first failure not recorded")
	}
	if b.RecordFailure("ns/a", 0, "uid-1", now) {
		t.Fatal("same sandbox counted twice")
	}
	pending, retry := b.Pending("ns/a", failed, now.Add(5*time.Second))
	if !pending[0] || retry != "5s" {
		t.Fatalf("pending %v retry %q", pending, retry)
	}
	if pending, _ := b.Pending("ns/a", failed, now.Add(11*time.Second)); pending[0] {
		t.Fatal("backoff did not expire")
	}

	// A second, different failure doubles the delay.
	b.RecordFailure("ns/a", 0, "uid-2", now)
	if pending, _ := b.Pending("ns/a", failed, now.Add(15*time.Second)); !pending[0] {
		t.Fatal("second failure did not double the delay")
	}
	if d := b.NextDelay("ns/a", now); d < 20*time.Second || d > 21*time.Second {
		t.Fatalf("next delay %s", d)
	}

	// Running briefly does not reset the count; running for the stability
	// window does.
	b.RecordRunning("ns/a", 0, "uid-3", now)
	if pending, _ := b.Pending("ns/a", failed, now.Add(15*time.Second)); !pending[0] {
		t.Fatal("a brief Running observation reset the backoff")
	}
	b.RecordRunning("ns/a", 0, "uid-3", now.Add(11*time.Minute))
	if pending, _ := b.Pending("ns/a", failed, now.Add(11*time.Minute)); pending[0] {
		t.Fatal("running for the stability window did not reset backoff")
	}

	b.Forget("ns/a")
	if len(b.apps) != 0 {
		t.Fatal("forget left state")
	}
}

// TestBackoffEscalatesForCrashLoops covers the common failure: the guest
// reaches Running, then Spin fails to start, again and again.
func TestBackoffEscalatesForCrashLoops(t *testing.T) {
	b := NewBackoff()
	now := time.Unix(0, 0)
	var delays []time.Duration
	for i := 1; i <= 7; i++ {
		uid := fmt.Sprintf("uid-%d", i)
		b.RecordRunning("ns/a", 0, uid, now) // no-op before the first failure
		now = now.Add(5 * time.Second)
		b.RecordRunning("ns/a", 0, uid, now)
		b.RecordFailure("ns/a", 0, uid, now)
		delays = append(delays, b.apps["ns/a"][0].notBefore.Sub(now))
		now = b.apps["ns/a"][0].notBefore
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delays %v, want %v", delays, want)
		}
	}

	// A replica that ran past the stability window starts over at the base.
	b.RecordRunning("ns/a", 0, "uid-long", now)
	b.RecordRunning("ns/a", 0, "uid-long", now.Add(time.Hour))
	b.RecordFailure("ns/a", 0, "uid-long", now.Add(time.Hour))
	if d := b.apps["ns/a"][0].notBefore.Sub(now.Add(time.Hour)); d != 10*time.Second {
		t.Fatalf("delay after a long healthy run = %s", d)
	}
}
