# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses semantic versioning.

Releases that change the rendered SwiftSandbox spec (and therefore replace
running replicas on upgrade) say so under "Upgrade notes".

## v0.1.1 (2026-10-12)

A patch release: the drain period of deleted replicas is configurable.
KubeSwift v0.16.4 or later is the recommended baseline; v0.16.3 remains the
minimum. Without the new settings nothing changes.

### Added

- The drain period of deleted replicas is configurable: how long a replica
  keeps running after its readiness was withdrawn, so requests in flight
  can finish, before KubeSwift stops it (`spec.termination.drainPeriod`).
  The annotation `spin.kubeswift.io/drain-period` on a SpinApp takes
  precedence over the same annotation on its executor (chart value
  `executors[].drainPeriod`); without either the field stays unset and
  KubeSwift's default applies (5 seconds for exposed sandboxes). Values are
  durations with units from `0s` to `10m`. Needs KubeSwift v0.16.3 or
  later. See docs/executor-contract.md (Drain period).
- Executor drain policy: `spin.kubeswift.io/max-drain-period` (chart value
  `executors[].maxDrainPeriod`, default KubeSwift's 10 minutes) caps the
  drain period of the executor's SpinApps. A SpinApp asking for more, or
  an invalid value, is reported as `UnsupportedConfiguration` with a
  Warning Event before any sandbox is created; an invalid executor value
  as `ExecutorInvalid`. A value is never clamped.

### Fixed

- A change to a `spin.kubeswift.io/` annotation of a SpinApp, such as
  `kubectl annotate spinapp <name> spin.kubeswift.io/drain-period=30s`, is
  reconciled at once; such edits do not change the generation, and were
  previously seen only with the next spec change. An unknown
  `spin.kubeswift.io/` annotation on a SpinApp is reported in a Warning
  Event.
- A SpinApp re-created while its predecessor's sandbox still drains is
  rechecked when that drain should end (every 5 to 30 seconds) instead of
  every 5 seconds, and waits 2 minutes plus that drain period before
  reporting `SandboxConflict`.

### Changed

- KubeSwift v0.16.4 is the contract-test and envtest dependency. v0.16.4
  hands a warm-pool slot to a checkout only once its guest is ready.
- While a deleted replica still terminates, `Progressing` names the sandbox
  its ordinal waits for; the `SandboxConflict` message names the deletion
  time instead of the age.

### Upgrade notes

- Upgrading from v0.1.0 changes no rendered SwiftSandbox spec, so it
  replaces no replica; the runtime image stays `spin-4.2.1-r2`.
- Upgrade the chart with `--reset-then-reuse-values` (Helm 3.14 or later)
  or with your saved values (`--values`), not with `--reuse-values`, which
  keeps the v0.1.0 chart's pinned controller digest and so the v0.1.0
  controller. See the README (Upgrade).
- Setting, changing or removing a drain period replaces the affected
  replicas once. A longer drain keeps each deleted replica, and its
  capacity, for that long, makes a rolling update of N replicas take at
  least N times the drain period of the replicas it replaces, and sets
  the launcher pod's termination grace period (drain plus 15 seconds),
  which node drains wait for.
- SpinApp authors can set a drain period of up to 10 minutes unless the
  executor sets `spin.kubeswift.io/max-drain-period`.

## v0.1.0 (2026-10-11)

First release. kubeswift-spin is an external SpinKube executor that runs
each SpinApp replica in its own KubeSwift microVM sandbox. Licensed under
the Apache License 2.0.

Supported baseline: KubeSwift v0.16.3 or later, Spin Operator v0.6.1, Spin
v4.2.1 (runtime image `spin-4.2.1-r2`), Kubernetes 1.31 or later (1.34
tested), linux/amd64. Images are also published for linux/arm64, which has
not been validated. See docs/compatibility.md for what was tested.

### Added

- External SpinKube executor: SpinApps whose executor is labelled
  `spin.kubeswift.io/managed-by: kubeswift-spin` and sets
  `createDeployment: false` are realized as KubeSwift SwiftSandboxes, one
  per replica. No CRD.
- Executor profiles through `spin.kubeswift.io/` annotations: runtime
  image, pull secret, verify key, network mode, rootfs mode, kernel
  profile, warm pool, node selector, default CPU and memory, egress
  allowlist (`egress-allow`), ingress peers (`ingress-from`) and readiness
  startup (`readiness-startup`).
- Compatibility analysis of every Spin Operator v0.6.1 `SpinAppSpec` field;
  unsupported configuration blocks reconciliation with a condition and an
  Event naming the field.
- Replica reconciliation: create, scale, deletion, conflict detection, and
  replacement of failed replicas (including liveness failures) with a
  backoff of 10 seconds doubling to 5 minutes. Status on the standard
  SpinKube conditions and `readyReplicas`, which counts only verified
  readiness.
- Rolling updates with one surge replica (ADR 0013): a replica at the new
  revision is created before an available one is removed, so
  `spec.replicas` replicas stay available, including for a single replica.
  Scale-downs keep available replicas until the remaining ones are ready.
- Exposure through Spin Operator's SpinApp Service, readiness and liveness
  probes from `spec.checks`, and readiness startup: a replica without its
  own readiness check is ready about 0.1 seconds after Spin first answers.
- Secret-backed variables and runtime-config options,
  `runtimeConfig.loadFromSecret` and private application registries through
  `imagePullSecrets`. Secrets are passed to KubeSwift by reference;
  kubeswift-spin reads no Secret.
- KubeSwift feature detection from the published OpenAPI schema (ADR 0012),
  reported in the startup log line `SwiftSandbox features detected`.
- Warm pools through `SwiftSandboxPool`, with a full shape check before a
  pool is used (`WarmPoolIncompatible`).
- `SandboxRejected` when the API server refuses to create a SwiftSandbox
  (for example a ResourceQuota), with a growing retry delay.
- Runtime image `spin-4.2.1-r2`: Spin v4.2.1 (verified against its release
  checksums and signatures) and an entrypoint that drops root and sets
  `no_new_privs`, versioned independently of the controller.
- Helm chart with least-privilege RBAC enforced by a policy check, startup
  API and permission checks, and Prometheus metrics.
- Example Spin applications: hello-http, request-info, key-value,
  outbound-http, serverless-ai and experimental MCP tools.
- `startupbench` (`test/perf/startupbench`) and `make perf-startup`, a
  startup benchmark for KVM clusters.
- Tests: unit, envtest on Kubernetes 1.34 and 1.37, a kind integration
  test with Spin Operator, Docker tests of the runtime image and examples,
  and a KVM end-to-end test with nine phases.
- Release workflow publishing signed images, chart and example artifacts,
  with provenance attestations, SBOMs and checksums, gated on a full
  OSV-Scanner scan of the tagged commit.

### Upgrade notes

- Upgrading KubeSwift to a version that adds a feature kubeswift-spin uses
  (exposure in v0.16.0, readiness startup in v0.16.2) changes the rendered
  sandbox spec, so the affected replicas are replaced once, one at a time.
  Upgrading KubeSwift from v0.16.2 to v0.16.3 changes no rendered spec;
  apply its CRDs first, as its release notes describe.
- A rollout runs one extra sandbox per SpinApp, the surge replica
  `<spinapp>-<replicas>`. Leave room for it in node capacity and in any
  ResourceQuota on SwiftSandboxes or launcher pods.
- Rotating a referenced Secret does not replace replicas; values are read
  when a sandbox starts.

### Known issues

- Spin Operator v0.6.1 rejects a `spec.checks` `httpGet` without
  `httpHeaders` at admission. Set `httpHeaders: []`.
- When a check omits `initialDelaySeconds`, Spin Operator applies 10
  seconds; the examples set 1.

### Known limitations

- Validated on linux/amd64 and one KVM lab cluster only.
- A deleted replica drains for KubeSwift's default of 5 seconds and is
  then stopped; a request still running after that is cut. A launcher pod
  deleted with `--force` bypasses the drain and the isolation kept through
  it (not tested).
- On KubeSwift v0.16.0 to v0.16.2 a deleted replica is not drained, its
  NetworkPolicy is removed while it can still serve, and a SpinApp
  re-created under the same name can be attached to the old launcher; the
  controller logs that KubeSwift is older than v0.16.3. On KubeSwift
  v0.15.1, HTTP SpinApps cannot be reached and Secret-backed configuration
  is rejected.
- The first checkout from a newly created warm pool is much slower than
  one from an established pool.
- `spec.image` must start with a registry host (`docker.io/org/app`, not
  `org/app`). Application artifacts are not signature-verified. The
  application registry must serve HTTPS with a certificate the runtime
  image's CA bundle trusts; there is no option for a custom CA.
- Registry credentials and Secret-backed configuration are readable by the
  Spin process inside the guest for the sandbox's lifetime.
- Any SpinApp author in a namespace can expose that namespace's Secrets to
  their application. Executor authors control sandbox images, nodes and
  network modes; there is no administrator policy for executor profiles.
- Not supported, among others (see docs/compatibility.md): autoscaling,
  `serviceAccountName`, volumes, `configMapKeyRef`,
  `deploymentConfig.caCertSecret`.
