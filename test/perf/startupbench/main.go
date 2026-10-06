// Command startupbench measures how long a SpinApp served by kubeswift-spin
// takes to start, from one clock.
//
// startupbench run creates a SpinApp from a manifest, one run at a time. For
// each run it opens watches on the SpinApp, its SwiftSandbox, the sandbox's
// pod, the Service, its EndpointSlices and Events before creating the
// SpinApp, records when each transition is first delivered, and probes the
// application directly (pod IP) and through the Service every -interval. It
// prints one JSON object per run on standard output, then deletes the
// SpinApp and waits until everything it owned is gone.
//
// startupbench summarize reads that output and prints min, p50, p90, p95,
// max, mean and standard deviation per metric, and optionally compares the
// medians and p95 with an earlier run.
//
// Run startupbench in a pod on the node that runs the sandboxes so that
// watch delivery, HTTP probes and (with -launcher-logs) kubelet log
// timestamps all come from the same clock. test/perf/startupbench/README.md
// describes the metrics and hack/perf-startup.sh runs a complete benchmark.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(runMain(os.Args[2:]))
	case "summarize":
		os.Exit(summarizeMain(os.Args[2:], os.Stdout))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  startupbench run -n NAMESPACE -f SPINAPP.yaml -mode cold|warm [flags]
  startupbench summarize [-baseline OLD.jsonl] [-csv OUT.csv] RESULTS.jsonl...

Run "startupbench run -h" or "startupbench summarize -h" for the flags.`)
}
