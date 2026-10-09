package main

import (
	"bufio"
	"context"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// launcherPatterns map KubeSwift v0.16.0 and v0.16.1 launcher pod log lines
// to marks. They are diagnostics for KubeSwift's own stages: the boot stages
// of a cold run, and for a warm run (whose slot pod started before the run)
// the dispatch and probe lines. They depend on log output that is not an
// API, so a pattern that stops matching just leaves its mark out.
var launcherPatterns = []struct {
	key string
	re  *regexp.Regexp
}{
	{"launcher.network-init", regexp.MustCompile(`^network-init `)},
	{"launcher.materialize", regexp.MustCompile(`^sandbox-materialize `)},
	{"launcher.dnsmasq-started", regexp.MustCompile(`dnsmasq: started`)},
	{"launcher.intent-loaded", regexp.MustCompile(`intent_loaded`)},
	{"launcher.vmm-spawn", regexp.MustCompile(`spawning cloud-hypervisor`)},
	{"launcher.dhcp-discover", regexp.MustCompile(`DHCPDISCOVER`)},
	{"launcher.guest-ip", regexp.MustCompile(`guest_ip_discovered`)},
	{"launcher.probe-runner-started", regexp.MustCompile(`probe_runner_started`)},
	{"launcher.workload-ready", regexp.MustCompile(`workload_ready=true`)},
	// The action loop accepts the checkout's workload action, then hands it
	// to the guest agent ("dispatch").
	{"launcher.action-accept", regexp.MustCompile(`action_accept namespace=`)},
	{"launcher.dispatch", regexp.MustCompile(`dispatch_sandbox_exec id=`)},
	// Warnings of the action loop's pod watch (KubeSwift v0.16.1) and action
	// rejections; counted over the pod's whole log.
	{"launcher.watch-unavailable", regexp.MustCompile(`action_loop_watch_unavailable`)},
	{"launcher.watch-retry-failed", regexp.MustCompile(`action_loop_watch_retry_failed`)},
	{"launcher.watch-restored", regexp.MustCompile(`action_loop_watch_restored`)},
	{"launcher.watch-unordered", regexp.MustCompile(`action_loop_watch_unordered`)},
	{"launcher.get-pod-error", regexp.MustCompile(`action_loop_get_pod_err`)},
	{"launcher.action-reject", regexp.MustCompile(`action_reject_`)},
}

// launcherMarks reads the pod's container logs with kubelet timestamps,
// records the first matching line after Start for each pattern, and returns
// how many lines matched each pattern over the whole log (a warm slot's log
// starts before the run). The kubelet stamps the lines on the node, so the
// marks share the benchmark's clock only when startupbench runs on that node.
// Log lines are not stored.
func launcherMarks(ctx context.Context, kc kubernetes.Interface, ns, pod string, rec *recorder) map[string]int {
	counts := map[string]int{}
	if pod == "" {
		return counts
	}
	for _, c := range []string{"network-init", "sandbox-materialize", "launcher"} {
		rc, err := kc.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: c, Timestamps: true}).Stream(ctx)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if key, t, ok := matchLauncherLine(c, sc.Text()); ok {
				rec.markAt(key, t)
				counts[key]++
			}
		}
		_ = rc.Close()
	}
	return counts
}

// matchLauncherLine parses "<RFC3339 timestamp> <text>" from container c.
func matchLauncherLine(c, line string) (string, time.Time, bool) {
	ts, rest, ok := strings.Cut(line, " ")
	if !ok {
		return "", time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return "", time.Time{}, false
	}
	tagged := c + " " + rest
	for _, p := range launcherPatterns {
		if p.re.MatchString(tagged) {
			return p.key, t, true
		}
	}
	return "", time.Time{}, false
}
