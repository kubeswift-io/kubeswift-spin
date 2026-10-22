package cachemanager

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
)

const (
	digestA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	digestC = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

func id(source string) compiledcache.Identity {
	return compiledcache.Identity{Namespace: "team-a", Source: "ghcr.io/org/app@" + source, Runtime: "ghcr.io/rt@" + digestC, Architecture: "amd64",
		Repository: "registry.example.com/c/team-a/wasmtime-cache"}
}

func rec(name string, consumers ...cachev1alpha1.Consumer) *cachev1alpha1.SpinCompiledCache {
	r := &cachev1alpha1.SpinCompiledCache{}
	r.Name = name
	r.Status.Consumers = consumers
	return r
}

func TestConsumers(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	a, b := id(digestA), id(digestB)
	v := view{
		wants:   map[string]want{"hello": {id: a, pool: "warm"}, "other": {id: a}},
		apps:    map[string]bool{"hello": true, "other": true, "gone": true, "stuck": true},
		unknown: map[string]bool{},
	}
	got, pools, referenced := Consumers(rec(a.RecordName()), a.RecordName(), v, now)
	wantList := []cachev1alpha1.Consumer{{Name: "hello", Current: true, Pool: "warm"}, {Name: "other", Current: true}}
	if !referenced || !reflect.DeepEqual(got, wantList) || !reflect.DeepEqual(pools, []string{"warm"}) {
		t.Fatalf("got %+v pools %v referenced=%v", got, pools, referenced)
	}

	// hello moves to digest B: record A keeps it as a replaced consumer.
	v.wants["hello"] = want{id: b, pool: "warm"}
	got, pools, _ = Consumers(rec(a.RecordName(), wantList...), a.RecordName(), v, now)
	if got[0].Name != "other" || !got[0].Current || got[1].Name != "hello" || got[1].Current || got[1].ReplacedAt == nil || !got[1].ReplacedAt.Time.Equal(now) || got[1].Pool != "" || len(pools) != 0 {
		t.Fatalf("after the move: %+v pools %v", got, pools)
	}

	// A deleted SpinApp is dropped once it is no longer current.
	delete(v.apps, "hello")
	got, _, _ = Consumers(rec(a.RecordName(), got...), a.RecordName(), v, now)
	if len(got) != 1 || got[0].Name != "other" {
		t.Fatalf("after the deletion: %+v", got)
	}

	// A SpinApp whose executor is missing or invalid keeps its entry: it is
	// still current, with its pool.
	stuck := []cachev1alpha1.Consumer{{Name: "stuck", Current: true, Pool: "warm"}}
	v.unknown["stuck"] = true
	got, pools, referenced = Consumers(rec(b.RecordName(), stuck...), b.RecordName(), view{wants: map[string]want{}, apps: v.apps, unknown: v.unknown}, now)
	if !referenced || !reflect.DeepEqual(got, stuck) || !reflect.DeepEqual(pools, []string{"warm"}) {
		t.Fatalf("unknown executor: %+v pools %v referenced=%v", got, pools, referenced)
	}

	// Nobody wants record B any more.
	_, _, referenced = Consumers(rec(b.RecordName()), b.RecordName(), view{wants: map[string]want{}, apps: v.apps, unknown: map[string]bool{}}, now)
	if referenced {
		t.Error("an unwanted record is not referenced")
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	old := &metav1.Time{Time: now.Add(-48 * time.Hour)}
	recent := &metav1.Time{Time: now.Add(-time.Hour)}
	at := func(h int) *metav1.Time { return &metav1.Time{Time: now.Add(time.Duration(-h) * time.Hour)} }
	mk := func(name string, last *metav1.Time, consumers ...cachev1alpha1.Consumer) *cachev1alpha1.SpinCompiledCache {
		r := rec(name, consumers...)
		r.Status.LastReferencedAt = last
		r.CreationTimestamp = *old
		return r
	}
	records := map[string]*cachev1alpha1.SpinCompiledCache{
		"current":    mk("current", recent, cachev1alpha1.Consumer{Name: "hello", Current: true}),
		"previous":   mk("previous", old, cachev1alpha1.Consumer{Name: "hello", ReplacedAt: at(30)}),
		"older":      mk("older", old, cachev1alpha1.Consumer{Name: "hello", ReplacedAt: at(40)}),
		"retained":   mk("retained", recent, cachev1alpha1.Consumer{Name: "hello", ReplacedAt: at(50)}),
		"orphan":     mk("orphan", old),
		"other-prev": mk("other-prev", old, cachev1alpha1.Consumer{Name: "gone", ReplacedAt: at(30)}),
	}
	apps := map[string]bool{"hello": true}
	got := Expired(records, apps, 24*time.Hour, now)
	want := []string{"older", "orphan", "other-prev"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expired %v, want %v (current, previous and retained are kept)", got, want)
	}
}
