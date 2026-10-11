package rollout

// Property tests: a randomized model of the sandboxes a SpinApp owns, driven
// through Compute with random spec changes (some revisions broken), random
// scaling, partial health progress and deletions that complete later. Every
// reconcile must keep availability at least min(before, replicas), never
// create a name that is still taken, and, once the spec is stable and good,
// converge to spec.replicas current, ready, unlabeled replicas.

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// propertySeeds bounds the run time; raise it locally to search further.
const propertySeeds = 2000

type sb struct {
	name     string
	ord      int
	rev      string
	surge    bool
	health   Health
	deleting bool
}

type world struct {
	replicas int
	rev      string
	bad      map[string]bool
	sbs      map[string]*sb
	log      []string
	creates  int
}

func (w *world) view() []Instance {
	var out []Instance
	names := make([]string, 0, len(w.sbs))
	for n := range w.sbs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := w.sbs[n]
		out = append(out, Instance{Name: s.name, Ordinal: s.ord, Revision: s.rev, Health: s.health, Deleting: s.deleting, Surge: s.surge})
	}
	return out
}

func (w *world) avail() int {
	n := 0
	for _, s := range w.sbs {
		if !s.deleting && Available(s.health, true) {
			n++
		}
	}
	return n
}

func (w *world) state() string {
	var parts []string
	for _, i := range w.view() {
		h := []string{"P", "Run", "Rdy", "T"}[i.Health]
		f := ""
		if i.Surge {
			f += "S"
		}
		if i.Deleting {
			f += "D"
		}
		parts = append(parts, fmt.Sprintf("%s:%s:%s%s", i.Name, i.Revision, h, f))
	}
	return fmt.Sprintf("r=%d rev=%s [%s]", w.replicas, w.rev, strings.Join(parts, " "))
}

// step runs one reconcile and checks invariants.
func (w *world) step(t *testing.T, inBackoff map[int]bool) Plan {
	before := w.avail()
	p := Compute(Input{Replicas: w.replicas, Revision: w.rev, Instances: w.view(), ReadinessObservable: true, InBackoff: inBackoff})
	w.log = append(w.log, w.state()+fmt.Sprintf(" => C%v D%v W%v", p.Create, p.Delete, p.Waiting))
	for _, d := range p.Delete {
		if s := w.sbs[d.Name]; s != nil {
			s.deleting = true
		}
	}
	after := w.avail()
	if want := min(before, w.replicas); after < want {
		t.Fatalf("availability dropped %d -> %d (replicas %d)\n%s", before, after, w.replicas, strings.Join(w.log, "\n"))
	}
	for _, o := range p.Create {
		n := fmt.Sprintf("app-%d", o)
		if _, ok := w.sbs[n]; ok {
			t.Fatalf("create of existing name %s\n%s", n, strings.Join(w.log, "\n"))
		}
		w.sbs[n] = &sb{name: n, ord: o, rev: w.rev, surge: o == w.replicas}
		w.creates++
	}
	return p
}

func (w *world) env(r *rand.Rand, all bool) {
	for n, s := range w.sbs {
		if !all && r.Intn(3) != 0 {
			continue
		}
		switch {
		case s.deleting:
			delete(w.sbs, n)
		case s.health == Pending:
			s.health = Running
		case s.health == Running:
			if w.bad[s.rev] {
				s.health = Terminal
			} else {
				s.health = Ready
			}
		}
	}
}

func TestPropertyRolloutsKeepAvailabilityAndConverge(t *testing.T) {
	for seed := int64(0); seed < propertySeeds; seed++ {
		r := rand.New(rand.NewSource(seed))
		w := &world{replicas: 1 + r.Intn(3), rev: "r0", bad: map[string]bool{}, sbs: map[string]*sb{}}
		// Converge to the initial state.
		for i := 0; i < 20; i++ {
			w.step(t, nil)
			w.env(r, true)
		}
		nrev := 0
		for i := 0; i < 40; i++ {
			switch r.Intn(10) {
			case 0:
				nrev++
				w.rev = fmt.Sprintf("r%d", nrev)
				w.bad[w.rev] = r.Intn(4) == 0
			case 1:
				w.replicas = 1 + r.Intn(3)
			}
			w.step(t, nil)
			w.env(r, false)
		}
		// Settle with a good revision.
		nrev++
		if r.Intn(2) == 0 {
			w.rev = fmt.Sprintf("r%d", nrev)
		}
		w.bad[w.rev] = false
		w.creates = 0
		w.log = append(w.log, "---- settle")
		converged := false
		for i := 0; i < 60; i++ {
			p := w.step(t, nil)
			if len(p.Create) == 0 && len(p.Delete) == 0 && !p.Waiting && w.final() {
				converged = true
				break
			}
			w.env(r, true)
		}
		if !converged {
			t.Fatalf("seed %d: no convergence\n%s", seed, strings.Join(w.log, "\n"))
		}
		for _, s := range w.sbs {
			if s.surge || s.ord >= w.replicas || s.rev != w.rev || s.health != Ready {
				t.Fatalf("seed %d: bad final state %s\n%s", seed, w.state(), strings.Join(w.log, "\n"))
			}
		}
		if len(w.sbs) != w.replicas {
			t.Fatalf("seed %d: final count %s", seed, w.state())
		}
	}
}

func (w *world) final() bool {
	for _, s := range w.sbs {
		if s.surge || s.ord >= w.replicas || s.rev != w.rev || s.health != Ready || s.deleting {
			return false
		}
	}
	return len(w.sbs) == w.replicas
}

// Under a spec whose revision never becomes ready, once the first changes
// have played out, the planner deletes only failed sandboxes (no churn of
// sandboxes that still run), and availability does not drop.
func TestPropertyBrokenRevisionDoesNotChurn(t *testing.T) {
	for seed := int64(0); seed < propertySeeds; seed++ {
		r := rand.New(rand.NewSource(seed))
		w := &world{replicas: 1 + r.Intn(3), rev: "r0", bad: map[string]bool{}, sbs: map[string]*sb{}}
		for i := 0; i < 20; i++ {
			w.step(t, nil)
			w.env(r, true)
		}
		nrev := 0
		for i := 0; i < 30; i++ {
			switch r.Intn(8) {
			case 0:
				nrev++
				w.rev = fmt.Sprintf("r%d", nrev)
				w.bad[w.rev] = r.Intn(3) == 0
			case 1:
				w.replicas = 1 + r.Intn(3)
			}
			bo := map[int]bool{}
			for _, s := range w.sbs {
				if s.health == Terminal && r.Intn(2) == 0 {
					bo[s.ord] = true
				}
			}
			w.step(t, bo)
			w.env(r, false)
		}
		nrev++
		w.rev = fmt.Sprintf("r%d", nrev)
		w.bad[w.rev] = true
		w.log = append(w.log, "---- broken settle")
		nonTerminalDeletes := 0
		for i := 0; i < 80; i++ {
			pre := map[string]Health{}
			for n, s := range w.sbs {
				pre[n] = s.health
			}
			bo := map[int]bool{}
			for _, s := range w.sbs {
				if s.health == Terminal {
					bo[s.ord] = true // everything in backoff
				}
			}
			p := w.step(t, bo)
			if i > 20 {
				for _, d := range p.Delete {
					if pre[d.Name] != Terminal {
						nonTerminalDeletes++
					}
				}
			}
			w.env(r, true)
		}
		if nonTerminalDeletes > 0 {
			t.Fatalf("seed %d: churn of non-terminal sandboxes under broken revision\n%s", seed, strings.Join(w.log[len(w.log)-60:], "\n"))
		}
	}
}
