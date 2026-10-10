# startupbench

`startupbench` measures how long a SpinApp served by kubeswift-spin takes
to start, on a real KubeSwift cluster with KVM. It is the tool behind the
numbers in [docs/performance.md](../../../docs/performance.md) and the
release-time benchmark described there.

Without `-launcher-logs` it needs no log scraping or guest instrumentation:
every metric comes from Kubernetes watches and from HTTP requests that the
tool sends itself. `claim_to_dispatch` and `dispatch_to_direct_http` need
`-launcher-logs`.

## What it measures

For each run, `startupbench run`:

1. opens watches on the SpinApp, its SwiftSandbox (`<name>-0`), the pods in
   the namespace (it follows the pod that the new sandbox controls, by
   owner UID), the Service, its EndpointSlices and the Events, starting
   from the current resource version;
2. records the start time and creates the SpinApp;
3. records the first time each transition is delivered;
4. every `-interval` (50 ms by default), sends a new HTTP request
   (no keep-alive) to the pod IP on port 3000 ("direct") and, once the
   Service exists, to `http://<name>.<namespace>.svc:80<path>` ("Service");
5. stops when the direct and Service requests have both returned the
   expected response and the SpinApp is `Available`, waits one more second
   for trailing transitions, and prints one JSON object;
6. deletes the SpinApp and waits until the SpinApp, its sandboxes, their
   pods (including terminating ones) and the Service are gone, so that the
   next run cannot be answered by the previous sandbox.

The expected response is HTTP 200 with a non-empty body that contains
`-expect`, if set.

| Metric | From | To |
|---|---|---|
| `sandbox_created` | SpinApp create | SwiftSandbox created by kubeswift-spin |
| `direct_http` | SpinApp create | first expected response from the pod IP (direct first response) |
| `service_http` | SpinApp create | first expected response through the Service (Service first response) |
| `available` | SpinApp create | SpinApp `Available=True` (control-plane availability) |
| `pod_ready` | SpinApp create | sandbox pod `Ready` |
| `endpointslice_ready` | SpinApp create | the pod listed as ready in the Service's EndpointSlice |
| `slot_claim` | SwiftSandbox created | KubeSwift reports the warm slot checked out (warm runs only) |
| `sandbox_workload_ready` | SwiftSandbox created | SwiftSandbox `WorkloadReady=True` |
| `create_to_claim` | SpinApp create | warm slot checked out (warm runs only) |
| `claim_to_dispatch` | warm slot checked out | the launcher hands the workload to the guest (warm runs, `-launcher-logs`) |
| `claim_to_direct_http` | warm slot checked out | first direct response (warm runs only) |
| `dispatch_to_direct_http` | the launcher hands the workload to the guest | first direct response (warm runs, `-launcher-logs`) |
| `direct_to_available` | first direct response | SpinApp `Available` |
| `available_to_pod_ready` | SpinApp `Available` | pod `Ready` |
| `pod_ready_to_endpointslice` | pod `Ready` | EndpointSlice ready |
| `endpointslice_to_service_http` | EndpointSlice ready | first Service response |

Stage metrics can be negative when two components race; for example the
pod can become `Ready` a few milliseconds before kubeswift-spin reports
`Available`, because both follow the same KubeSwift signal.

`slot_claim` uses the earlier of the SwiftSandbox condition with reason
`CheckedOut` and the `CheckedOut` Event. It measures slot assignment only;
the workload starts in the slot afterwards.

Every timestamp is the time the benchmark pod observed the transition. Run
the benchmark pod on the node that runs the sandboxes (the script does) so
that HTTP probes and watch delivery share one clock. Watch delivery adds a
few milliseconds. The next request is sent `-interval` after the previous
one finished, and a request that fails can take up to 1 second (300 ms to
connect), so a response metric is late by at most `-interval` plus one
failed request.

Not measured: anything inside the guest (kernel boot, Spin startup, the
application pull). `-launcher-logs` adds marks parsed from the KubeSwift
launcher pod's logs: network setup, VM spawn, DHCP and probe start for
cold runs; for warm runs, whose slot pod started before the run, only the
lines after the run start: the action loop accepting the checkout's
workload (`action_accept`), handing it to the guest (`dispatch_sandbox_exec`,
the `launcher.dispatch` mark), probe start and workload ready. A launcher
log line carries the container runtime's timestamp, while slot claim is when
startupbench received `CheckedOut`, so `claim_to_dispatch` can be a few
milliseconds short.

With `-launcher-logs` each result also has `launcherLines`: for each
pattern, keyed by mark name, how many lines of the launcher pod's logs (the
`network-init`, `sandbox-materialize` and `launcher` containers, since the
pod started) matched. A warm slot serves one checkout, so a normal warm run
has `launcher.action-accept: 1` and `launcher.dispatch: 1`. KubeSwift
v0.16.1's pod watch messages are counted as `launcher.watch-unavailable`,
`launcher.watch-retry-failed`, `launcher.watch-unordered`,
`launcher.get-pod-error` and `launcher.watch-restored`, and action
rejections as `launcher.action-reject`. Any of the first four shows that the
watch failed and the launcher fell back to reading its pod every 2 seconds.
The patterns match KubeSwift v0.16.0 and v0.16.1 output, which is not an
API, and the option needs `pods/log` permission.

## Running the benchmark

`make perf-startup` runs [hack/perf-startup.sh](../../../hack/perf-startup.sh)
against the current kubeconfig context. It needs a cluster prepared as for
the [KVM e2e test](../../e2e/README.md) (KubeSwift v0.16.0 or v0.16.1, the
versions tested, because the warm-slot labels and launcher patterns are
specific to them; Spin Operator, kubeswift-spin, a Ready SwiftKernel
`sandbox` in the namespace),
`kubectl`, and either a startupbench image the node can pull or `docker`
and a registry to push one to:

```bash
make perf-startup PERF_NODE=<node> PERF_REGISTRY=ttl.sh
```

`ttl.sh` is an anonymous registry whose images expire after 24 hours; the
image contains only the startupbench binary, and the script runs it by the
digest it pushed. An image given in `PERF_IMAGE` must also be pinned by
digest.

The script runs `PERF_RUNS` (default 10) cold runs and then the same number
of warm runs, each mode in one Job pod on `PERF_NODE`, with the canonical
[examples/hello-http/spinapp.yaml](../../../examples/hello-http/spinapp.yaml).
The comment at the top of the script lists every variable.

- Cold runs use the executor `startupbench-cold`, which pins sandboxes to
  `PERF_NODE` with `spin.kubeswift.io/node-selector`.
- Warm runs use the executor `startupbench-warm` and the one-slot pool
  `startupbench-warm` (the controller's runtime image, 1 vCPU, 256Mi, the
  same node), created after the cold runs. Before each warm run
  startupbench waits until the slot's pod is at least 30 seconds old,
  because KubeSwift v0.16.0 and v0.16.1 can hand out a slot whose guest is
  not ready yet (see
  [compatibility.md](../../../docs/compatibility.md#known-upstream-issues)),
  and then a random time below 2 seconds
  (`-start-jitter`). KubeSwift v0.16.0 checks a slot for new work every 2
  seconds; without the jitter every run would start at about the same point
  of that cycle and the warm results would be biased. KubeSwift v0.16.1
  starts the work as soon as the API server delivers it; the jitter is kept
  so that results stay comparable across versions. A warm run that did not
  check out a slot is reported but excluded from the summary.

Cold runs measure a cold boot with the runtime image already cached on the
node; the first cold run after a runtime image change also includes the
image pull and is usually the slowest.

Results go to `PERF_OUT` (default `bin/perf/<UTC time>/`):

| File | Content |
|---|---|
| `environment.txt` | date, commit, node, kubelet, kernel, container runtime, controller images, runtime image, settings |
| `cold.jsonl`, `warm.jsonl` | one JSON object per run: marks and metrics in milliseconds |
| `cold.log`, `warm.log` | the Job logs |
| `summary.txt`, `summary.csv` | min, p50, p90, p95, max, mean and standard deviation per metric |

## Summaries and comparisons

```bash
go run ./test/perf/startupbench summarize bin/perf/<run>/cold.jsonl bin/perf/<run>/warm.jsonl
```

```bash
go run ./test/perf/startupbench summarize -baseline bin/perf/<old run>/cold.jsonl -baseline bin/perf/<old run>/warm.jsonl bin/perf/<run>/cold.jsonl bin/perf/<run>/warm.jsonl
```

`make perf-startup PERF_BASELINE=<directory>` does the same with every
`*.jsonl` file in that directory.

Percentiles interpolate linearly between the closest ranks. With
`-baseline`, the p50 and p95 of `direct_http`, `service_http`, `available`
and `slot_claim` are compared per mode, and a p50 or p95 that grew by more
than 25% and by more than 100 ms is marked `REGRESSION`. The comparison only means something when the hardware,
cluster, versions and registry conditions are comparable;
`environment.txt` records them.

## Permissions and cleanup

The benchmark pod runs as a ServiceAccount `startupbench` whose Role in the
benchmark namespace allows:

- SpinApps: list, watch, create; get and delete only for
  `startupbench-cold` and `startupbench-warm`
- SwiftSandboxes: get, list, watch
- Pods and Services: get, list, watch
- EndpointSlices and Events: list, watch
- with `PERF_LAUNCHER_LOGS=1` only: `pods/log` get

It has no direct access to Secrets or ConfigMaps. It can create SpinApps,
and a SpinApp can name any executor in the namespace, so run the benchmark
in a test namespace. startupbench deletes only a
SpinApp that carries the label `app.kubernetes.io/created-by: startupbench`,
which it sets on the SpinApps it creates; it refuses to run if another
SpinApp has the benchmark name. The label value is constant, and the
SpinApp names are fixed per mode (`startupbench-cold`, `startupbench-warm`),
so repeated runs do not add label values or kubeswift-spin metric series.

The script deletes the Jobs, SpinApps, executors, pool, ConfigMap, Role,
RoleBinding and ServiceAccount when it exits, unless `PERF_KEEP=1`.

## Running one measurement by hand

```bash
go run ./test/perf/startupbench run -h
```

`startupbench run` also works from a workstation with a kubeconfig, but the
direct probe then needs a route to pod IPs and the Service probe needs
cluster DNS, and the timestamps mix two clocks. Use the script or a pod on
the sandbox node for numbers you intend to compare.
