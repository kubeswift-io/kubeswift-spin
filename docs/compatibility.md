# Compatibility

## Tested versions

Only the combinations below have been exercised. "Tested" names the test
that ran; nothing else is implied.

| Component | Version | How it was tested |
|---|---|---|
| kubeswift-spin | v0.2.1, v0.2.1-rc1, v0.2.0, v0.2.0-rc1, v0.1.1, v0.1.0 | v0.1.0: all tests below (see [Validation of v0.1.0](#validation-of-v010)); v0.1.1 adds the drain period, validated on KubeSwift v0.16.4 (see [Drain period on KubeSwift v0.16.4](#drain-period-on-kubeswift-v0164)); v0.2.0-rc1 adds compiled caches; its release artifacts passed the KVM e2e and the compiled-cache e2e on KubeSwift v0.19.1 (see [Validation of v0.2.0-rc1](#validation-of-v020-rc1)); v0.2.0 differs from v0.2.0-rc1 only in documentation and version strings; v0.2.1-rc1 passed every KVM e2e phase and the compiled-cache e2e on KubeSwift v0.19.3 (see [Validation of v0.2.1-rc1](#validation-of-v021-rc1)), and v0.2.1 differs from it only in documentation, version strings and test scripts |
| Spin | v4.2.5 | runtime image (`make runtime-test`), examples (`make example-test`); KVM e2e not yet run on v4.2.5 (v4.2.1: KVM e2e) |
| spin-sdk (Rust, examples) | 7.0.0 | examples built with Rust 1.97.1 |
| Spin Operator | v0.6.1 | Go API and CRDs in envtest; operator installed in the kind integration test (`WITH_SPIN_OPERATOR=1`) and on the KVM e2e cluster |
| KubeSwift | v0.19.3 | contract test of `internal/sandboxapi` against the v0.19.3 Go types and CRDs; v0.19.3 sandbox and placement CRDs in envtest. The required version for compiled caches from kubeswift-spin v0.2.1. With kubeswift-spin v0.2.1-rc1 on the lab cluster: every KVM e2e phase including the private registry, the compiled-cache e2e against a registry behind a private CA, and the startup benchmark (see [Validation of v0.2.1-rc1](#validation-of-v021-rc1)) |
| KubeSwift | v0.19.1 | compiled caches: the compiled-cache end-to-end test (seven examples, trust boundaries, restarts, teardown) and the compiled-cache startup benchmark on the lab cluster with a development build of v0.2.0-rc1 (see [Compiled caches on KubeSwift v0.19.1](#compiled-caches-on-kubeswift-v0191)); the KVM e2e (all phases but 6) and the compiled-cache e2e with the v0.2.0-rc1 release artifacts (see [Validation of v0.2.0-rc1](#validation-of-v020-rc1)); contract test of `internal/sandboxapi` against the v0.19.1 Go types and CRDs; v0.19.1 sandbox and placement CRDs in envtest, in kubeswift-spin v0.2.0. The required version for compiled caches in kubeswift-spin v0.2.0; v0.2.1 requires v0.19.3 |
| KubeSwift | v0.16.4 | drain period: default, `0s`, `10s` and a request longer than the drain, through rolling updates, scale-down and a broken replacement under traffic, `ingress-from` through a 10-second drain, invalid values; warm-pool checkout and refill on the lab cluster (see [Drain period on KubeSwift v0.16.4](#drain-period-on-kubeswift-v0164)); contract test of `internal/sandboxapi` against the v0.16.4 Go types and CRDs and v0.16.4 sandbox CRDs in envtest, in kubeswift-spin v0.1.1 (now v0.19.3). The recommended baseline without compiled caches |
| KubeSwift | v0.16.3 | KVM e2e, rolling updates under traffic, termination and `ingress-from` through deletion, re-creation under the same name, readiness startup and the startup benchmark on the lab cluster; contract test against the v0.16.3 Go types and CRDs in kubeswift-spin v0.1.0 (now v0.16.4). The minimum for the termination behavior kubeswift-spin relies on |
| KubeSwift | v0.16.0 to v0.16.2 | KVM e2e, startup benchmark and, on v0.16.2, rolling updates with earlier builds on the lab cluster; see [Known upstream issues](#known-upstream-issues) for what they lack |
| KubeSwift | v0.15.1 | legacy mode only: unit and envtest tests with a feature detector that reports no optional features. Legacy mode uses only the API subset that was contract-tested against v0.15.1 before the module moved to v0.16.0. Not run on a v0.15.1 cluster since the v0.16.0 integration. |
| cert-manager | v1.21.1 | KVM e2e cluster (the kind integration test pins v1.21.2) |
| Kubernetes API server | 1.34.1, 1.37.0 | envtest controller suite |
| Kubernetes (k0s) | v1.34.3, Calico CNI, linux/amd64 nodes | KVM e2e cluster |
| Architecture | linux/amd64 | everything above; arm64 images are built and published but not validated (never run) |
| Kubernetes (kind) | v1.34.0 | kind integration test |

### Validation of v0.1.0

Environment: a KVM lab cluster running Kubernetes v1.34.3 (k0s) with the
Calico CNI on linux/amd64 nodes, kube-proxy in iptables mode with default
settings, cert-manager v1.21.1, Spin Operator v0.6.1 and the KubeSwift
v0.16.3 release images, with the SwiftKernel artifact
`kernels/sandbox:6.6.16` (Linux 6.6.158). The end-to-end and
release-artifact checks used signed images, chart and example artifacts
that the release workflow built from a pre-release whose source differs
from v0.1.0 only in version strings; the long traffic runs used a
development build of the same source (it differs only in the wording of
one status message).

End-to-end test (`test/e2e/kvm-e2e.sh`): every check of all nine phases
passed: hello-http with scaling and a rolling update, Secret-backed
variables, serverless-ai with an egress allowlist and a Secret-backed LLM
token, a warm-pool checkout, `runtimeConfig.loadFromSecret`, a private
application registry through `imagePullSecrets`, `ingress-from`, liveness
failure and replacement, and teardown (in two runs). hello-http was
`Available` about 11 to 12 seconds after creation from a cold start; the
first checkout of a newly created warm pool took 12.0 seconds. Startup benchmarks are in
[performance.md](performance.md#current-results).

Release artifacts: the controller image, the chart, the runtime image and
the hello-http example verified with cosign against the release workflow
identity. After a clean install of the chart, the controller detected
exposure, secrets, egress, readiness startup and termination; the end-to-end phases 1, 2, 4
and 9 passed (29 checks), a single-replica rolling update under traffic had
no failed request, and the deletion left no SpinApp, SwiftSandbox, pool or
NetworkPolicy behind.

Readiness startup: `auto`, `always` and `never` behaved as documented with
fast, slow and never-ready applications (see
[performance.md](performance.md#readiness-startup-kubeswift-v0162)).

Re-creation under the same name: a SpinApp deleted and created again at
once waited for its predecessor's terminating sandbox, without reporting
`SandboxConflict`, and its new sandbox ran its own launcher pod. A plain
SwiftSandbox re-created while its predecessor's launcher was still draining
waited with `Resolved=False/PreviousLauncherStopping` and then got its own
launcher pod.

`ingress-from` through deletion: see
[security-model.md](security-model.md#network-isolation).

Earlier builds of kubeswift-spin were also run on KubeSwift v0.16.0,
v0.16.1 and v0.16.2 (end-to-end test, startup benchmark, rolling updates).
Those releases lack behavior the rolling updates and deletion rely on; see
[Known upstream issues](#known-upstream-issues).

### Zero-loss rolling updates

Method: an in-cluster client on another node sent requests to the SpinApp
Service for the whole run, eight concurrent loops, a new connection per
request and a 2-second timeout; any error, timeout or unexpected response
counts as failed, once that loop has had its first success. A watcher
recorded pods, EndpointSlices and SwiftSandboxes on the same clock. Each
rollout changes the spec (a pod label) and ends when every replica is ready
at the new revision. The hello-http example with its readiness check, on
KubeSwift v0.16.3, which drains each deleted replica explicitly (see
[executor-contract.md](executor-contract.md#rolling-updates)).

| Scenario | Requests | Failed |
|---|---:|---:|
| 4 rollouts, 1 replica, cold | 243,327 | 0 |
| 3 rollouts, 2 replicas, cold | 287,267 | 0 |
| 3 rollouts, 3 replicas, cold | 387,680 | 0 |
| 3 rollouts, 2 replicas, warm pool | 169,586 | 0 |
| 3 rollouts, 1 replica, a second spec change as soon as replica 0 was replaced | 271,092 | 0 |
| scaling 1, 3, 1, 3, 1 | 188,625 | 0 |
| a revision that never starts, held 90 s, then a fix, 1 replica | 238,523 | 0 |
| the same with 2 replicas | 272,800 | 0 |
| 2 rollouts with a controller restart 15 s into each | 177,619 | 0 |

In every replacement of a serving replica that the watch recorded (50;
scale-downs and replicas replaced before they were ready are not counted)
the old replica was deleted 9 to 276 ms after its replacement was ready,
was marked not ready (`Terminating=Draining`) within 91 ms of the deletion,
drained, and its launcher exited successfully 5.4 to 6.8 seconds after the
deletion; the NetworkPolicy was removed 5 to 70 ms after the launcher
exited. With the broken revision the surge replica failed and was retried
with backoff while the old replicas kept serving and the SpinApp stayed
`Available`.

### Drain period on KubeSwift v0.16.4

KubeSwift v0.16.4 release images (controller, swiftletd and
sandbox-materialize verified with cosign against the KubeSwift release
workflow; every launcher ran the signed swiftletd digest), the lab cluster
above, kubeswift-spin v0.1.0 for the baseline and a development build of
the drain-period change. The published outbound-http example served the
traffic: each `/fetch` called an in-cluster upstream that answered after a
set delay and recorded which replica called it, so requests in flight and
requests that reached a replica after its withdrawal could be counted.
Short requests (`/healthz`, four loops) ran alongside. Times are from the
old replica's deletion; "withdrawn" is its endpoint turning not ready.

| Case | `drainPeriod` in the spec | Withdrawn | Launcher exited | Requests (long / short) | Failed |
|---|---|---:|---:|---:|---:|
| unset, v0.1.0, rolling update, 3-second requests | unset | 46 ms | 5.4 s | 67 / 16,080 | 0 |
| unset, rolling update, 3-second requests | unset | 36 to 89 ms | 5.5 to 5.7 s | 134 / 30,844 (two runs) | 0 |
| SpinApp `0s`, rolling update, 1-second requests | `0s` | 33 ms | 0.5 s | 57 / 13,778 | 2 / 3 |
| SpinApp `10s` over executor `30s`, rolling update, 7-second requests | `10s` | 33 to 63 ms | 10.6 to 10.7 s | 163 / 36,022 (two runs) | 0 |
| SpinApp `10s`, rolling update, 20-second requests | `10s` | 37 ms | 10.7 s | 110 / 22,767 | 32 / 0 |
| SpinApp `10s`, 2 replicas, rolling update, 3-second requests | `10s` | 53 to 86 ms | 10.6 to 10.8 s | 118 / 23,096 | 0 |
| executor `10s`, scale-down 3 to 1 | `10s` | 52 to 85 ms | 10.8 to 12.0 s | 55 / 12,272 | 0 |
| executor `10s`, broken replacement held 90 s, 1 replica | `10s` | not deleted | | 208 / 45,319 | 0 |

- Without a setting, `spec.termination` stayed unset on the old and new
  replicas and KubeSwift applied its 5-second drain, as with v0.1.0.
- Every request in flight when a replica was withdrawn and shorter than
  its drain completed (6 per unset run, 14 per 10-second run with
  7-second requests). A few requests reached a replica up to 0.9 seconds
  after its withdrawal, before kube-proxy's next sync; they were served
  like the others (in the 20-second case they too outlasted the drain).
- With `0s` the replica stopped about half a second after its deletion: the
  2 requests in flight and 3 short requests in kube-proxy's sync window
  failed.
- With 20-second requests and a 10-second drain, the requests still running
  when the drain ended failed: the stopped microVM sends no reset, so the
  client saw no response and waited for its own timeout (30 seconds).
  Requests that could finish within the drain completed, and the replica
  and its sandbox were gone 11 seconds after the deletion.
- The scale-down deleted replicas 1 and 2 at once (replica 0 was ready and
  kept serving) and each drained for 10 seconds. With the broken
  replacement the old replica was never deleted, the SpinApp stayed
  `Available` and reported `Progressing=False` with `SandboxFailed`.
- `ingress-from` through a 10-second drain (two runs): a pod outside the
  peers got no answer from the old replica (0 of 164 and 0 of 160
  attempts), while an allowed pod was served until the drain ended (last
  answer 10.0 and 10.1 seconds after the deletion); the NetworkPolicy was
  removed with the launcher's exit.
- Invalid values on a SpinApp (`abc`, `-5s`, `11m`, `30`) and above an
  executor's `max-drain-period` gave `UnsupportedConfiguration` and a
  Warning Event and created no sandbox; `11m` on an executor gave
  `ExecutorInvalid`. A misspelled key added with `kubectl annotate` got a
  Warning Event, and correcting an invalid value by annotation alone
  created the replica.
- Warm pool (kubeswift-spin v0.1.0, KubeSwift v0.16.4): a new two-slot pool
  reported its slots as warming until their guests answered, then four
  checkouts, the first right after the pool was ready, all claimed a warm
  slot (`GuestRunning=True/CheckedOut`), the pool refilled after each, and
  no `ExecFailed`, `GuestAgentUnreachable` or `PoolColdFallback` Event
  occurred. SpinApp creation to `Available` was 1.88 and 1.98 seconds
  without a readiness check and 3.38 and 3.40 seconds with the example's
  check (client side, including the apply).

### Validation of v0.2.1-rc1

Environment: the lab cluster above with KubeSwift v0.19.3 and Spin
Operator v0.6.1, using the artifacts the release workflow published for the
tag v0.2.1-rc1. For the private registry tests, `test/e2e/private-registry.sh`
ran a registry with TLS from a CA created for the run and htpasswd
authentication, and KubeSwift's `sandboxMaterialize.registryCABundle` and
the signer's `compiledCache.signer.registryCABundle` held that CA. Lab
nodes are linux/amd64.

- Release artifacts: the files of the draft release matched `SHA256SUMS`;
  cosign v3.1.3 verified the controller and cache signer images, the chart
  and the seven example artifacts against the workflow identity of the
  tag, and the reused runtime image (`spin-4.2.1-r3`) against the release
  workflow identity; the chart pins the digests in `images.txt`.
- Upgrade and rollback: from v0.2.0 with `helm upgrade
  --reset-then-reuse-values`; the controller started as v0.2.1-rc1 and
  logged that compiled caches assume KubeSwift v0.19.3. `helm rollback` to
  v0.2.0 afterwards worked.
- `test/e2e/kvm-e2e.sh` with the v0.2.1-rc1 example artifacts passed every
  check (74) of all nine phases, the warm pool and the private registry
  (phase 6, against the registry behind the private CA, with the runtime
  image built inside the cluster) included. hello-http was `Available` 11.4
  seconds after creation from a cold start and 1.8 seconds from a warm
  slot.
- `test/e2e/compiled-cache-e2e.sh` in private mode (applications, the
  CA-trusting runtime image and the cache repository in that registry; the
  cache manager, signer and replicas pulling with credentials) passed every
  phase, over three runs (94 checks in phases 1 and 2, 50 in phases 3, 5
  and 4, 52 in phase 5 again after the checks below were tightened):
  - the seven examples compiled, exported, signed and placed, and their
    warm replicas mounted the caches (`Mounted/Cached`);
  - the trust boundaries of v0.2.0 held (`VerificationFailed` for an
    unsigned export and a signature with another key, `ClaimMismatch`,
    no attachment outside the cache repository), compilers referenced no
    Secret but the application's pull Secret, the registry password
    appeared in no sandbox spec or Event, and a SpinApp was `Available`
    while the signer was stopped;
  - restarts of the cache manager and signer during a compilation, and of
    the controller, replaced nothing and trusted nothing early;
  - a replica that booted cold (no free warm slot, `fetchTimeout: 0s`)
    mounted the cache its node held (`Mounted/Cached`, `Resolved=True`);
  - on a placement the API server refused (a quota), the SpinApp stayed
    `Available` with `CompiledCache=True/PlacementFailed`; with the record
    then claiming a prepared pool, a warm replica's cache was skipped
    before the claim (`ResolvePending`), the next one's after the claim
    (`NotOnNode`, the workload dispatched again), and a cold replica's at
    once (`FetchTimeout`, `Resolved=True`); all became ready, none was
    replaced, and no `DeliveryFailed` hold started;
  - a missing cache pull Secret (`PullSecretInvalid`) and a credential the
    registry refused (`ResolveRefused`): skipped, never pulled
    anonymously, and the replicas became ready.
- Startup benchmark: see
  [performance.md](performance.md#compiled-wasmtime-cache-kubeswift-v0193-2026-10-24).
- Not run: arm64 (no arm64 KVM hardware), a second cluster, a cold replica
  whose launcher pod starts late but within KubeSwift's authorization
  window, and `VerifyKeyInvalid` (kubeswift-spin never sets a per-artifact
  verify key Secret; caches are verified by the platform's policy).

### Validation of v0.2.0-rc1

Environment: the lab cluster above with KubeSwift v0.19.1 and Spin
Operator v0.6.1, using the artifacts the release workflow published for
the tag v0.2.0-rc1.

- Release artifacts: the files of the draft release matched `SHA256SUMS`;
  cosign v3.1.3 verified the controller and cache signer images, the
  chart and the seven example artifacts against the workflow identity of
  the tag, and the runtime image against the release workflow identity of
  a version tag; the packaged chart pins the controller, runtime and cache
  signer digests listed in `images.txt`.
- Upgrade: from v0.1.1 with `helm upgrade --reset-then-reuse-values`, as
  the README describes. The controller started as v0.2.0-rc1 and detected
  every SwiftSandbox feature, including compiled caches. `helm rollback`
  to v0.1.1 afterwards also worked.
- `test/e2e/kvm-e2e.sh` with the v0.2.0-rc1 example artifacts passed every
  check (64) of phases 1 to 5 and 7 to 9. hello-http was `Available` 16.4
  seconds after creation from a cold start and 2.1 seconds from a warm
  slot, the rolling update took 34.4 seconds, and after a liveness failure
  the SpinApp was `Available` again 26.7 seconds after its health check
  target recovered, including the replacement backoff. Phase 6
  (private registry) was not run: it pushes a test runtime image from the
  machine running the script to a scratch registry, and the lab registry
  accepts pushes only from inside the cluster (public registries such as
  `ttl.sh` were deliberately not used). The private registry path
  last ran on KVM for v0.1.0. Since then its translation moved unchanged
  into a function (`translate.RegistryAuth`), the entrypoint's handling of
  the credentials did not change, and the unit test of the rendered secret
  files (`TestImagePullSecretsBecomeSecretFiles`) is the same as in v0.1.0
  and passes.
- `test/e2e/compiled-cache-e2e.sh` with the release's chart, images and
  example artifacts (and request-info copied to the lab registry as
  `request-info-b`) passed every check (98) of all four phases: the seven
  examples mounted their signed caches from prepared nodes
  (`Mounted/Cached`), and the forged records were refused as in the
  development run below, including the signature with another key
  (`VerificationFailed`). The controller logged no error and replaced no
  replica for `compiled_cache_not_ready`.
- Release-time startup benchmark: v0.1.1 and then v0.2.0-rc1 on the same
  node, 10 cold and 10 warm runs each, with no regression flagged (see
  [performance.md](performance.md#release-time-benchmark-of-v020-kubeswift-v0191-2026-10-23)).

### Compiled caches on KubeSwift v0.19.1

Environment: the lab cluster above with KubeSwift v0.19.1, the SwiftKernel
artifact `kernels/sandbox:6.6.17`, a lab registry for the cache
repository, and a development build of kubeswift-spin whose compiled-cache
code matches v0.2.0-rc1 except for later review fixes (the narrowed
replacement of stuck replicas, `NoWarmPool`, and waking records that wait
for a compiler when a compile ends), which ran with the release artifacts
in [Validation of v0.2.0-rc1](#validation-of-v020-rc1). The end-to-end
script has since gained the `E2E_CONTEXT` check and cleanup changes.

- `test/e2e/compiled-cache-e2e.sh` passed every phase: seven examples
  (including multi-component) compiled, signed, placed on a warm pool and
  served from warm replicas that mounted the cache; trust boundaries (an
  unsigned export refused with `VerificationFailed`, another application's
  signed cache refused with `ClaimMismatch`, a repository outside the
  policy not attached, a signature with another key refused with
  `VerificationFailed`), tenant RBAC, and no key or credential in a
  compiler or replica; a restart of the cache manager and the signer during
  a compilation, which neither started a second compiler nor trusted the
  cache early; and removal of the placements with their consumers.
- Startup, hello-http, 30 warm starts per case: see
  [performance.md](performance.md#compiled-wasmtime-cache-kubeswift-v0191-2026-10-23).
- Not run on KVM: the replacement of a replica stuck after a skipped cache
  (unit tests only; the placement gate prevented the case in both runs), a
  cold fetch
  timeout above `0s` other than the 3 and 20 seconds tried by hand, arm64.

### Not tested

- On KubeSwift v0.16.3: a launcher pod on an unreachable node (KubeSwift's
  release 30 seconds past the grace period), `LauncherPodConflict`, a
  force-deleted launcher pod, and downgrading KubeSwift after removing its
  finalizers. The `SandboxConflict` after 2 minutes plus the drain period
  for a terminating same-named sandbox ran only in envtest.
- The drain-period annotations on a KubeSwift v0.16.3 cluster (run only on
  v0.16.4), their rejection on KubeSwift v0.16.0 to v0.16.2 (unit and
  envtest only), and drain periods other than 0s and 10s, in particular
  minutes-long drains with node drains and ResourceQuota.
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
v0.16.2 and termination in v0.16.3. KubeSwift v0.16.3 is the minimum and
v0.16.4 or later the recommended baseline: earlier releases lack the termination and launcher
ownership behavior described below.

| Feature | Detected when the schema has | Enables |
|---|---|---|
| exposure | `spec.network.ports`, `spec.readinessProbe` and `spec.podMetadata` | reaching replicas through the SpinApp Service, `readyReplicas`, `spec.checks`, `spec.podLabels`, `spin.kubeswift.io/ingress-from` |
| secrets | `spec.secretFiles` (the same release honors `env[].valueFrom.secretKeyRef`) | `secretKeyRef` variables and runtime-config options, `runtimeConfig.loadFromSecret`, `imagePullSecrets` |
| egress | `spec.network.egress` | `spin.kubeswift.io/egress-allow` |
| readiness startup | `spec.readinessStartup` | replicas reported ready about when they first answer; `spin.kubeswift.io/readiness-startup` (see [executor-contract.md](executor-contract.md#readiness-startup)) |
| termination | `spec.termination` | `spin.kubeswift.io/drain-period` on a SpinApp or executor (optional; KubeSwift's 5-second drain applies without it). kubeswift-spin reports the feature at startup and, on KubeSwift v0.16.0 to v0.16.2 (exposure detected, termination not), logs that KubeSwift is older than v0.16.3; the annotation makes the executor invalid, or the SpinApp unsupported, there. KubeSwift drains and stops deleted replicas itself (see below) |

KubeSwift v0.16.3 also changes behavior that kubeswift-spin relies on but
does not configure, so no schema field marks it:

- **Termination.** A deleted sandbox that exposes ports is taken out of
  Service endpoints, keeps running for a drain (`spec.termination.drainPeriod`,
  default 5 s), then has its microVM stopped; its launcher pod ends
  `Succeeded`. kubeswift-spin sets `drainPeriod` only when the SpinApp or
  its executor has the `spin.kubeswift.io/drain-period` annotation;
  otherwise every replica, which exposes the Spin port, gets the default,
  and the spec, and so every replica, stays unchanged.
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

## SpinApp annotations

kubeswift-spin reads two annotations on a SpinApp. An invalid value stops
reconciliation like an unsupported field (`UnsupportedConfiguration`). Any
other annotation under `spin.kubeswift.io/` is reported in a Warning Event
and otherwise ignored, so SpinApps that reconciled before keep doing so.

| Annotation | Effect |
|---|---|
| `spin.kubeswift.io/compiled-cache` | `disabled` opts the SpinApp out of the compiled Wasmtime cache its executor enables. `enabled` changes nothing: a SpinApp can only opt out, and on an executor that does not enable compiled caches it gives a Warning Event. See [compiled-cache.md](compiled-cache.md). |
| `spin.kubeswift.io/drain-period` | The replicas' `spec.termination.drainPeriod`, overriding the executor's annotation of the same name; a duration with units from `0s` to the executor's `spin.kubeswift.io/max-drain-period` (default `10m`). Needs KubeSwift v0.16.3 or later. See [executor-contract.md](executor-contract.md#drain-period). |

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
  guest is ready** (`ExecFailed` not observed on v0.16.2 or v0.16.3, and
  not listed as fixed). A slot counts as warm, and can be checked out, as soon
  as its launcher container is running. A checkout in the first seconds
  after a slot is created can fail with `ExecFailed`; kubeswift-spin then
  replaces the sandbox after the failure backoff, and the replacement is
  usually served by the same pool. Observed twice, each time on the first
  checkout of a newly created pool (on v0.16.0 and on v0.16.1). The v0.16.2
  and v0.16.3 release notes list no change for it. In the e2e runs the
  first checkout of a newly created pool took about 35 seconds to
  `Available` on v0.16.2 and 12 seconds on v0.16.3, against about 3.1
  seconds p50 to `Available` (1.6 without a readiness check) from an
  established pool in the startup benchmark. Create pools some time before
  relying on warm starts.
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
- **Upgrading KubeSwift from v0.16.3 to v0.16.4** changes no SwiftSandbox
  that kubeswift-spin renders. Apply its CRDs (`swiftsandboxpools` gains
  `status.warmingReplicas`) and run its controller and swiftletd at the
  same release: a pool reports a slot as warm a few seconds later than
  before, once the guest answers, and with an older swiftletd every
  checkout boots cold.
- **Upgrading KubeSwift from v0.16.2 to v0.16.3** changes no SwiftSandbox that
  kubeswift-spin renders, so it replaces no replica. Apply the v0.16.3 CRDs
  as its upgrade notes say. Its controller adds finalizers to every
  SwiftSandbox and launcher NetworkPolicy; remove them before downgrading
  KubeSwift, as its release notes describe. The first start of the runtime
  image on each node after the upgrade reads the image's layers from the
  registry once (cache repository authorization), so it takes about as long
  as a pull.
- **Upgrading KubeSwift from v0.16.0 or v0.16.1 to v0.16.2 or later**
  gives replicas without a readiness check `spec.readinessStartup` (with
  the default `auto`; every replica with `always`) and replaces them once.
  The KubeSwift chart does not move SwiftKernels to the v0.16.2 kernel
  artifact (`kernels/sandbox:6.6.16`, Linux 6.6.158);
  changing `spec.ociRef.image` pulls nothing until the kernel's pull Jobs
  are deleted, as the KubeSwift v0.16.2 upgrade notes describe. Running
  sandboxes keep the kernel they booted with until they are replaced.
