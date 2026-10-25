# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses semantic versioning.

Releases that change the rendered SwiftSandbox spec (and therefore replace
running replicas on upgrade) say so under "Upgrade notes".

## Unreleased (v0.3.0)

### Changed

- The runtime image moves to Spin v4.2.5 (`spin-4.2.5-r1`), signed release
  binaries verified with `hack/verify-spin-release.sh`. Spin 4.2.4 fixed a
  crafted component id in an OCI application writing outside Spin's assets
  directory. Spin 4.2.5 uses Wasmtime 50.0.0
  (`runtimecontract.WasmtimeCacheVersion` is now `wasmtime-50.0.0`).

### Upgrade notes

- The default runtime image changes, so every replica of an executor that
  uses it is replaced once after the controller upgrade (one at a time).
- Compiled caches are rebuilt for the new runtime image (a new runtime
  digest and Wasmtime version); until a new cache is ready, replicas start
  without one, as on first use.

## v0.2.1 (2026-10-24)

A hardening release for compiled caches: KubeSwift v0.19.3 becomes their
baseline, and the workarounds for KubeSwift v0.19.1 go. With compiled
caches disabled it behaves as v0.2.0. The runtime image is unchanged
(`spin-4.2.1-r3`), so upgrading replaces no replica.

The v0.2.1-rc1 release artifacts, which v0.2.1 matches except for
documentation, version strings and test scripts, were validated on the KVM
lab with KubeSwift v0.19.3 (docs/compatibility.md): the upgrade from
v0.2.0 and the rollback to it, every phase of the KVM end-to-end test
including the private registry phase, the compiled-cache end-to-end test
against a registry behind a private CA (including cold replicas, skipped
caches and failed placements), and the startup benchmark, which showed no
regression (docs/performance.md). Compiled caches have run on
linux/amd64 only.

### Added

- Chart value `compiledCache.signer.registryCABundle`: PEM CA certificates
  the cache signer and cosign trust, on top of the system roots, for a
  cache registry behind a private CA (KubeSwift's
  `sandboxMaterialize.registryCABundle` covers the rest of the path).
  Without it the signer could not sign caches in such a registry.
- `test/e2e/private-registry.sh`: a registry behind a private CA for the
  end-to-end tests. KVM e2e phase 6 runs against it
  (`E2E_PRIVATE_REGISTRY_DIR`) instead of pushing to a public scratch
  registry, and `compiled-cache-e2e.sh` has a private-registry mode and a
  fallback phase (cold replicas, skipped caches, refused placements,
  controller restart, cache pull Secret failures).
- `private-registry.sh copy` and `runtime` print `repository@digest` and
  take a `path:tag`; the e2e scripts pass it the registry's state
  directory; in private mode the wrong-key signing Job trusts the
  registry's CA, and phase 5 requires a warm replica to be skipped with
  `NotOnNode` after the claim (fixes from running them on v0.2.1-rc1).

### Changed

- Compiled caches need KubeSwift v0.19.3 or later. v0.19.2 fixed a warm
  replica that never became ready after its optional cache was skipped at
  the checkout (kubeswift-io/kubeswift#843) and a sandbox that failed when
  its cache pull Secret could not be used (#844); v0.19.3 uses a cache the
  node already holds for a replica that boots cold (#851). KubeSwift
  publishes no version kubeswift-spin can read, and v0.19.0 to v0.19.3
  have the same schema, so the requirement is documented, not detected.
  SpinApps without compiled caches keep the same KubeSwift requirements.
- The controller no longer replaces a warm replica whose cache KubeSwift
  skipped and that is not yet ready (the v0.2.0 workaround for #843), and a
  skipped cache never starts the 30-minute `DeliveryFailed` hold. The hold
  remains for a replica KubeSwift failed because of the cache, such as an
  artifact init container that failed, or a kernel without artifact
  mounts (a `KernelUnsupported` for another kernel feature no longer starts
  one). A hold now also ends on time when a reconcile is blocked.
- The placement gate (warm replicas get the cache once their pool's nodes
  are prepared) is documented as an optimization: a replica whose cache is
  skipped starts and becomes ready without it.
- The KubeSwift test dependency is v0.19.3.
- The cache signer's cosign is built with `github.com/go-chi/chi/v5`
  v5.4.0 (GHSA-j6qf-3m9c-vwj3, in a dependency of cosign).

### Upgrade notes

- With compiled caches enabled, upgrade KubeSwift to v0.19.3 first (its
  controller and sandbox-materialize from the same release), then
  kubeswift-spin (docs/compiled-cache.md#upgrading). Do not run compiled
  caches with this release on KubeSwift v0.19.1 or v0.19.2. Before
  upgrading kubeswift-spin, delete any warm replica KubeSwift v0.19.1 left
  not ready after skipping its cache (the upgrade guide shows how to find
  them): this release no longer replaces them.
- The deletion reason `compiled_cache_not_ready` of
  `kubeswift_spin_sandbox_deletions_total` is gone; alerts on it can be
  removed.

## v0.2.0 (2026-10-23)

Adds trusted compiled Wasmtime caches (ADR 0014). With compiled caches
disabled (the default) it behaves as v0.1.1, apart from the runtime image
upgrade below.

Compiled caches need KubeSwift v0.19.1 or later and the
`kernels/sandbox:6.6.17` SwiftKernel or later in the namespaces that use
them, and have run on linux/amd64 only. The v0.2.0-rc1 release artifacts,
which v0.2.0 matches except for documentation and version strings, were
validated on the KVM lab with KubeSwift v0.19.1, Spin Operator v0.6.1 and
Spin v4.2.1 (docs/compatibility.md): the upgrade from v0.1.1, the KVM
end-to-end test (every phase except the private registry phase), the
compiled-cache end-to-end test, and the release-time startup benchmark
against v0.1.1 on the same cluster, which showed no regression
(docs/performance.md).

### Added

- Trusted compiled Wasmtime caches (ADR 0014, docs/compiled-cache.md). Off
  by default; three switches turn them on: the chart value
  `compiledCache.enabled`, the namespace label
  `spin.kubeswift.io/compiled-cache=enabled` (an administrator's assertion
  of the prerequisites), and the executor annotation
  `spin.kubeswift.io/compiled-cache: enabled` (chart value
  `executors[].compiledCache`). A SpinApp can opt out with the same
  annotation set to `disabled`.
  - One `SpinCompiledCache` record (`spin.kubeswift.io/v1alpha1`, in the
    release namespace) per namespace, application digest, runtime digest,
    architecture and repository; unused records are deleted after a
    retention period, keeping each SpinApp's current and previous record.
  - The cache manager (`manager --mode=cache-manager`, its own
    ServiceAccount) compiles each record in a SwiftSandbox running the
    runtime image's compile mode, with no signing key or push credential;
    KubeSwift exports the result with platform credentials
    (`SwiftArtifactExportPolicy`). Compilers are bounded in size, time and
    concurrency, and failures back off from 1 minute to 6 hours.
  - The cache signer (`kubeswift-spin-cache-signer` image, its own
    Deployment and ServiceAccount, the only holder of the signing key)
    checks the compiler, its export and the artifact, and signs the digest
    with cosign v3.1.3 and claims for the kind, namespace, source, runtime,
    architecture and compiler. KubeSwift verifies them under a
    `SwiftArtifactVerificationPolicy`.
  - The cache manager keeps one `SwiftArtifactPlacement` per warm pool of
    a cache's consumers. New warm-pool replicas get the signed cache as an
    optional, platform-verified artifact at `/opt/kubeswift-spin-cache`
    once their pool is prepared; replicas that boot cold get it only when
    `compiledCache.fetchTimeout` is above `0s` (the default is `0s`). The
    cache is not part of the revision: it never replaces running replicas.
  - The runtime entrypoint uses the cache only from a read-only mount whose
    metadata names the replica's application, runtime and architecture,
    and drops `WASMTIME_CACHE_FILE` and `DISABLE_WASMTIME_CACHE` set
    anywhere else.
  - The SpinApp condition `CompiledCache` and the Events
    `CompiledCacheRequested`, `CompiledCacheReady`, `CompiledCacheFailed`,
    `CompiledCacheSkipped` and `CompiledCachePlacementFailed`. The cache
    never affects `Available` or `Progressing`.
  - Metrics `kubeswift_spin_compiled_cache_lookups_total`,
    `_attachments_total`, `_compilations_total` and `_signatures_total`,
    and the deletion reason `compiled_cache_not_ready`
    (docs/observability.md).
  - Workarounds for KubeSwift v0.19.1
    ([kubeswift-io/kubeswift#843](https://github.com/kubeswift-io/kubeswift/issues/843),
    [#844](https://github.com/kubeswift-io/kubeswift/issues/844)): a warm
    replica whose cache KubeSwift skipped after the checkout and whose
    readiness it never accepts is replaced, one per reconcile. After such a
    replacement, or after a replica fails on an unreadable cache pull
    Secret, the SpinApp's new replicas start without the cache for up to
    30 minutes (`DeliveryFailed`).
- The `multi-component` example.
- `test/e2e/compiled-cache-e2e.sh`, and `PERF_COMPILED_CACHE` in
  `hack/perf-startup.sh`.

### Changed

- The KubeSwift test dependency is v0.19.1, and Go is 1.26.10.
- The chart's Leases are restricted to each component's own Lease name.
- Releases also publish the cache signer image, with an SPDX SBOM, and
  list it in `images.txt`.

### Upgrade notes

- The runtime image is `spin-4.2.1-r3` (compile mode and the compiled
  cache mount). Upgrading replaces every replica that uses the chart's
  default runtime image.
- Upgrade with `--reset-then-reuse-values`, not `--reuse-values`, as the
  README describes: the new chart values must take their defaults. With
  `--reuse-values` the chart fails to render (`nil pointer evaluating
  interface {}.enabled`), because v0.1.1 has no `compiledCache` values.

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
