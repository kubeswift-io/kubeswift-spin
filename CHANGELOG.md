# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses semantic versioning.

Releases that change the rendered SwiftSandbox spec (and therefore replace
running replicas on upgrade) say so under "Upgrade notes".

## Unreleased

### Fixed

- The chart's leader-election Role allows core `events` create and patch in
  the release namespace. controller-runtime records the LeaderElection
  Event through the core/v1 API, and without it every leader acquisition
  logged "events is forbidden". The kind integration test now fails on any
  RBAC denial in the controller log.
- docs/releasing.md: verifying signatures needs cosign v3.

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
