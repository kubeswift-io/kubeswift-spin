# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses semantic versioning.

Releases that change the rendered SwiftSandbox spec (and therefore replace
running replicas on upgrade) say so under "Upgrade notes".

## Unreleased

The controller and runtime entrypoint are rebuilt with Go 1.26.9 (see
Security); their behavior and the rendered SwiftSandbox spec are unchanged.
kubeswift-spin still passes user readiness checks through as written.

### Changed

- The hello-http example and the KVM e2e test set
  `initialDelaySeconds: 1` on their readiness checks. When the field is
  omitted Spin Operator applies 10 seconds, and a hello-http replica that
  could already answer was not `Available` until about 8.6 to 8.9 seconds
  after its first direct response. On
  the lab cluster the median time to `Available` went from 18.9 to
  11.9 seconds cold and from 11.4 to 4.2 seconds
  from a warm pool; the time to the first direct response did not change.
- The KVM e2e test waits for `Available` with `kubectl wait`, which
  watches the SpinApp, and prints durations to 0.1 seconds; its other timed
  steps poll every 0.5 seconds instead of every 3 seconds.

### Added

- `startupbench` (`test/perf/startupbench`) and `make perf-startup`: a
  startup benchmark for KVM clusters that measures, from one clock and with
  watches, the time from SpinApp creation to the first direct and Service
  responses, `Available`, pod `Ready`, the EndpointSlice and the warm-pool
  slot claim, and compares results with a baseline.
- `startupbench` reports, for warm runs, `create_to_claim` and
  `claim_to_direct_http`; with `-launcher-logs` it also reports
  `claim_to_dispatch` and `dispatch_to_direct_http` from the launcher's
  `dispatch_sandbox_exec` log line, and counts per pattern the launcher
  pod's log lines (`launcherLines`), including workload actions and pod
  watch warnings, which show whether KubeSwift v0.16.1 dispatched by watch
  or fell back to polling.
- docs/performance-results/2026-10-08-kubeswift-v0.16.1: the warm-pool
  baseline on KubeSwift v0.16.1 (20 warm runs, 5 cold runs).
- docs/performance.md: the four startup measures (first-response,
  control-plane availability, warm-pool slot-claim and Spin runtime startup
  latency), current results, where the time goes, the effect of the
  readiness configuration and the release-time benchmark policy.

### Security

- Go 1.26.9 and `golang.org/x/net` v0.60.0. Go 1.26.8, which built the
  v0.1.0-rc3 controller and runtime entrypoint, has standard-library
  vulnerabilities in `net/http` (HTTP/1 and HTTP/2), `crypto/tls`,
  `mime/multipart` and `html/template` that `govulncheck` reports as
  reachable from the controller or the entrypoint (GO-2026-6599, GO-2026-6600, GO-2026-6603,
  GO-2026-6605, GO-2026-6607, GO-2026-6608, GO-2026-6610, GO-2026-6611,
  GO-2026-6612, GO-2026-6613, GO-2026-6617); five of them also affect
  `golang.org/x/net` v0.59.0. `go.mod` now names the toolchain, so CI
  builds with the same Go version as the images.

### Documentation

- KubeSwift v0.16.1 is listed as tested. It starts the workload in a
  checked-out warm slot when the API server delivers the checkout instead of
  on a 2-second check: slot claim to workload start went from 1,034 ms to
  30 ms (p50) and the first direct response from a warm pool from 2.81 to
  1.58 seconds (p50). performance.md, compatibility.md and
  why-kubeswift-spin.md give the v0.16.1 numbers.
- compatibility.md, why-kubeswift-spin.md, executor-contract.md and the
  hello-http README no longer present "19 to 26 seconds" as startup
  latency. That figure was the time to `Available` measured by the e2e
  test with 3-second polling and the 10-second readiness default.

## v0.1.0-rc3 (2026-10-06)

Release candidate with no controller, runtime or translation change since
v0.1.0-rc2: the rendered SwiftSandbox spec is unchanged, so upgrading from
rc2 replaces no replicas. The runtime image `spin-4.2.1-r1` is reused.

### Added

- OSV-Scanner (google/osv-scanner-action v2.6.0, pinned by commit): a
  differential scan on pull requests that fails on newly introduced
  vulnerabilities, a full scan daily and on every push to `main` reported in
  code scanning, and a full scan of the tagged commit that gates every
  publishing job of the release workflow. `make osv-scan` runs the same
  CLI version locally and is part of `make verify-all`. Scans name their
  lockfiles explicitly; `make verify` checks that list against the tracked
  dependency manifests and checks `osv-scanner.toml` exceptions.
- The chart policy check also requires the leader-election rules to be a
  namespaced Role and the other rule sets to be ClusterRoles.

### Fixed

- The chart's leader-election Role allows core `events` create and patch in
  the release namespace. controller-runtime records the LeaderElection
  Event through the core/v1 API, and without it every leader acquisition
  logged "events is forbidden". The kind integration test now fails on any
  RBAC denial in the controller log.
- docs/releasing.md: release signatures are Sigstore bundles stored as OCI
  referrers; verify them with cosign v3 (cosign v2.2.2 reports "no
  matching signatures").

## v0.1.0-rc2 (2026-10-05)

First complete release candidate. The v0.1.0-rc1 tag exists, but its
release run failed at the last example push and produced no GitHub
release; v0.1.0-rc2 fixes the workflow and is otherwise the same code.
It validates the initial architecture and the
core execution path on real KVM hardware and is meant for evaluation and
integration testing. It is not production-ready. Licensed under the Apache
License 2.0.

Tested with KubeSwift v0.16.0, Spin Operator v0.6.1, Spin v4.2.1 and
Kubernetes 1.34 on linux/amd64; see docs/compatibility.md. Images are also
built and published for linux/arm64, which has not been validated.

### Added

- External SpinKube executor: SpinApps whose executor is labelled
  `spin.kubeswift.io/managed-by: kubeswift-spin` and sets
  `createDeployment: false` are realized as KubeSwift SwiftSandboxes, one
  per replica. No CRD.
- Executor profiles through `spin.kubeswift.io/` annotations: runtime
  image, pull secret, verify key, network mode, rootfs mode, kernel
  profile, warm pool, node selector, default CPU and memory, egress
  allowlist (`egress-allow`) and ingress peers (`ingress-from`).
- Compatibility analysis of every Spin Operator v0.6.1 `SpinAppSpec` field;
  unsupported configuration blocks reconciliation with a condition and an
  Event naming the field.
- Replica reconciliation: create, scale, rolling replacement gated on
  readiness, deletion, conflict detection, and replacement of failed
  replicas (including liveness failures) with a backoff of 10 seconds
  doubling to 5 minutes. Status on the standard SpinKube conditions and
  `readyReplicas`.
- Detection of KubeSwift v0.16.0 sandbox features from the published
  OpenAPI schema (ADR 0012). With them: exposure through Spin Operator's
  SpinApp Service, readiness and liveness probes from `spec.checks`,
  `spec.podLabels`, Secret-backed variables and runtime-config options,
  `runtimeConfig.loadFromSecret`, private application registries through
  `imagePullSecrets`, and egress allowlists. Secrets are passed to
  KubeSwift by reference; kubeswift-spin reads no Secret.
- Warm pools through `SwiftSandboxPool`, with a full shape check before a
  pool is used (`WarmPoolIncompatible`).
- Runtime image `spin-4.2.1-r1`: Spin v4.2.1 (verified against its release
  checksums and signatures) and an entrypoint that drops root and sets
  `no_new_privs`, versioned independently of the controller.
- Helm chart with least-privilege RBAC enforced by a policy check, startup
  API and permission checks, Prometheus metrics.
- Example Spin applications (hello-http, request-info, key-value,
  outbound-http, serverless-ai, experimental MCP tools).
- Tests: unit, envtest on Kubernetes 1.34 and 1.37, a kind integration
  test with Spin Operator, Docker tests of the runtime image and examples,
  and a KVM end-to-end test with nine phases, including secret leak scans,
  a private registry, ingress restriction and liveness replacement.
- Release workflow publishing signed images, chart and example artifacts,
  with provenance attestations, SBOMs and checksums.

### Upgrade notes

- Upgrading KubeSwift from v0.15.1 to v0.16.0 under a running controller
  changes the rendered sandbox spec (ports, probes, launcher pod labels),
  so every replica is replaced once, one at a time.
- Rotating a referenced Secret does not replace replicas; values are read
  when a sandbox starts.

### Known issues

- Spin Operator v0.6.1 rejects a `spec.checks` `httpGet` without
  `httpHeaders` at admission. Set `httpHeaders: []`.

### Known limitations

- Rolling replacement has no surge: a SpinApp with one replica is
  unavailable while it is replaced. Use two or more replicas.
- `spec.image` must start with a registry host (`docker.io/org/app`, not
  `org/app`). Application artifacts are not signature-verified. The
  application registry must serve HTTPS with a certificate the runtime
  image's CA bundle trusts; there is no option for a custom CA.
- Registry credentials and Secret-backed configuration are readable by the
  Spin process inside the guest for the sandbox's lifetime.
- Any SpinApp author in a namespace can expose that namespace's Secrets to
  their application. Executor authors control sandbox images, nodes and
  network modes; there is no administrator policy for executor profiles.
- On KubeSwift v0.15.1, HTTP SpinApps cannot be reached and Secret-backed
  configuration is rejected.
- Not supported: autoscaling, `serviceAccountName`, volumes,
  `configMapKeyRef`, `deploymentConfig.caCertSecret`.
- Not tested: arm64 (images are built and published but not validated),
  more than one cluster, CNIs other than Calico, Kubernetes distributions
  other than k0s, and KubeSwift v0.15.1 on a cluster since the v0.16.0
  integration. The release workflow runs for the first time with this tag.
