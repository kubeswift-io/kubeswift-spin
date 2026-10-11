package controller

import (
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

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
	failed := []status.Instance{{Instance: rollout.Instance{Name: "a-0", Ordinal: 0, Revision: "r1", Health: rollout.Terminal}}}

	if !b.RecordFailure("ns/a", 0, "uid-1", "r1", now) {
		t.Fatal("first failure not recorded")
	}
	if b.RecordFailure("ns/a", 0, "uid-1", "r1", now) {
		t.Fatal("same sandbox counted twice")
	}
	pending, wait := b.Pending("ns/a", failed, "r1", now.Add(5*time.Second))
	if !pending[0] || wait != 5*time.Second {
		t.Fatalf("pending %v wait %s", pending, wait)
	}
	if pending, _ := b.Pending("ns/a", failed, "r1", now.Add(11*time.Second)); pending[0] {
		t.Fatal("backoff did not expire")
	}

	// A second, different failure doubles the delay.
	b.RecordFailure("ns/a", 0, "uid-2", "r1", now)
	if pending, _ := b.Pending("ns/a", failed, "r1", now.Add(15*time.Second)); !pending[0] {
		t.Fatal("second failure did not double the delay")
	}
	if _, d := b.Pending("ns/a", failed, "r1", now); d != 20*time.Second {
		t.Fatalf("wait %s", d)
	}

	// Running briefly does not reset the count; running for the stability
	// window does.
	b.RecordRunning("ns/a", 0, "uid-3", now)
	if pending, _ := b.Pending("ns/a", failed, "r1", now.Add(15*time.Second)); !pending[0] {
		t.Fatal("a brief Running observation reset the backoff")
	}
	b.RecordRunning("ns/a", 0, "uid-3", now.Add(11*time.Minute))
	if pending, _ := b.Pending("ns/a", failed, "r1", now.Add(11*time.Minute)); pending[0] {
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
		b.RecordFailure("ns/a", 0, uid, "r1", now)
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
	b.RecordFailure("ns/a", 0, "uid-long", "r1", now.Add(time.Hour))
	if d := b.apps["ns/a"][0].notBefore.Sub(now.Add(time.Hour)); d != 10*time.Second {
		t.Fatalf("delay after a long healthy run = %s", d)
	}
}

func TestBackoffStartsOverAtANewRevision(t *testing.T) {
	b := NewBackoff()
	now := time.Unix(1000, 0)
	for i, uid := range []string{"u1", "u2", "u3", "u4"} {
		b.RecordFailure("ns/a", 1, uid, "r2", now.Add(time.Duration(i)*time.Second))
	}
	b.RecordFailure("ns/a", 1, "u5", "r3", now.Add(10*time.Second))
	if got := b.apps["ns/a"][1].notBefore.Sub(now.Add(10 * time.Second)); got != b.Base {
		t.Fatalf("delay at a new revision = %s, want %s", got, b.Base)
	}
}

// A failed replica from an earlier revision waits Base after its failure,
// not its full backoff, so a fixed spec is not held back but a spec that
// changes often cannot churn faster than once per Base.
func TestBackoffOfAnOutdatedReplicaIsShort(t *testing.T) {
	b := NewBackoff()
	now := time.Unix(1000, 0)
	for i, uid := range []string{"u1", "u2", "u3", "u4"} {
		b.RecordFailure("ns/a", 0, uid, "r1", now.Add(time.Duration(i)*time.Second))
	}
	failed := []status.Instance{{Instance: rollout.Instance{Name: "a-0", Ordinal: 0, Revision: "r1", Health: rollout.Terminal}}}
	last := now.Add(3 * time.Second)
	if p, d := b.Pending("ns/a", failed, "r1", last.Add(time.Minute)); !p[0] || d != 20*time.Second {
		t.Fatalf("current revision: pending %v wait %s", p, d)
	}
	if p, d := b.Pending("ns/a", failed, "r2", last.Add(4*time.Second)); !p[0] || d != 6*time.Second {
		t.Fatalf("outdated revision: pending %v wait %s", p, d)
	}
	if p, _ := b.Pending("ns/a", failed, "r2", last.Add(b.Base)); p[0] {
		t.Fatal("outdated revision still waiting after Base")
	}
}

func TestRejectionBackoffGrowsAndClears(t *testing.T) {
	b := NewBackoff()
	if d, first := b.RecordRejection("ns/a"); d != b.Base || !first {
		t.Fatalf("first: %s %v", d, first)
	}
	if d, first := b.RecordRejection("ns/a"); d != 2*b.Base || first {
		t.Fatalf("second: %s %v", d, first)
	}
	b.ClearRejection("ns/a")
	if d, first := b.RecordRejection("ns/a"); d != b.Base || !first {
		t.Fatalf("after clear: %s %v", d, first)
	}
	b.Forget("ns/a")
	if len(b.rejected) != 0 {
		t.Fatal("forget left rejection state")
	}
}

// An admission webhook chooses the reason of its refusal; it is never echoed.
func TestRejectionReasonIsAFixedWord(t *testing.T) {
	err := &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: 403, Reason: "value-from-a-webhook"}}
	if r, ok := rejectionReason(err); !ok || r != "Forbidden" {
		t.Fatalf("got %q %v", r, ok)
	}
	err = &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: 422, Reason: "value-from-a-webhook"}}
	if r, ok := rejectionReason(err); !ok || r != "Invalid" {
		t.Fatalf("got %q %v", r, ok)
	}
	if _, ok := rejectionReason(apierrors.NewAlreadyExists(schema.GroupResource{}, "x")); ok {
		t.Fatal("AlreadyExists treated as a rejection")
	}
}
