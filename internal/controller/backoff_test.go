package controller

import (
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

	// Running again resets the count.
	b.RecordRunning("ns/a", 0)
	if pending, _ := b.Pending("ns/a", failed, now); pending[0] {
		t.Fatal("running did not reset backoff")
	}

	b.Forget("ns/a")
	if len(b.apps) != 0 {
		t.Fatal("forget left state")
	}
}
