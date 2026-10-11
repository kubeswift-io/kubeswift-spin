# Compatibility

## Tested versions

Only the combinations below have been exercised. "Tested" names the test
that ran; nothing else is implied.

| Component | Version | How it was tested |
|---|---|---|
| kubeswift-spin | v0.1.0-rc5, v0.1.0-rc3, v0.1.0-rc2 | all tests below; KVM runs used a development build before rc2, then the published v0.1.0-rc2, v0.1.0-rc3 and v0.1.0-rc5 artifacts; the other KubeSwift v0.16.2 runs used a development build of v0.1.0-rc5 (commit 7cb6d55) |
| Spin | v4.2.1 | runtime image (`make runtime-test`), examples (`make example-test`), KVM e2e |
| spin-sdk (Rust, examples) | 7.0.0 | examples built with Rust 1.97.1 |
| Spin Operator | v0.6.1 | Go API and CRDs in envtest; operator installed in the kind integration test (`WITH_SPIN_OPERATOR=1`) and on the KVM e2e cluster |
| KubeSwift | v0.16.3 | KVM e2e, rolling updates under traffic, termination and `ingress-from` through deletion, re-creation under the same name, readiness startup, startup benchmark sanity check, with the release images (below); contract test of `internal/sandboxapi` against the v0.16.3 Go types and CRDs; v0.16.3 sandbox CRDs in envtest. The supported baseline |
| KubeSwift | v0.16.2 | KVM e2e, rolling updates under traffic, startup benchmark, readiness startup and warm-pool functional checks on the lab cluster with the release images (below); until v0.1.0-rc5, contract test and envtest CRDs at v0.16.2 |
| KubeSwift | v0.16.1 | KVM e2e, startup benchmark and warm-pool functional checks on the lab cluster (below); its sandbox API types and CRDs are unchanged from v0.16.0 (release diff), and the contract test then ran against v0.16.0 |
| KubeSwift | v0.16.0 | KVM e2e on a lab cluster (below); until v0.1.0-rc4, contract test of `internal/sandboxapi` and envtest CRDs at v0.16.0 |
| KubeSwift | v0.15.1 | legacy mode only: unit and envtest tests with a feature detector that reports no optional features. Legacy mode uses only the API subset that was contract-tested against v0.15.1 before the module moved to v0.16.0. Not run on a v0.15.1 cluster since the v0.16.0 integration. |
| cert-manager | v1.21.1 | KVM e2e cluster (the kind integration test pins v1.21.2) |
| Kubernetes API server | 1.34.1, 1.37.0 | envtest controller suite |
| Kubernetes (k0s) | v1.34.3, Calico CNI, linux/amd64 nodes | KVM e2e cluster |
| Architecture | linux/amd64 | everything above; arm64 images are built and published but not validated (never run) |
| Kubernetes (kind) | v1.34.0 | kind integration test |

### Startup latency

Newer results (KubeSwift v0.16.2, v0.1.0-rc5 code, three readiness
configurations) are in [performance.md](performance.md#current-results);
the figures below are kept for comparison.


Measured with `make perf-startup` on the lab cluster below, with the
v0.1.0-rc3 controller and runtime image and the canonical hello-http
SpinApp (readiness check with `initialDelaySeconds: 1`; seconds from
SpinApp creation; terms and method in [performance.md](performance.md)):

Cold, KubeSwift v0.16.0, 2026-10-06, 20 runs (the sandbox boots a microVM;
runtime image cached on the node; five cold runs on v0.16.1 had p50s within
0.04 seconds of these):

- first direct response: p50 9.92, p95 10.41
- first Service response: p50 12.99, p95 13.07
- SpinApp `Available`: p50 11.86, p95 12.11

Warm pool, KubeSwift v0.16.1, 2026-10-08, 20 runs:

- slot claim (from SwiftSandbox creation; not workload readiness): p50
  26 ms, p95 50 ms
- slot claim to workload start: p50 30 ms, p95 36 ms (1,034 ms p50 with
  v0.16.0)
- first direct response: p50 1.58, p95 2.35 (2.81 and 3.53 with v0.16.0)
- first Service response: p50 3.68, p95 4.59 (5.18 and 5.94 with v0.16.0)
- SpinApp `Available`: p50 3.19, p95 3.74 (4.16 and 4.89 with v0.16.0)

The `Available` durations in the run records below were measured by the
KVM e2e test (and, for the release-tag checks, by hand) with 3-second
polling. The hello-http and private-registry SpinApps had readiness checks
without `initialDelaySeconds`, so the Spin Operator default of 10 seconds
applied. Each value includes up to 3 seconds of polling delay. They are
control-plane availability times, not first-response times.

### KVM e2e lab run (2026-10-05)

Cluster: k0s Kubernetes v1.34.3, three linux/amd64 nodes, two of them
kernel nodes with KVM, Calico; KubeSwift v0.16.0, cert-manager v1.21.1,
Spin Operator v0.6.1. kubeswift-spin was a development build of commit
4e5f606 (controller `v0.1.0-dev.4e5f606`, runtime
`spin-4.2.1-r1-dev.4e5f606`) installed from `charts/kubeswift-spin`, with
the example artifacts tagged `v0.1.0-dev.49ded6b`.

`test/e2e/kvm-e2e.sh` with all nine phases (`E2E_WARM_POOL=1`,
`E2E_SCRATCH_REGISTRY=ttl.sh`) passed every check:

- hello-http became `Available` 26 seconds after the SpinApp was created
  (19 to 26 seconds over several runs, with the 10-second readiness
  default and 3-second polling; see
  [Startup latency](#startup-latency)) and answered through the
  SpinApp Service from a client pod. Scaling to 3, 2 and 1 replicas updated
  `readyReplicas` and the Service endpoints.
- A rolling update of two replicas took 104 seconds, with 0 failed
  requests out of 503 sent during it.
- Secret-backed variable, Secret-backed runtime-config option,
  `loadFromSecret` and `imagePullSecrets`: each value reached Spin, and a
  leak scan found none of them outside Secrets (objects, Events, logs; see
  [security-model.md](security-model.md#secrets)).
- serverless-ai reached an in-cluster mock through the egress allowlist; a
  restricted sandbox without the allowlist could not (the request timed
  out).
- A replica checked out from a warm pool was `Available` in 7 seconds (6
  to 7 seconds over several runs; this SpinApp had no readiness check, so
  a TCP check without delay applied, and the test polled every 3 seconds).
- Private registry: Spin pulled hello-http from an in-cluster htpasswd
  registry with `imagePullSecrets` (`Available` in 29 seconds); without the
  Secret the sandbox failed and the SpinApp never became ready.
- `ingress-from`: a labelled client was admitted, an unlabelled client got
  no connection, and relabelling the same pod flipped the result.
- Liveness: after the check started failing, the sandbox was `Failed`
  (`LivenessProbeFailed`) within 7 seconds and `readyReplicas` dropped to
  0. kubeswift-spin deleted it 10 seconds after the failure and the next
  failed replacement 20 seconds after its failure (backoff), recreating
  each once the foreground deletion finished (12 to 22 seconds). After
  the target recovered, the replacement was `Available` 48 seconds after
  recovery and stayed unchanged for 90 seconds. The controller logged no
  error.
- Teardown left no SpinApp, sandbox, Service or sandbox NetworkPolicy.

See [test/e2e](../test/e2e/README.md) for the phases.

### v0.1.0-rc2 from published artifacts (2026-10-05)

On the same lab cluster, after removing the previous installation (Helm
release, cluster role and binding, the `kubeswift-spin-system` and test
namespaces, executors and SpinApps; the SwiftKernel was recreated), v0.1.0-rc2
was installed with the README command from
`oci://ghcr.io/kubeswift-io/charts/kubeswift-spin` version `0.1.0-rc2`. The
controller ran the published digest, sandboxes booted the published runtime
digest (`spin-4.2.1-r1`, never used on the cluster before), and Spin pulled
the published example artifacts.

- hello-http applied from the release tag became `Available` in 25 seconds
  and answered through the Service. Scaled to 3 replicas on both kernel
  nodes, each replica answered directly and 9 of 9 requests through the
  Service succeeded; scaling back to 1 and deleting left no sandbox,
  Service or sandbox NetworkPolicy.
- `test/e2e/kvm-e2e.sh` from the tag, with all nine phases and the default
  (published) example artifacts, passed all 75 checks: time to `Available`
  19 seconds cold and 6 seconds from a warm pool (see the note above),
  rolling update with 0 failed requests out
  of 504, private registry `Available` in 31 seconds, no Secret value found
  outside Secrets, liveness replacement with the same timings as in the
  development-build run above.
- The controller logged no error or warning during the run, except one
  rejected LeaderElection Event at startup (the chart did not allow core
  Events in its namespace; fixed after rc2).

### v0.1.0-rc3 from published artifacts (2026-10-06)

Same cluster, after removing the v0.1.0-rc2 installation (Helm release,
cluster role and binding, both namespaces, executors, SpinApps and the
leader-election Lease; the SwiftKernel was recreated). v0.1.0-rc3 was
installed with the README command from
`oci://ghcr.io/kubeswift-io/charts/kubeswift-spin` version `0.1.0-rc3`
(chart digest `sha256:ad4ccf89...`); the controller ran the published
digest `sha256:7e9f65eb...`. The runtime image is the same digest as in
rc2, so nodes may have had it cached.

- The controller was ready 17 seconds after `helm install` and recorded
  the LeaderElection Event without an RBAC denial; after its pod was
  deleted, the new pod acquired the lease and recorded its own Event, again
  without a denial. The controller logged no warning or error during the
  whole validation.
- hello-http from the release tag became `Available` in 20 seconds; scaled
  to 3 replicas on both kernel nodes, each replica answered and 12 of 12
  requests through the Service succeeded; scaling to 1 and deleting left
  no sandbox, Service or NetworkPolicy.
- `test/e2e/kvm-e2e.sh` from the tag with all nine phases and the published
  example artifacts passed all 75 checks: time to `Available` 19 seconds
  cold (see the note above), rolling update with 0 failed requests out of
  504, no Secret value found outside Secrets, egress allowed and denied,
  ingress allowed and denied, liveness replacement as in rc2. The warm-pool
  phase took 38 seconds: the first checkout failed (see the warm-pool issue
  under [Known upstream issues](#known-upstream-issues)) and kubeswift-spin
  replaced the sandbox. The private registry phase took 51 seconds, of which
  27 were KubeSwift materializing the test runtime image that the test
  builds and pushes for every run.

### KubeSwift v0.16.1 (2026-10-08 and 2026-10-09)

Same cluster with KubeSwift v0.16.1 (release images, controller
`sha256:a9171962...`, swiftletd `sha256:9ff64351...`), v0.1.0-rc3 of
kubeswift-spin and Spin Operator v0.6.1. No warm pool existed before the
runs, so every launcher was created by v0.16.1.

- `test/e2e/kvm-e2e.sh` (2026-10-09) with all nine phases passed all 75
  checks. In phase
  4 the first checkout of the newly created pool failed with `ExecFailed`
  and kubeswift-spin replaced the sandbox, so `Available` took 35.7 seconds
  (see the warm-pool issue under
  [Known upstream issues](#known-upstream-issues)).
- Startup benchmark: see [Startup latency](#startup-latency). In all 20 warm
  runs the launcher started the workload 16 to 53 ms after the slot claim,
  exactly one workload action was accepted and dispatched per checkout, and
  no launcher logged a pod watch failure or a fallback to polling.
- Warm-pool checks with a SpinApp on a pool executor: scaling from 1 to 3
  replicas (two from warm slots, one booted cold when no slot was free) and
  back to 1, 12 of 12 requests through the Service, a rolling update served
  from a refilled slot, a new slot running about 2 seconds after a checkout
  (timed once), deleting the pool while a replica it had served kept running
  (the SpinApp reported `WarmPoolIncompatible` until the pool was recreated),
  and a new SpinApp served from the recreated pool. Launcher logs read after
  more than two watch resyncs showed no repeated action. Deleting the SpinApps
  left no sandbox, pod, Service, NetworkPolicy, Role or ServiceAccount.
- The kubeswift-spin controller logged no warning or error.

### KubeSwift v0.16.2 (2026-10-11)

Same cluster with KubeSwift v0.16.2 (release images, controller
`sha256:ab9efa8f...`, swiftletd `sha256:6ea72a05...`), SwiftKernel
`kernels/sandbox:6.6.16` (Linux 6.6.158), Spin Operator v0.6.1 and a
development build of kubeswift-spin v0.1.0-rc5 (commit 7cb6d55), with the
example artifacts of v0.1.0-rc4.

- `test/e2e/kvm-e2e.sh` with all nine phases passed all 75 checks; phase 6
  staged its test runtime image in a lab registry instead of a public
  scratch registry. The two-replica rolling update in phase 1 had no failed
  request (449 requests). In phase 4 the first checkout of the newly created
  pool took 35.1 seconds to `Available`, as in the v0.16.1 run (see the
  warm-pool issue under [Known upstream issues](#known-upstream-issues));
  the cause was not recorded this time.
- Examples beyond the e2e test: key-value (PUT and GET), outbound-http (an
  in-cluster target refused in `restricted` mode) and the experimental MCP
  server (`tools/list`) were `Available` and answered.
- Warm pool with one slot and a two-replica SpinApp: one replica checked out
  the slot, the other fell back to a cold sandbox, the Service answered, and
  the pool refilled to one warm slot.
- Readiness startup: see
  [performance.md](performance.md#readiness-startup-kubeswift-v0162) for the
  slow and never-ready application checks.
- `ingress-from`: a pod outside the peers is refused while a replica runs,
  but reaches a replica that is being deleted (see
  [security-model.md](security-model.md#network-isolation)).

### v0.1.0-rc5 from published artifacts (2026-10-11)

Same cluster with KubeSwift v0.16.2, upgraded in place from v0.1.0-rc4 with
`helm upgrade` from `oci://ghcr.io/kubeswift-io/charts/kubeswift-spin`
version `0.1.0-rc5` (chart digest `sha256:3bb53e32...`); the controller ran
the published digest `sha256:3a68ac37...` and logged version v0.1.0-rc5 at
commit 2c6cf5a. The runtime image is the `spin-4.2.1-r2` digest
`sha256:5af1c06c...` published with v0.1.0-rc4. The controller image, the
chart and the hello-http example verified with cosign v3.0.6 against the
release workflow identity of the tag; the runtime image verified against
the v0.1.0-rc4 tag that first built it.

- `test/e2e/kvm-e2e.sh` from the tag with phases 1, 2, 4 and 9 and the
  published example artifacts passed all 29 checks in 5 minutes: hello-http
  `Available` in 11.8 seconds cold, scaling to 3 and to 2 replicas, a
  two-replica rolling update with 0 failed requests out of 442, a
  Secret-backed variable, a warm-pool checkout (35.3 seconds to
  `Available`, the first checkout of a newly created pool as in the earlier
  runs) and teardown with nothing left in the namespace.
- A single-replica rolling update of the hello-http example from the tag,
  with requests through the Service the whole time: the surge replica was
  created first, the rollout settled in 87 seconds with the surge replica
  removed, and no request failed out of 8,544. Deleting the SpinApp left no
  sandbox.
- The controller logged no error.

### KubeSwift v0.16.3 (2026-10-11)

Same cluster upgraded to the KubeSwift v0.16.3 release: chart `0.16.3`
(`sha256:4c741057...`), controller `sha256:8ffb0b7b...`, swiftletd
`sha256:904295d6...`, sandbox-materialize `sha256:76d69da6...`, all signed
by the KubeSwift release workflow; the SwiftKernel stayed
`kernels/sandbox:6.6.16` (`sha256:4305c3b3...`, Linux 6.6.158); the
SwiftSandbox CRD has `spec.termination`.

The published kubeswift-spin v0.1.0-rc5, unchanged, first:

- The controller restarted cleanly on v0.16.3 and detected exposure,
  secrets, egress and readiness startup.
- `test/e2e/kvm-e2e.sh` passed every check of all nine phases: 23 in a run
  of phases 1 and 2, and 55 in a run of phases 3 to 9 (each run with its
  preflight checks), the second after the test's
  `http` helper was made to retry a connection refused right after an
  endpoint turned ready, a race in the test, not in kubeswift-spin). The
  first cold start after the upgrade took 16.8 seconds to `Available`
  (about 11 seconds afterwards): each node read the runtime image's layers
  once (cache repository authorization). The private registry phase passed.
- Rolling updates under traffic: see
  [Zero-loss rolling updates](#zero-loss-rolling-updates).
- Readiness startup: `auto`, `always` and `never` behaved as on v0.16.2,
  with fast, slow and never-ready applications (see
  [performance.md](performance.md#readiness-startup-kubeswift-v0162)).
- A SpinApp deleted and created again at once under the same name: the old
  sandbox drained under KubeSwift's termination finalizer and the new
  sandbox started its own launcher afterwards. rc5 briefly reported
  `SandboxConflict` meanwhile, which rc6 no longer does. With a plain
  SwiftSandbox whose object was removed while its launcher was still
  draining, the re-created sandbox waited with
  `Resolved=False/PreviousLauncherStopping` and then got its own launcher
  pod
  ([kubeswift-io/kubeswift#772](https://github.com/kubeswift-io/kubeswift/issues/772)
  fixed).
- `ingress-from` through deletion: see
  [security-model.md](security-model.md#network-isolation).

kubeswift-spin v0.1.0-rc6 code (a development build of commit 29d8bed)
then ran the same rolling update, isolation and re-creation scenarios
(rolling updates in [Zero-loss rolling updates](#zero-loss-rolling-updates),
isolation in [security-model.md](security-model.md#network-isolation)); the
re-created SpinApp waited for its predecessor's sandbox without reporting
`SandboxConflict`. `make verify-all` and `make kind-test WITH_SPIN_OPERATOR=1`
passed.

### Zero-loss rolling updates

KubeSwift v0.16.2, 2026-10-11. Method: an in-cluster client on another node
sent requests to the SpinApp Service for the whole run, eight concurrent
loops, a new connection per request and a 2-second timeout; any error,
timeout or unexpected response counts as failed. A watcher recorded pods,
EndpointSlices and SwiftSandboxes on the same clock. Lab cluster as above,
KubeSwift v0.16.2 release images, kube-proxy in iptables mode with default
settings, the hello-http example with its readiness check. Each rollout
changes the spec (a pod label) and ends when every replica is ready at the
new revision.

v0.1.0-rc5 code at commit 7cb6d55 (development build):

| Scenario | Requests | Failed |
|---|---:|---:|
| 3 rollouts, 1 replica, cold | 386,479 | 0 |
| 3 rollouts, 2 replicas, warm pool | 496,773 | 0 |
| 3 rollouts, 1 replica, a second spec change as soon as replica 0 was replaced | 519,313 | 0 |
| scaling 1, 3, 1, 3, 1 | 261,692 | 0 |
| a rollout to a revision that never starts, held 90 s, then a fix, 1 replica | 283,285 | 0 |
| the same with 2 replicas | 329,473 | 0 |
| 2 rollouts with a controller restart 15 s into each | 338,075 | 0 |

Earlier v0.1.0-rc5 code (commit 3979592, before the scale-down and backoff
changes): no failed request in 3,575,000 across 3 rollouts each with 1 and 2
replicas, cold and warm; second spec changes with 1 replica (3) and 2
replicas (2); scaling; controller restarts. One further run, not counted in
that total, lost 450 requests: it re-created the SpinApp under the name of
one deleted seconds earlier, and KubeSwift attached the new sandbox to the
old replica's terminating launcher pod
([kubeswift-io/kubeswift#772](https://github.com/kubeswift-io/kubeswift/issues/772),
see [Known upstream issues](#known-upstream-issues)). With the previous
SpinApp's launcher pods gone first, the same scenario (a revision that never
starts) had no failure (table above).

With a revision that never starts, the surge replica failed and was
retried with backoff while the old replicas kept serving (`Available=True`,
`Progressing=False` with `SandboxFailed`). For comparison, v0.1.0-rc4 on
the same cluster lost 68 of 105,634 requests in one single-replica rollout
and none with two replicas.

On KubeSwift v0.16.3 (2026-10-11), with the same method, KubeSwift drains
each deleted replica explicitly (see
[executor-contract.md](executor-contract.md#rolling-updates)). The client
now counts a failure only after the same worker (one of eight) has had a
success, so the start of the client pod itself is not counted; the published
v0.1.0-rc5 counted it, and 24 failures in two of its runs were all in the
first two seconds of the client, before any rollout.

| Scenario | v0.1.0-rc5 (published), requests / failed | v0.1.0-rc6 code (commit 29d8bed), requests / failed |
|---|---:|---:|
| 4 rollouts, 1 replica, cold | 236,053 / 0 | 243,327 / 0 |
| 3 rollouts, 2 replicas, cold | 290,805 / 0 | 287,267 / 0 |
| 3 rollouts, 3 replicas, cold | 379,969 / 0 | 387,680 / 0 |
| 3 rollouts, 2 replicas, warm pool | 176,031 / 0 | 169,586 / 0 |
| 3 rollouts, 1 replica, a second spec change as soon as replica 0 was replaced | 270,916 / 0 | 271,092 / 0 |
| scaling 1, 3, 1, 3, 1 | 132,760 / 0 | 188,625 / 0 |
| a revision that never starts, held 90 s, then a fix, 1 replica | 239,142 / 8 (client start) | 238,523 / 0 |
| the same with 2 replicas | 273,182 / 0 | 272,800 / 0 |
| 2 rollouts with a controller restart 15 s into each | 177,131 / 16 (client start) | 177,619 / 0 |

In every replacement of a serving replica (49 with rc5, 50 with rc6) the old
replica was deleted 1 to 276 ms after its replacement was ready, was marked
not ready (`Terminating=Draining`) within 91 ms of the deletion and left the
ready endpoints within 17 ms of its launcher pod's deletion (where the watch
recorded both), drained, and its launcher exited 0 5.4 to 6.8 seconds after
the deletion; the NetworkPolicy was removed 5 to 70 ms after the launcher
exited. With the broken revision the surge replica failed and was retried
with backoff while the old replicas kept serving and the SpinApp stayed
`Available`.

The results above, on KubeSwift v0.16.2, depended on its termination
behavior described under [Known upstream issues](#known-upstream-issues): a
deleted replica kept serving until its grace period ended. The KubeSwift
v0.16.3 runs below rely on its explicit drain instead.

### Not tested

- arm64 (images are built and published but not validated), other CNIs
  than Calico, other Kubernetes distributions, and more than one cluster.
- A Wasm component actively trying to read guest files or credentials; the
  credential boundary rests on Spin's capability model.
- Spin Operator `main` after v0.6.1. Its Go module path changed to
  `github.com/spinframework/spin-operator` and it adds the status fields
  `deploymentName` and `serviceName`; kubeswift-spin builds against the
  v0.6.1 module (`github.com/spinkube/spin-operator`).
- The KVM e2e workflow on a self-hosted runner (none is registered); the
  KVM test was run by hand.
- KubeSwift v0.15.1 on a cluster since the v0.16.0 integration.

## Required APIs

The controller checks these at startup and exits with an error naming the
missing API:

- `core.spinkube.dev/v1alpha1`: `spinapps`, `spinapps/status`,
  `spinappexecutors`
- `sandbox.kubeswift.io/v1alpha1`: `swiftsandboxes`

`sandbox.kubeswift.io/v1alpha1` `swiftsandboxpools` is optional and only
needed by executors that set `spin.kubeswift.io/sandbox-pool`.

The API server must also publish the OpenAPI v3 schema of
`sandbox.kubeswift.io/v1alpha1`: the controller does not start until it has
read the SwiftSandbox features from it (see below).

## KubeSwift features

kubeswift-spin reads the OpenAPI v3 schema of `sandbox.kubeswift.io/v1alpha1`
and uses a SwiftSandbox feature only when its fields are present. Exposure,
secrets and egress first appear in KubeSwift v0.16.0, readiness startup in
v0.16.2 and termination in v0.16.3. KubeSwift v0.16.3 or later is the
supported baseline: earlier releases lack the termination and launcher
ownership behavior described below.

| Feature | Detected when the schema has | Enables |
|---|---|---|
| exposure | `spec.network.ports`, `spec.readinessProbe` and `spec.podMetadata` | reaching replicas through the SpinApp Service, `readyReplicas`, `spec.checks`, `spec.podLabels`, `spin.kubeswift.io/ingress-from` |
| secrets | `spec.secretFiles` (the same release honors `env[].valueFrom.secretKeyRef`) | `secretKeyRef` variables and runtime-config options, `runtimeConfig.loadFromSecret`, `imagePullSecrets` |
| egress | `spec.network.egress` | `spin.kubeswift.io/egress-allow` |
| readiness startup | `spec.readinessStartup` | replicas reported ready about when they first answer; `spin.kubeswift.io/readiness-startup` (see [executor-contract.md](executor-contract.md#readiness-startup)) |
| termination | `spec.termination` | nothing to configure: kubeswift-spin reports the feature at startup and logs a message when it is missing. KubeSwift drains and stops deleted replicas itself (see below) |

KubeSwift v0.16.3 also changes behavior that kubeswift-spin relies on but
does not configure, so no schema field marks it:

- **Termination.** A deleted sandbox that exposes ports is taken out of
  Service endpoints, keeps running for a drain (`spec.termination.drainPeriod`,
  default 5 s), then has its microVM stopped; its launcher pod ends
  `Succeeded`. kubeswift-spin does not set `drainPeriod`: every replica
  exposes the Spin port, so the default applies, and leaving the spec
  unchanged means upgrading kubeswift-spin replaces no replica.
- **Isolation through termination.** The sandbox's NetworkPolicy is created
  before its launcher pod and kept until the launcher has stopped.
- **Launcher ownership.** A sandbox uses only a launcher pod it controls; a
  sandbox re-created under the name of one whose launcher is still stopping
  waits (`PreviousLauncherStopping`), and a live pod it does not control is
  reported as `LauncherPodConflict` instead of being adopted.
- **Cache repository authorization.** A node's cached runtime rootfs is used
  for a repository only after that repository has served the image once.
  After the upgrade, the first sandbox per runtime image repository on each
  node reads the image's layers once.

On KubeSwift v0.16.0 to v0.16.2, a deleted replica is not drained: it
keeps serving until its launcher's 30-second grace period ends, its
NetworkPolicy is removed while it can still serve, and a sandbox re-created
under the same name can take over the old launcher pod (see
[Known upstream issues](#known-upstream-issues)). On KubeSwift v0.16.0 and
v0.16.1, readiness startup is not detected:
readiness checks run at whole-second times, and
`spin.kubeswift.io/readiness-startup: always` makes the executor invalid. On
KubeSwift v0.15.1 none is detected. SpinApps then run without ports,
probes or launcher pod labels, report `Available=False` with reason
`NetworkUnavailable`, and Secret-backed configuration is rejected with
`UnsupportedConfiguration`. Detection is described in
[networking.md](networking.md#feature-detection).

## SpinApp field support

Every field of the v0.6.1 `SpinAppSpec` is classified below. A unit test
(`internal/compatibility`) fails if the upstream type gains a field that is
not classified, and checks that this table matches the code. "KubeSwift
v0.16.0" in a note means the field needs the feature detected above; on
older KubeSwift it is rejected.

A field marked unsupported or not applicable is never ignored: if it is set,
reconciliation stops for that SpinApp, `Progressing` becomes `False` with
reason `UnsupportedConfiguration`, and a Warning Event names the field.
Existing sandboxes keep running.

| Field | Support | Notes |
|---|---|---|
| `executor` | supported | Only executors labelled `spin.kubeswift.io/managed-by=kubeswift-spin` are realized. |
| `image` | supported | Passed to `spin up --from`. Must be a registry reference that starts with a registry host (write `docker.io/org/app:tag`, not `org/app:tag`) and is reachable from the sandbox; a private registry needs `imagePullSecrets`. |
| `replicas` | supported | One SwiftSandbox per replica, named `<app>-<ordinal>`. Bounded by `--max-replicas` (default 20). |
| `resources` | partially supported | `cpu` and `memory` size the microVM (see [executor-contract.md](executor-contract.md#cpu-and-memory)). Any other resource name is rejected. |
| `components` | supported | Passed as `spin up --component-id`, which Spin 4.2.1 marks experimental. |
| `variables` | partially supported | Literal values and `valueFrom.secretKeyRef` (KubeSwift v0.16.0) become `SPIN_VARIABLE_*` environment variables; a Secret is passed as a reference, never as a value. `configMapKeyRef`, `fieldRef` and `resourceFieldRef` are rejected. |
| `runtimeConfig` | partially supported | `keyValueStores`, `sqliteDatabases` and `llmCompute` are rendered into a runtime-config file; options may use `valueFrom.secretKeyRef` (KubeSwift v0.16.0). `loadFromSecret` (KubeSwift v0.16.0) delivers the Secret's `runtime-config.toml` key as a file and cannot be combined with the other fields. `configMapKeyRef`, non-empty literal credential options, URLs with embedded credentials and `llmCompute` type `spin` are rejected. |
| `checks` | supported | Become the sandbox readiness and liveness probes against the Spin HTTP port (KubeSwift v0.16.0). Without a readiness check, a TCP check is used. Only `httpGet` checks exist in SpinKube. On older KubeSwift they are accepted with a Warning Event but not enforced. With the executor annotation `spin.kubeswift.io/readiness-startup: always` (KubeSwift v0.16.2 or later), `readiness.initialDelaySeconds` is not applied and a `PartiallySupported` Warning Event says so. |
| `imagePullSecrets` | supported | Delivered as secret files to the guest, where Spin pulls the application (KubeSwift v0.16.0). The credentials are readable by the Spin process, not by Wasm components. |
| `enableAutoscaling` | unsupported | SpinApp has no scale subresource and there is no Deployment for an HPA or KEDA to target. |
| `invocationLimits` | partially supported | `memory` maps to `SPIN_MAX_INSTANCE_MEMORY` (bytes), as in Spin Operator. Other keys are rejected. |
| `serviceAccountName` | unsupported | A sandbox guest has no Kubernetes identity. |
| `serviceAnnotations` | supported | Applied by Spin Operator, which still creates the SpinApp Service when `createDeployment` is false. |
| `deploymentAnnotations` | not applicable | No Deployment exists. Spin Operator's webhook also rejects it for this executor type. |
| `podAnnotations` | not applicable | No application pod exists. Spin Operator's webhook also rejects it for this executor type. |
| `podLabels` | supported | Added to the KubeSwift launcher pod (KubeSwift v0.16.0). Keys under `kubeswift.io` or its subdomains and under `core.spinkube.dev` are rejected. |
| `volumes` | unsupported | KubeSwift mounts only Secret files and OCI artifacts into a sandbox, not Kubernetes volumes. |
| `volumeMounts` | unsupported | KubeSwift mounts only Secret files and OCI artifacts into a sandbox, not Kubernetes volumes. |

When exposure is detected, a SpinApp name longer than 52 characters is
rejected: the label `core.spinkube.dev/app.<name>.status` that the SpinApp
Service selects would exceed the 63-character limit of a label name.

## SpinAppExecutor field support

| Field | Handling |
|---|---|
| `createDeployment` | Must be `false`; `true` makes the executor invalid (`ExecutorInvalid`). |
| `deploymentConfig.otel` | Supported: copied to the sandbox as `OTEL_EXPORTER_OTLP_*` environment variables. |
| `deploymentConfig.installDefaultCACerts` | Accepted; the runtime image always contains a CA bundle. |
| `deploymentConfig.caCertSecret` | Rejected: not implemented. KubeSwift v0.16.0 could deliver the Secret as a file, but kubeswift-spin does not install it as a trust anchor yet. |
| `deploymentConfig.runtimeClassName` | Rejected: not applicable to sandboxes. |
| `deploymentConfig.spinImage` | Rejected: use the `spin.kubeswift.io/runtime-image` annotation. |

The profile annotations, including `spin.kubeswift.io/egress-allow` and
`spin.kubeswift.io/ingress-from`, are listed in
[executor-contract.md](executor-contract.md#executor-profiles).

## Known upstream issues

- **Spin Operator v0.6.1 rejects an `httpGet` check without `httpHeaders`.**
  Its defaulting webhook writes `httpHeaders: null`, and its own validation
  then refuses the SpinApp with a message saying that `httpHeaders` must be
  of type array. The API server rejects the SpinApp at admission, before
  any controller sees it, so kubeswift-spin cannot detect or correct it.
  Set `httpHeaders: []` explicitly:

  ```yaml
  spec:
    checks:
      readiness:
        httpGet:
          path: /healthz
          httpHeaders: []
  ```

- **KubeSwift v0.16.0 and v0.16.1 can hand out a warm-pool slot before its
  guest is ready.** A slot counts as warm, and can be checked out, as soon
  as its launcher container is running. A checkout in the first seconds
  after a slot is created can fail with `ExecFailed`; kubeswift-spin then
  replaces the sandbox after the failure backoff, and the replacement is
  usually served by the same pool. Observed twice, each time on the first
  checkout of a newly created pool: during the v0.1.0-rc3 validation
  (v0.16.0) and in the KVM e2e run of 2026-10-09 (v0.16.1). The v0.16.2 and
  v0.16.3 release notes list no change for it. In the e2e runs the first
  checkout of a newly created pool took 35.1 and 35.3 seconds to
  `Available` on v0.16.2 and 12.2 seconds on v0.16.3, against about 1.5
  seconds from an established pool. Create pools some time before relying
  on warm starts.
- **KubeSwift v0.16.0 and v0.16.1 log write conflicts as `Reconciler
  error`** at error level, although they retry them and converge (seen
  during the v0.16.1 benchmark runs)
  ([kubeswift-io/kubeswift#758](https://github.com/kubeswift-io/kubeswift/issues/758)).
  Do not alert on those lines alone.
- **KubeSwift v0.16.2 and earlier attach a new SwiftSandbox to a terminating
  launcher pod that has its name** (fixed in v0.16.3,
  [kubeswift-io/kubeswift#772](https://github.com/kubeswift-io/kubeswift/issues/772)).
  KubeSwift found a cold-started sandbox's launcher pod by the sandbox's
  name and did not check that the pod belonged to that sandbox. A SpinApp
  deleted and created again under the same name within 30 seconds was
  `Available` on the previous app's terminating replica, then had no ready
  replica until the replacement started. On those releases, delete SpinApps
  and SwiftSandboxes with `--cascade=foreground` before re-creating them
  under the same name.
- **KubeSwift v0.16.2 and earlier keep a deleted sandbox's guest running
  until the launcher's termination grace period ends, and remove its
  NetworkPolicy at once** (fixed in v0.16.3). swiftletd answered SIGTERM by
  pressing the guest's ACPI power button, which the guest ignores, so the
  launcher was killed after 30 seconds and ended `Failed`; meanwhile the
  replica's port was reachable from any pod in the cluster (see
  [security-model.md](security-model.md#network-isolation)). Rolling updates
  without failed requests on those releases depended on this accidental
  drain. KubeSwift v0.16.3 drains and stops deleted sandboxes explicitly
  (see [KubeSwift features](#kubeswift-features)).

## Upgrading upstream versions

- **Spin**: change the version and digests in `runtime/Dockerfile` and
  `hack/install-spin.sh`, then run `make runtime-test example-test`. See
  [runtime-image.md](runtime-image.md#updating-spin).
- **Spin Operator**: bump the Go module, run `make test`. The matrix test
  fails on new `SpinAppSpec` fields until they are classified here and in
  `internal/compatibility`.
- **KubeSwift**: bump the test-only Go module, run `make test` (the
  `internal/sandboxapi` contract test checks the fields kubeswift-spin uses)
  and the KVM e2e test. A new SwiftSandbox feature needs code changes and
  schema detection before it is used.
- **Upgrading KubeSwift on a cluster** from a version without a feature to
  one with it (for example v0.15.1 to v0.16.0) changes the rendered sandbox
  spec, so every replica is replaced once, one at a time. The controller
  re-reads the schema at most every `--capability-refresh` (default 5
  minutes) and renders the new spec at the next reconcile of each SpinApp;
  restarting the controller applies it at once.
- **Upgrading KubeSwift to v0.16.3** changes no SwiftSandbox that
  kubeswift-spin renders, so it replaces no replica. Apply the v0.16.3 CRDs
  as its upgrade notes say. Its controller adds finalizers to every
  SwiftSandbox and launcher NetworkPolicy; remove them before downgrading
  KubeSwift, as its release notes describe. The first start of the runtime
  image on each node after the upgrade reads the image's layers from the
  registry once (cache repository authorization), so it takes about as long
  as a pull.
- **Upgrading KubeSwift to v0.16.2** with kubeswift-spin v0.1.0-rc5 or
  later gives replicas without a readiness check `spec.readinessStartup`
  (with the default `auto`; every replica with `always`) and replaces them
  once (see the v0.1.0-rc5
  upgrade notes in the CHANGELOG). The chart does not move SwiftKernels to
  the v0.16.2 kernel artifact (`kernels/sandbox:6.6.16`, Linux 6.6.158);
  changing `spec.ociRef.image` pulls nothing until the kernel's pull Jobs
  are deleted, as the KubeSwift v0.16.2 upgrade notes describe. Running
  sandboxes keep the kernel they booted with until they are replaced.
