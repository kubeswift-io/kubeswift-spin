package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPercentileInterpolates(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	for _, tc := range []struct{ p, want float64 }{{0, 1}, {.5, 5.5}, {.9, 9.1}, {.95, 9.55}, {1, 10}} {
		if got := percentile(v, tc.p); !near(got, tc.want) {
			t.Errorf("p%v = %v, want %v", tc.p*100, got, tc.want)
		}
	}
	if got := percentile([]float64{7}, .95); got != 7 {
		t.Errorf("single value: %v", got)
	}
	if !math.IsNaN(percentile(nil, .5)) {
		t.Error("empty input must give NaN")
	}
}

func TestSummarize(t *testing.T) {
	s := summarize([]float64{4, 2, 8, 6})
	if s.N != 4 || s.Min != 2 || s.Max != 8 || s.P50 != 5 || s.Mean != 5 {
		t.Errorf("unexpected %+v", s)
	}
	if !near(s.Stdev, math.Sqrt(20.0/3)) {
		t.Errorf("sample stdev %v", s.Stdev)
	}
	if one := summarize([]float64{3}); one.Stdev != 0 || one.P95 != 3 {
		t.Errorf("one value: %+v", one)
	}
}

func TestComputeMetrics(t *testing.T) {
	m := computeMetrics(map[string]float64{
		markSandboxAdded: 40, markCheckedOut: 70, markDirectOK: 2500,
		markAvailable: 3300, markPodReady: 3280, markEndpointsReady: 3290, markServiceOK: 4000,
	})
	want := map[string]float64{
		"sandbox_created": 40, "slot_claim": 30, "direct_http": 2500, "available": 3300,
		"pod_ready": 3280, "endpointslice_ready": 3290, "service_http": 4000,
		"direct_to_available": 800, "available_to_pod_ready": -20,
		"pod_ready_to_endpointslice": 10, "endpointslice_to_service_http": 710,
	}
	for k, v := range want {
		if got, ok := m[k]; !ok || !near(got, v) {
			t.Errorf("%s = %v (present %v), want %v", k, got, ok, v)
		}
	}
	if _, ok := m["sandbox_workload_ready"]; ok {
		t.Error("a metric without its marks must be absent, not zero")
	}
}

func TestEveryMetricIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range metrics {
		if m.Name == "" || m.To == "" || m.Help == "" || seen[m.Name] {
			t.Errorf("bad or duplicate metric %+v", m)
		}
		seen[m.Name] = true
	}
	for _, h := range headlineMetrics {
		if !seen[h] {
			t.Errorf("headline metric %s is not defined", h)
		}
	}
}

func TestGroupByModeExcludesInvalidRuns(t *testing.T) {
	results := []Result{
		{Run: "cold-1", Mode: "cold", Metrics: map[string]float64{"direct_http": 9000}},
		{Run: "cold-2", Mode: "cold", Metrics: map[string]float64{"direct_http": 9400}},
		{Run: "cold-3", Mode: "cold", TimedOut: true, Metrics: map[string]float64{"direct_http": 1}},
		{Run: "warm-1", Mode: "warm", CheckedOut: true, Metrics: map[string]float64{"direct_http": 2500}},
		{Run: "warm-2", Mode: "warm", CheckedOut: false, Metrics: map[string]float64{"direct_http": 9000}},
	}
	g := groupByMode(results)
	if len(g) != 2 || g[0].Mode != "cold" || g[1].Mode != "warm" {
		t.Fatalf("groups %+v", g)
	}
	if g[0].Runs != 2 || len(g[0].Excluded) != 1 || g[0].Stats["direct_http"].P50 != 9200 {
		t.Errorf("cold %+v", g[0])
	}
	if g[1].Runs != 1 || len(g[1].Excluded) != 1 || g[1].Stats["direct_http"].Max != 2500 {
		t.Errorf("warm %+v", g[1])
	}
}

func TestCompareFlagsRegressionsAboveThreshold(t *testing.T) {
	mk := func(p50, p95 float64) []group {
		return []group{{Mode: "warm", Stats: map[string]Stats{"available": {N: 10, P50: p50, P95: p95}}}}
	}
	for _, tc := range []struct {
		p50, p95 float64
		want     bool
	}{
		{1240, 1500, false}, // +24% p50
		{1260, 1500, true},  // +26% p50
		{1000, 1900, true},  // +27% p95
		{500, 600, false},   // faster
	} {
		c := compare(mk(1000, 1500), mk(tc.p50, tc.p95))
		if len(c) != 1 || c[0].Regressed != tc.want {
			t.Errorf("p50 %v p95 %v: %+v, want regressed=%v", tc.p50, tc.p95, c, tc.want)
		}
	}
	small := []group{{Mode: "warm", Stats: map[string]Stats{"slot_claim": {N: 10, P50: 25, P95: 37}}}}
	grown := []group{{Mode: "warm", Stats: map[string]Stats{"slot_claim": {N: 10, P50: 25, P95: 53}}}}
	if c := compare(small, grown); len(c) != 1 || c[0].Regressed {
		t.Errorf("a growth of 16 ms must not be flagged: %+v", c)
	}
	if c := compare([]group{{Mode: "cold", Stats: map[string]Stats{"available": {N: 1, P50: 1}}}}, mk(5, 5)); len(c) != 0 {
		t.Errorf("modes without a baseline must not be compared: %+v", c)
	}
}

func TestReadResultsSkipsNoiseAndComputesMissingMetrics(t *testing.T) {
	in := strings.Join([]string{
		"pod/startupbench-cold created",
		`{"run":"cold-1","mode":"cold","marks":{"direct.ok":9000,"sandbox.added":20}}`,
		"",
	}, "\n")
	rs, err := readResults(strings.NewReader(in))
	if err != nil || len(rs) != 1 {
		t.Fatalf("results %v, err %v", rs, err)
	}
	if rs[0].Metrics["direct_http"] != 9000 || rs[0].Metrics["sandbox_created"] != 20 {
		t.Errorf("metrics %v", rs[0].Metrics)
	}
	if _, err := readResults(strings.NewReader("{broken")); err == nil {
		t.Error("a malformed result line must be an error")
	}
}

func TestMatchLauncherLine(t *testing.T) {
	key, ts, ok := matchLauncherLine("launcher", "2026-10-06T09:04:23.378286070+02:00 dnsmasq-dhcp: DHCPDISCOVER(br0) 2e:9a:b7:1d:0b:e0")
	if !ok || key != "launcher.dhcp-discover" || !ts.Equal(time.Date(2026, 10, 6, 7, 4, 23, 378286070, time.UTC)) {
		t.Errorf("got %q %v %v", key, ts, ok)
	}
	if key, _, ok := matchLauncherLine("network-init", "2026-10-06T09:04:17.296114329+02:00 Restricted egress: on br0"); !ok || key != "launcher.network-init" {
		t.Errorf("container-tagged pattern: %q %v", key, ok)
	}
	for _, line := range []string{"no timestamp", "2026-10-06T09:04:17Z unrelated text"} {
		if _, _, ok := matchLauncherLine("launcher", line); ok {
			t.Errorf("%q must not match", line)
		}
	}
}

func TestLoadSpinAppAppliesOverridesAndLabel(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := write("ok.yaml", "apiVersion: core.spinkube.dev/v1alpha1\nkind: SpinApp\nmetadata: {name: hello-http, namespace: other}\nspec: {image: a, executor: kubeswift, replicas: 1}\n")
	u, err := loadSpinApp(config{manifest: ok, namespace: "bench", name: "startupbench-warm", executor: "startupbench-warm"})
	if err != nil {
		t.Fatal(err)
	}
	ex, _, _ := unstructured.NestedString(u.Object, "spec", "executor")
	img, _, _ := unstructured.NestedString(u.Object, "spec", "image")
	if u.GetName() != "startupbench-warm" || u.GetNamespace() != "bench" || ex != "startupbench-warm" || img != "a" || u.GetLabels()[createdByLabel] != createdByValue {
		t.Errorf("unexpected object %v", u.Object)
	}
	for name, body := range map[string]string{
		"replicas.yaml": "apiVersion: core.spinkube.dev/v1alpha1\nkind: SpinApp\nmetadata: {name: x}\nspec: {replicas: 2}\n",
		"kind.yaml":     "apiVersion: v1\nkind: Pod\nmetadata: {name: x}\n",
	} {
		if _, err := loadSpinApp(config{manifest: write(name, body), namespace: "bench"}); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

// The repository's canonical manifest must load, so the Make target works.
func TestCanonicalManifestLoads(t *testing.T) {
	if _, err := loadSpinApp(config{manifest: "../../../examples/hello-http/spinapp.yaml", namespace: "bench"}); err != nil {
		t.Fatal(err)
	}
}

func TestReplicaOf(t *testing.T) {
	for _, tc := range []struct {
		sandbox string
		want    bool
	}{
		{"startupbench-cold-0", true},
		{"startupbench-cold-12", true},
		{"startupbench-cold-x-0", false},
		{"startupbench-cold-", false},
		{"startupbench-warm-0", false},
	} {
		if got := replicaOf(tc.sandbox, "startupbench-cold"); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.sandbox, got, tc.want)
		}
	}
}
