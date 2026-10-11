# KubeSwift requirement: sub-second sandbox readiness behind a Service

Status: implemented in KubeSwift v0.16.2
([#770](https://github.com/kubeswift-io/kubeswift/pull/770)): readiness
reaches the launcher pod in one write, and `spec.readinessStartup` is new.

## Problem

A SpinApp started from a warm SwiftSandboxPool answers its first direct
request about 400 ms after it is created when the application comes from a
local registry and the runtime image holds a Wasmtime compilation cache
entry for it (with the published runtime image and `ghcr.io`, about 1.5 s;
see [performance.md](../performance.md#current-results)). On KubeSwift
v0.16.1 the SpinApp Service routed to it only after 2.8 s p50 / 3.5 s p95.
The application was ready, and three mechanisms delayed it:

1. **Whole-second probe times.** The readiness probe cannot run before
   `initialDelaySeconds`, and `periodSeconds` is a whole number of seconds,
   so a workload ready at +310 ms after dispatch was found ready at about +1 s.
2. **Pod writes before the readiness gate.** The kubelet recomputes a pod's
   `Ready` condition only in a pod sync, and every sync first waits until its
   runtime cache is newer than the previous sync
   (`podCache.GetNewerThan(uid, lastSyncTime)`). The generic PLEG refreshes it
   once per relist (1 s, fixed). When another pod write was synced less than a
   relist earlier, the readiness gate waits for the next relist. Before
   readiness, a checkout made four such writes: the claim, the exec action, the
   `exec-status=running` annotation and the readiness annotation, which the
   controller then copied to the gate. Gate to `Ready` was 1.6 s p50.
3. **kube-proxy's `minSyncPeriod`** (iptables and nftables, default 1 s):
   syncs are at least that far apart, and the SpinApp's Service is created
   with the SpinApp, so its first sync starts the clock.

Plain-pod measurements on Kubernetes v1.34.3 (30 runs each, gate watch event
to `Ready` watch event):

| Pod writes before the gate is set | p50 | p95 |
|---|---|---|
| none for seconds | 26 ms | 34 ms |
| one label write 380 ms earlier | 34 ms | 415 ms |
| the old checkout sequence (claim, action, running, readiness annotation) | 1396 ms | 2013 ms |
| for comparison: kubelet exec readiness probe, period 1 s | 569 ms | 980 ms |

## KubeSwift changes (generic, not Spin-specific)

- **One pod write for readiness.** On a pod whose launcher reports readiness
  itself (`kubeswift.io/sandbox-readiness-report: pod-condition`, set when the
  pod is built), swiftletd writes the readiness verdict straight to the
  readiness-gate condition `sandbox.kubeswift.io/workload-ready` through
  `pods/status`. The controller derives `WorkloadReady` from that condition and
  no longer writes the gate on such pods.
- **No other pod write between claim and readiness.** A checkout without
  Secrets puts the exec action in the claim's own write. swiftletd no longer
  writes `exec-status=running` before a sandbox workload; the controller only
  acts on the final status.
- **Readiness belongs to one workload.** The condition message starts with
  `workload <action id>:` (the action id is the sandbox UID). The controller
  counts it only for the sandbox the slot runs now, and clears a True that
  names another workload. Warm slots are single-use, so a slot never runs a
  second workload, and swiftletd writes the latest verdict only, synchronously.
  When the workload ends, swiftletd sets the condition False.
- **Least privilege.** Only a launcher's own ServiceAccount gets `patch` on
  `pods/status` for its own pod, and only on a cluster where the chart
  installs the ValidatingAdmissionPolicy
  `kubeswift-sandbox-launcher-status-gate`. That policy lets the account
  change the `workload-ready` condition and nothing else in the pod's status.
  Without the policy, launchers keep reporting by annotation. The guest never
  has Kubernetes credentials; it can only make a probe pass or fail.
- **`spec.readinessStartup` (opt-in).** The readiness probe starts with the
  workload and is retried every `interval` (default 100 ms, 10 ms to 1 s) until
  it first passes, for at most `window` (default 30 s, 1 s to 10 m). Failures in
  that phase do not count toward `failureThreshold` and are not reported.
  After the first pass, or after the window, the probe runs every
  `periodSeconds` with the kubelet's thresholds. Kubernetes probe semantics are
  unchanged when the field is unset.

Alternatives considered:

- **Controller relay** (swiftletd writes `SwiftSandbox/status`, the controller
  writes the gate). This costs the same single pod write. A warm slot does not
  know its sandbox until it is claimed, so its account would need status
  rights on a SwiftSandbox granted at checkout. It also adds a second writer to
  the SwiftSandbox status.
- **Container readiness via a kubelet probe.** It recomputes `Ready` without a
  pod sync, but probe periods are whole seconds (569 ms p50 in the table).
- **EventedPLEG.** Alpha in v1.34. Its global cache timestamp moves every 5 s,
  so a gate-only sync can wait longer.
- **Kubernetes 1.36/1.37 `PLEGOnDemandRelist`.** It relists early only after a
  sync that performed runtime actions, so a gate-only sync still waits.

## kubeswift-spin change (to use the generic API)

The executor annotation `spin.kubeswift.io/readiness-startup` decides when
replicas get `spec.readinessStartup` (KubeSwift's 100 ms interval and 30 s
window defaults):

- `auto` (default): for the TCP check kubeswift-spin adds when a SpinApp
  sets no readiness check.
- `always`: also for a SpinApp's own readiness check, whose
  `initialDelaySeconds` then does not delay the first checks.
- `never`: for no check.

kubeswift-spin detects the field from the CRD schema; `always` is refused when
KubeSwift does not serve it, and `auto` falls back to whole-second checks.
The measurements below used an earlier experimental annotation with a 50 ms
interval.

## Results

Warm SpinApp starts on one node: hello-http from a local registry, a runtime
image holding a Wasmtime compilation cache entry for it, 1 vCPU / 256 MiB,
Linux 6.6.158 sandbox kernel, slots at least 30 s old. All values are ms
from SpinApp creation, p50 / p95.

| Population | runs | direct HTTP | readiness confirmed | Pod Ready | Service HTTP |
|---|---|---|---|---|---|
| v0.16.1 (baseline) | 30 | 412 / 443 | 1099 / 1126 | 2730 / 3223 | 2813 / 3476 |
| single write | 30 | 389 / 415 | 1062 / 1085 | 1132 / 1164 | 2466 / 2498 |
| single write + readinessStartup 50 ms | 30 | 387 / 458 | 407 / 475 | 622 / 1072 | 1151 / 1486 |
| same + kube-proxy minSyncPeriod 100 ms | 30 | 417 / 490 | 416 / 497 | 547 / 1035 | 825 / 1146 |
| final, same configuration | 50 | 438 / 508 | 449 / 521 | 593 / 904 | **827 / 1150** |

## Remaining floor

- **kube-proxy `minSyncPeriod`** (default 1 s): with a stock kube-proxy,
  creation to Service HTTP cannot go below about 1.1 s when the Service is
  created with the application. Lowering it is an operator choice. The
  upstream default trades CPU on large clusters for latency.
- **kubelet relist after the claim:** the claim is a pod write. A gate set
  within the next relist waits for it: 0 to 1 s, about 1 in 3 times. This is
  the p95 tail above. Removing it needs a kubelet change that lets a
  readiness-gate-only change skip `GetNewerThan` (an upstream Kubernetes
  issue was drafted and not filed).
- **A client's own retry spacing:** connection attempts made just before the
  endpoint was programmed did not fail fast in these measurements (curl
  reached its 1 s timeout), so a client that retries on a timeout adds up to
  its timeout to the first success.
