# Startup performance

This document defines the startup measures the project reports, explains
how they are measured, gives the current results from the KVM lab cluster
and shows where the time goes. The numbers describe one cluster; other
hardware, kernels and registries give other numbers.

Startup latency is paid when a replica is created: on deployment,
scale-out, a rolling update or the replacement of a failed replica. A
running replica keeps its microVM and its Spin process and serves every
request from them. Request latency is not covered here.

## Terms

**First-response latency**: the time from SpinApp creation until the
application returns the expected HTTP response. Two variants:

- *first direct response*: a request to the launcher pod IP on the Spin
  HTTP port (3000). It shows when the application can serve.
- *first Service response*: a request through the SpinApp Service, as an
  in-cluster client sends it. It also needs the pod to be `Ready` and listed
  as ready in the Service's EndpointSlice, and the node's Service routing
  to be updated.

**Control-plane availability latency**: the time from SpinApp creation
until the SpinApp condition `Available` is `True` (reason
`ApplicationReady`). kubeswift-spin sets it when the sandbox's readiness
probe has passed, so it depends on the readiness check's
`initialDelaySeconds` and `periodSeconds` (see
[Readiness configuration](#readiness-configuration)).

**Warm-pool slot-claim latency**: the time from SwiftSandbox creation until
KubeSwift reports that it assigned an already running slot of a
SwiftSandboxPool to it (`CheckedOut`). The workload starts in the slot
afterwards, so slot claim is not workload readiness.

**Spin runtime startup latency**: the time from the start of the Spin
process until its HTTP listener answers. For an application referenced
from a registry it includes the pull, which Spin performs at every start.

## How it is measured

[startupbench](../test/perf/startupbench/README.md) creates the SpinApp,
records when each transition is delivered by Kubernetes watches, and sends
a new HTTP request directly to the pod and through the Service 50 ms after
the previous one finished.
It runs in a pod on the node that runs the sandboxes, so all timestamps come
from one clock, and it deletes everything a run created before the next run
starts. `make perf-startup` runs 10 cold and 10 warm runs by default.

The durations printed by the KVM e2e test (`test/e2e/kvm-e2e.sh`) are
functional indicators: until 2026-10-06 it polled every 3 seconds, and the
"19 to 26 seconds" to `Available` reported for earlier releases in
[compatibility.md](compatibility.md) include that polling and Spin
Operator's default readiness delay of 10 seconds.

Spin runtime startup latency and the stages inside the guest are not
visible to startupbench. They were measured once, on 2026-10-06, with a
temporary build of the runtime entrypoint that printed timestamps and with
Spin's own tracing; that build is not part of the repository.

## Current results

Environment for both result sets below:

- cluster: k0s Kubernetes v1.34.3, Calico, containerd 1.7.30, Ubuntu 24.04
  with kernel 6.8; benchmark and sandboxes on one KVM node with 8 CPUs
  and 64 GiB
- Spin Operator v0.6.1, kubeswift-spin v0.1.0-rc3 (controller and runtime
  image, Spin 4.2.1)
- the canonical [hello-http SpinApp](../examples/hello-http/spinapp.yaml):
  1 vCPU, 256Mi, readiness check on `/healthz` with
  `initialDelaySeconds: 1` and `periodSeconds: 2`, application artifact
  `v0.1.0-rc3` pulled from `ghcr.io` by Spin at every start (by tag for
  KubeSwift v0.16.0; by that tag's digest, `sha256:5016dcb7...`, for
  v0.16.1)
- cold: the sandbox boots a microVM; the runtime image is already cached
  on the node
- warm: a one-slot SwiftSandboxPool with a slot started at least 30 seconds
  earlier, and a random start delay below 2 seconds (see the
  [startupbench README](../test/perf/startupbench/README.md#running-the-benchmark))

### Warm pool, KubeSwift v0.16.1 (2026-10-08)

KubeSwift v0.16.1 starts the workload in a checked-out slot as soon as the
API server delivers the checkout, instead of on the launcher's next 2-second
check (slots booted before an upgrade keep the 2-second check until they
are replaced). 20 warm runs, every launcher created by v0.16.1:

| Seconds from SpinApp creation | Min | p50 | p95 | Max |
|---|---:|---:|---:|---:|
| first direct response | 1.51 | 1.58 | 2.35 | 11.46 |
| first Service response | 3.30 | 3.68 | 4.59 | 14.11 |
| `Available` | 3.12 | 3.19 | 3.74 | 13.23 |

| Warm stage | Min | p50 | p95 | Max |
|---|---:|---:|---:|---:|
| slot claim (SwiftSandbox created to `CheckedOut`) | 16 ms | 26 ms | 50 ms | 55 ms |
| SpinApp creation to slot claim | 40 ms | 65 ms | 100 ms | 108 ms |
| slot claim to workload start (dispatch) | 16 ms | 30 ms | 36 ms | 53 ms |
| workload start to first direct response | 1,418 ms | 1,480 ms | 2,222 ms | 11,376 ms |

The maxima of the response, `Available` and post-dispatch rows come from one
run, whose slot claim to dispatch was 20 ms and whose time after dispatch,
inside the guest, was 11.4 seconds; the benchmark cannot see
which part of the guest path (Spin, the application pull) caused it.
Without that run, the first direct response is 1.57 seconds p50 and 1.81
seconds p95. "Dispatch" is the launcher's `dispatch_sandbox_exec` log line
(`-launcher-logs`); slot claim is when startupbench observed `CheckedOut`.
On v0.16.0, workload start is the launcher's `probe_runner_started` line
(the dispatch line was not recorded); on v0.16.1 that line follows dispatch
by under 2 ms, so the two are comparable.

Five cold runs on KubeSwift v0.16.1, a sanity check rather than a baseline,
had p50s within 0.04 seconds of the v0.16.0 cold results below (first direct
response 9.90 seconds p50).

### KubeSwift v0.16.0 (2026-10-06)

The cold results below are the current cold baseline; the warm results are
superseded by the v0.16.1 results above. 20 cold and 20 warm runs:

| Seconds from SpinApp creation | Cold min | Cold p50 | Cold p95 | Cold max | Warm min | Warm p50 | Warm p95 | Warm max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| first direct response | 9.09 | 9.92 | 10.41 | 11.02 | 1.76 | 2.81 | 3.53 | 3.65 |
| first Service response | 10.79 | 12.99 | 13.07 | 14.12 | 4.07 | 5.18 | 5.94 | 5.96 |
| `Available` | 9.92 | 11.86 | 12.11 | 13.03 | 3.22 | 4.16 | 4.89 | 5.00 |

Warm-pool slot claim (SwiftSandbox created to `CheckedOut`): 25 ms p50, 53 ms
p95 (17 to 58 ms). With v0.16.0 the workload started 1,034 ms p50 after the
slot claim (60 to 1,899 ms), because the launcher checked for new work every
2 seconds.

### From the first direct response to Service traffic

p50, milliseconds:

| Stage | Cold (v0.16.0) | Warm (v0.16.0) | Warm (v0.16.1) |
|---|---:|---:|---:|
| first direct response to `Available` | 1,882 | 1,577 | 1,617 |
| `Available` to pod `Ready` | 802 | 841 | 299 |
| pod `Ready` to EndpointSlice ready | 12 | 9 | 6 |
| EndpointSlice ready to first Service response | 275 | 215 | 134 |

Why `Available` to pod `Ready` was shorter in the v0.16.1 runs has not been
investigated.

`Available` follows the first direct response by the readiness probe
schedule: the probe starts when the guest has an address (cold) or the
workload starts (warm), the first probe runs after `initialDelaySeconds`
and then every `periodSeconds`. Cold, the application is usually not yet
listening at the first probe (1 second), so the second probe (3 seconds)
passes. In 2 of the 20 v0.16.0 cold runs the first probe passed and `Available`
came within 0.12 seconds of the first response. The pod becomes `Ready`
after the kubelet sees KubeSwift's readiness gate. Service traffic then
needs the EndpointSlice and the node's Service rules to be updated.

## Readiness configuration

When a check omits `initialDelaySeconds`, the Spin Operator CRD defaults it
to 10. The same startupbench, cluster and day (KubeSwift v0.16.0), with only
that field changed (10 runs per mode without the field, 20 with it):

| p50, seconds from SpinApp creation | Cold, no `initialDelaySeconds` (10) | Cold, `initialDelaySeconds: 1` | Warm, no `initialDelaySeconds` (10) | Warm, `initialDelaySeconds: 1` |
|---|---:|---:|---:|---:|
| first direct response | 9.98 | 9.92 | 2.79 | 2.81 |
| first Service response | 20.06 | 12.99 | 12.41 | 5.18 |
| `Available` | 18.87 | 11.86 | 11.40 | 4.16 |

The first direct response does not change: the application starts the same
way. The readiness delay only changes when the control plane and the
Service consider the replica ready. With the default, a hello-http replica
that could already answer was not `Available` until about 8.6 (warm) to
8.9 (cold) seconds after its first direct response, and received no
Service traffic for about 9.6 to 10.0 seconds after it; with
`initialDelaySeconds: 1` these gaps are 1.6 to 1.9 and 2.6 to 2.9 seconds
(medians of the per-run differences).

The examples therefore set `initialDelaySeconds: 1`. A probe that runs
before Spin listens fails and is retried after `periodSeconds`; the replica
stays not ready until a probe succeeds. In KubeSwift v0.16.0 the launcher
reports a readiness change only after `successThreshold` passes or
`failureThreshold` failures in a row, so a single early failure is not
reported. `0` is accepted by
Spin Operator v0.6.1 and passed through, but its Go type omits a zero
value, so a client that rewrites the SpinApp spec through that type
restores the default of 10. Applications that take longer to start should
use a delay close to their own startup time; `periodSeconds` sets how soon
after the application is ready a probe notices it.

## Where the time goes

Unless a paragraph says otherwise, these stages come from the 2026-10-06
investigation that preceded startupbench (20 cold and 20 warm runs with
launcher log timestamps, plus 5 cold and 10 warm runs with the
instrumented entrypoint); those runs are not in `performance-results/`.
They end at the first direct response and do not depend on the readiness
configuration. Medians in milliseconds.

### Cold path

| Stage | p50 |
|---|---:|
| kubeswift-spin creates the SwiftSandbox | 22 |
| KubeSwift creates the launcher pod (includes resolving the runtime image digest from its registry) | 777 |
| scheduler | 13 |
| kubelet starts the first init container | 776 |
| in-pod network setup (`network-init`) | 1,211 |
| root filesystem from the node cache (`sandbox-materialize`) | 353 |
| fixed one-second wait in the KubeSwift launcher script | 1,001 |
| launcher starts Cloud Hypervisor | 5 |
| guest kernel boot until its DHCP request (about 3.1 seconds of it without console output) | 3,660 |
| DHCP until the launcher sees the guest address | 337 |
| guest userspace, Spin start, application pull, first direct response | 1,163 |

Inside the guest (5 runs): from kernel start to the runtime entrypoint
3.4 seconds, entrypoint 5 ms, then Spin runtime startup latency of
1.5 seconds, of which 0.88 seconds is the pull from `ghcr.io` (DNS 7 ms,
TCP 9 ms, four connections).

### Warm path

With KubeSwift v0.16.0:

| Stage | p50 |
|---|---:|
| kubeswift-spin creates the SwiftSandbox | 23 |
| slot claim (`CheckedOut`) | 29 |
| KubeSwift starts the workload in the slot (the launcher checks for new work every 2 seconds) | 811 |
| workload start to entrypoint (vsock, guest agent) | 75 |
| entrypoint | 8 |
| Spin runtime startup, including the pull (0.95 seconds) | 1,559 |

The warm runs in `performance-results/2026-10-06/` also read the launcher
logs (`PERF_LAUNCHER_LOGS=1`) and give the same picture: from checkout to
workload start (the launcher's `probe_runner_started` line) 1,034 ms p50,
spread from 60 to 1,899 ms, and from workload start to the first direct
response 1,518 ms p50.

With KubeSwift v0.16.0 the warm path to the first response was mostly two
items: the launcher's 2-second check for new work and the application pull.
KubeSwift v0.16.1 removed the first: slot claim to workload start is 30 ms p50
(see [Current results](#warm-pool-kubeswift-v0161-2026-10-08)). What remains
is in the guest: workload start to the first direct response is 1,480 ms p50;
in the v0.16.0 investigation the application pull alone took about 0.95
seconds of the guest path. With the application in the guest instead of in a
registry, the same warm path answered 0.94 seconds sooner (10 runs each,
investigation data); that configuration is not supported by kubeswift-spin
today.

### Spin runtime startup baseline

Investigation data, not in `performance-results/`:

| Environment | Application from a local directory, p50 (p95) | Application from `ghcr.io`, p50 (p95) |
|---|---:|---:|
| Spin in a container on a lab node, 20 runs | 186 ms (267 ms) | 1,095 ms (3,600 ms) |
| Spin in the microVM, warm slot, 1 vCPU, 10 runs | 497 ms (550 ms) | 1,452 ms (1,798 ms) |

In the microVM, loading the component after the pull takes about 0.44
seconds against 0.19 seconds in a container, on one vCPU. Registry pulls
have a long tail: one container run in 20 took 7.4 seconds.

## Not covered by the benchmark

- A runtime image that is not yet cached on the node. The first cold start
  then includes the image pull: 27 to 36 seconds to the first direct
  response in 3 investigation runs with the image in `ttl.sh`.
- Request latency of a running replica, concurrent starts, many replicas,
  arm64, other hardware and other CNIs than Calico.

## Release-time benchmark

Before each release, run the benchmark on the lab cluster:

```bash
make perf-startup PERF_NODE=<node> PERF_REGISTRY=ttl.sh PERF_BASELINE=docs/performance-results/<previous-results>
```

- 10 cold and 10 warm runs (`PERF_RUNS`, default 10).
- Report p50 and p95 of the first direct response, the first Service
  response and `Available`, for cold and warm, and the warm slot claim.
- Compare with the previous release's results from a comparable
  environment: the same hardware, cluster, KubeSwift and Spin Operator
  versions, and the same registry for the application artifact.
  The script's `environment.txt` records most of these; add what it cannot
  detect, such as the CNI. `startupbench summarize -baseline` flags a p50
  or p95 that grew by more than 25% and by more than 100 ms.
- A flagged regression is investigated and explained in the release notes;
  it does not fail the release on its own. Registry and network conditions
  vary, so rerun before drawing conclusions.
- Keep each release's `environment.txt`, `summary.txt` and JSONL results in
  a directory under `docs/performance-results/`, named by date or version.

The KubeSwift v0.16.1 results are in
[performance-results/2026-10-08-kubeswift-v0.16.1](performance-results/2026-10-08-kubeswift-v0.16.1/);
the KubeSwift v0.16.0 results, and the runs without `initialDelaySeconds`,
are in [performance-results/2026-10-06](performance-results/2026-10-06/).
