# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses semantic versioning.

Releases that change the rendered SwiftSandbox spec (and therefore replace
running replicas on upgrade) say so under "Upgrade notes".

## Unreleased

Initial implementation, targeting v0.1.0. Licensed under the Apache License
2.0.

### Added

- External SpinKube executor: SpinApps whose executor is labelled
  `spin.kubeswift.io/managed-by: kubeswift-spin` are realized as KubeSwift
  SwiftSandboxes, one per replica.
- Executor profiles through `spin.kubeswift.io/` annotations: runtime image,
  pull secret, verify key, network mode, rootfs mode, kernel profile, warm
  pool, node selector, default CPU and memory, egress allowlist
  (`egress-allow`) and ingress peers (`ingress-from`).
- SpinApp compatibility analysis covering every v0.6.1 `SpinAppSpec` field;
  unsupported configuration blocks reconciliation with a condition and an
  Event.
- Deterministic replica reconciliation with rolling replacement, failure
  backoff and conflict detection; status on the standard SpinKube
  conditions.
- Startup checks for required APIs and RBAC.
- Detection of KubeSwift v0.16.0 SwiftSandbox features from the published
  OpenAPI v3 schema (exposure, secrets, egress). The controller does not
  start until one detection succeeds, logs `SwiftSandbox features detected`,
  re-reads the schema every `--capability-refresh` and keeps the last
  successful result on errors (ADR 0012).
- Exposure on KubeSwift v0.16.0: each sandbox declares port `http-app`
  (3000), labels its launcher pod with
  `core.spinkube.dev/app.<name>.status=ready` and gets readiness and
  liveness probes from `spec.checks` (a TCP readiness check by default), so
  Spin Operator's unchanged SpinApp Service routes to ready replicas.
  `readyReplicas` counts sandboxes with `WorkloadReady=True`.
  `spec.checks` and `spec.podLabels` are supported.
- Secret references on KubeSwift v0.16.0: `variables[].valueFrom.secretKeyRef`,
  `valueFrom.secretKeyRef` on runtime-config options (placeholders
  substituted by the entrypoint), `runtimeConfig.loadFromSecret` and
  `imagePullSecrets` (merged Docker config in the guest). kubeswift-spin
  still reads no Secret.
- Egress allowlist on KubeSwift v0.16.0 through `spin.kubeswift.io/egress-allow`,
  chart executor values `egressAllow` and `ingressFrom`, and the sample
  executor `config/executor/kubeswift-egress.yaml`.
- `Available` reason `ApplicationNotReady` for running sandboxes that do
  not pass the readiness check yet.
- Runtime image `spin-4.2.1-r1` with Spin v4.2.1 and a privilege-dropping
  entrypoint, versioned independently of the controller so that controller
  upgrades do not replace running replicas.
- Helm chart, Prometheus metrics, example Spin applications, and upstream
  KubeSwift requirement proposals.
- KVM e2e test (`test/e2e/kvm-e2e.sh`) covering Service access, scaling,
  rolling updates under traffic, Secret-backed variables and tokens, the
  egress allowlist and warm-pool checkout. It passed on a lab cluster with
  KubeSwift v0.16.0 on 2026-10-05.

### Changed

- The KubeSwift test dependency, contract test and envtest CRDs moved from
  v0.15.1 to v0.16.0. KubeSwift v0.15.1 remains usable in a degraded mode
  (no exposure, `NetworkUnavailable`, Secret references rejected).
- Warm-pool compatibility also compares `network.ports` and
  `network.egress`; pools must declare port `http-app` 3000 on KubeSwift
  v0.16.0.
- SpinApp names longer than 52 characters are rejected when exposure is
  used.
- Messages for literal credential options and URLs with credentials now
  suggest `valueFrom.secretKeyRef`.
- Status patch conflicts are retried quietly;
  `kubeswift_spin_reconciliations_total` has the `result` values `success`,
  `conflict` and `error`.
- The `serverless-ai` example uses the `kubeswift-egress` executor and reads
  `auth_token` from the Secret `llm-token`, key `token`.

### Upgrade notes

- Upgrading KubeSwift from v0.15.1 to v0.16.0 under a running controller
  changes the rendered sandbox spec (ports, probes, launcher pod labels), so
  every replica is replaced once, one at a time.
- Rotating a referenced Secret does not replace replicas; values are read
  when a sandbox starts.

### Known issues

- Spin Operator v0.6.1 rejects a `spec.checks` `httpGet` without
  `httpHeaders`, because its defaulting webhook writes `null`. Set
  `httpHeaders: []`.

### Known limitations

- On KubeSwift v0.15.1, HTTP SpinApps cannot be reached and Secret-backed
  configuration is rejected.
- Any SpinApp author in a namespace can expose that namespace's Secrets to
  their application.
- `deploymentConfig.caCertSecret`, `configMapKeyRef` and autoscaling are not
  supported.
- Not tested: secret files (`imagePullSecrets`, `loadFromSecret`) in a
  microVM, `ingress-from`, liveness-failure replacement, arm64, the kind
  integration test against the v0.16.0 CRDs, and the GitHub workflows.
