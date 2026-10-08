package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

// Result is one run. Marks and Metrics are milliseconds; marks count from
// Start, the moment before the create request was sent.
type Result struct {
	Run        string             `json:"run"`
	Mode       string             `json:"mode"`
	SpinApp    string             `json:"spinApp"`
	Start      time.Time          `json:"start"`
	TimedOut   bool               `json:"timedOut"`
	CheckedOut bool               `json:"checkedOut"`
	Marks      map[string]float64 `json:"marks"`
	Metrics    map[string]float64 `json:"metrics"`
	// LauncherLines counts launcher log lines per pattern (-launcher-logs).
	LauncherLines map[string]int `json:"launcherLines,omitempty"`
}

// Mark names. Every mark is the first time the benchmark observed the
// transition, in milliseconds since Start.
const (
	markSandboxAdded   = "sandbox.added"
	markCheckedOut     = "sandbox.checkedOut"
	markWorkloadReady  = "sandbox.WorkloadReady=True"
	markDirectOK       = "direct.ok"
	markServiceOK      = "service.ok"
	markAvailable      = "spinapp.Available=True"
	markPodReady       = "pod.Ready"
	markEndpointsReady = "endpointslice.ready"
	markDispatch       = "launcher.dispatch"
)

// metric is the time from one mark to another; an empty From means Start.
type metric struct {
	Name, From, To, Help string
}

// metrics lists what summarize reports, in order. The first group counts
// from SpinApp creation; the second splits the path into stages.
var metrics = []metric{
	{"sandbox_created", "", markSandboxAdded, "SpinApp create -> SwiftSandbox created"},
	{"direct_http", "", markDirectOK, "SpinApp create -> first expected HTTP response from the pod IP"},
	{"service_http", "", markServiceOK, "SpinApp create -> first expected HTTP response through the Service"},
	{"available", "", markAvailable, "SpinApp create -> SpinApp Available=True"},
	{"pod_ready", "", markPodReady, "SpinApp create -> sandbox pod Ready"},
	{"endpointslice_ready", "", markEndpointsReady, "SpinApp create -> pod listed ready in the Service's EndpointSlice"},
	{"slot_claim", markSandboxAdded, markCheckedOut, "SwiftSandbox created -> warm slot checked out (warm only)"},
	{"create_to_claim", "", markCheckedOut, "SpinApp create -> warm slot checked out (warm only)"},
	{"claim_to_dispatch", markCheckedOut, markDispatch, "warm slot checked out -> workload handed to the guest agent (warm, -launcher-logs)"},
	{"claim_to_direct_http", markCheckedOut, markDirectOK, "warm slot checked out -> first direct response (warm only)"},
	{"dispatch_to_direct_http", markDispatch, markDirectOK, "workload handed to the guest agent -> first direct response (warm, -launcher-logs)"},
	{"sandbox_workload_ready", markSandboxAdded, markWorkloadReady, "SwiftSandbox created -> SwiftSandbox WorkloadReady=True"},
	{"direct_to_available", markDirectOK, markAvailable, "first direct HTTP response -> SpinApp Available"},
	{"available_to_pod_ready", markAvailable, markPodReady, "SpinApp Available -> pod Ready"},
	{"pod_ready_to_endpointslice", markPodReady, markEndpointsReady, "pod Ready -> EndpointSlice ready"},
	{"endpointslice_to_service_http", markEndpointsReady, markServiceOK, "EndpointSlice ready -> first Service HTTP response"},
}

// computeMetrics derives every metric whose marks are present. Stage
// metrics can be negative when two components race (for example the pod can
// become Ready just before kubeswift-spin reports Available).
func computeMetrics(marks map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for _, m := range metrics {
		to, ok := marks[m.To]
		if !ok {
			continue
		}
		from := 0.0
		if m.From != "" {
			if from, ok = marks[m.From]; !ok {
				continue
			}
		}
		out[m.Name] = to - from
	}
	return out
}

// Stats summarizes one metric over several runs.
type Stats struct {
	N                                    int
	Min, P50, P90, P95, Max, Mean, Stdev float64
}

// percentile interpolates linearly between the two closest ranks of sorted
// values (the method of numpy's default and of most spreadsheets).
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	k := float64(len(sorted)-1) * p
	lo := int(math.Floor(k))
	hi := min(lo+1, len(sorted)-1)
	return sorted[lo] + (sorted[hi]-sorted[lo])*(k-float64(lo))
}

func summarize(values []float64) Stats {
	if len(values) == 0 {
		return Stats{}
	}
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	s := Stats{N: len(v), Min: v[0], Max: v[len(v)-1], P50: percentile(v, .5), P90: percentile(v, .9), P95: percentile(v, .95)}
	for _, x := range v {
		s.Mean += x
	}
	s.Mean /= float64(len(v))
	if len(v) > 1 {
		for _, x := range v {
			s.Stdev += (x - s.Mean) * (x - s.Mean)
		}
		s.Stdev = math.Sqrt(s.Stdev / float64(len(v)-1))
	}
	return s
}

// readResults parses startupbench run output. Lines that are not JSON
// objects (for example kubectl noise in collected logs) are skipped.
func readResults(r io.Reader) ([]Result, error) {
	var out []Result
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var res Result
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			return nil, fmt.Errorf("parsing result: %w", err)
		}
		if res.Metrics == nil {
			res.Metrics = computeMetrics(res.Marks)
		}
		out = append(out, res)
	}
	return out, sc.Err()
}

func readResultFiles(paths []string) ([]Result, error) {
	var out []Result
	for _, p := range paths {
		f, err := os.Open(p) //nolint:gosec // the paths are command-line arguments
		if err != nil {
			return nil, err
		}
		rs, err := readResults(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, rs...)
	}
	return out, nil
}

// group is the set of runs summarized together.
type group struct {
	Mode     string
	Runs     int
	Excluded []string
	Stats    map[string]Stats
}

// groupByMode summarizes valid runs per mode. A run is excluded when it
// timed out, or when a warm run was not served from a warm slot (KubeSwift
// booted a fresh sandbox instead), because it would mix two populations.
func groupByMode(results []Result) []group {
	byMode := map[string]*group{}
	var order []string
	values := map[string]map[string][]float64{}
	for _, r := range results {
		g := byMode[r.Mode]
		if g == nil {
			g = &group{Mode: r.Mode, Stats: map[string]Stats{}}
			byMode[r.Mode] = g
			values[r.Mode] = map[string][]float64{}
			order = append(order, r.Mode)
		}
		switch {
		case r.TimedOut:
			g.Excluded = append(g.Excluded, r.Run+" (timed out)")
			continue
		case r.Mode == "warm" && !r.CheckedOut:
			g.Excluded = append(g.Excluded, r.Run+" (no warm slot checked out)")
			continue
		}
		g.Runs++
		for k, v := range r.Metrics {
			values[r.Mode][k] = append(values[r.Mode][k], v)
		}
	}
	out := make([]group, 0, len(order))
	for _, mode := range order {
		g := byMode[mode]
		for k, v := range values[mode] {
			g.Stats[k] = summarize(v)
		}
		out = append(out, *g)
	}
	return out
}

// A metric is flagged when its p50 or p95 grew by more than
// regressionThreshold against the baseline and by more than
// regressionFloorMs, so that noise in metrics of a few milliseconds (the
// slot claim) is not flagged. It is a warning, not a failure: results are
// only comparable on the same hardware, cluster and registry conditions.
const (
	regressionThreshold = 0.25
	regressionFloorMs   = 100
)

// compared is one metric of one mode against the baseline.
type compared struct {
	Mode, Metric       string
	OldP50, NewP50     float64
	OldP95, NewP95     float64
	DeltaP50, DeltaP95 float64 // fraction, (new - old) / old
	Regressed          bool
}

// headlineMetrics are compared against a baseline; the stage metrics are too
// small and too noisy for a percentage threshold.
var headlineMetrics = []string{"direct_http", "service_http", "available", "slot_claim"}

func compare(baseline, current []group) []compared {
	old := map[string]group{}
	for _, g := range baseline {
		old[g.Mode] = g
	}
	var out []compared
	for _, g := range current {
		b, ok := old[g.Mode]
		if !ok {
			continue
		}
		for _, name := range headlineMetrics {
			o, okOld := b.Stats[name]
			n, okNew := g.Stats[name]
			if !okOld || !okNew || o.N == 0 || n.N == 0 {
				continue
			}
			c := compared{Mode: g.Mode, Metric: name, OldP50: o.P50, NewP50: n.P50, OldP95: o.P95, NewP95: n.P95}
			c.DeltaP50 = ratio(n.P50, o.P50)
			c.DeltaP95 = ratio(n.P95, o.P95)
			c.Regressed = (c.DeltaP50 > regressionThreshold && n.P50-o.P50 > regressionFloorMs) ||
				(c.DeltaP95 > regressionThreshold && n.P95-o.P95 > regressionFloorMs)
			out = append(out, c)
		}
	}
	return out
}

func ratio(n, o float64) float64 {
	if o <= 0 {
		return 0
	}
	return (n - o) / o
}
