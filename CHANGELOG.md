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
  pool, node selector, default CPU and memory.
- SpinApp compatibility analysis covering every v0.6.1 `SpinAppSpec` field;
  unsupported configuration blocks reconciliation with a condition and an
  Event.
- Deterministic replica reconciliation with rolling replacement, failure
  backoff and conflict detection; status on the standard SpinKube
  conditions.
- Startup checks for required APIs and RBAC; OpenAPI-based detection of
  sandbox port exposure.
- Runtime image `spin-4.2.1-r1` with Spin v4.2.1 and a privilege-dropping
  entrypoint, versioned independently of the controller so that controller
  upgrades do not replace running replicas.
- Helm chart, Prometheus metrics, example Spin applications, and upstream
  KubeSwift requirement proposals.

### Known limitations

- HTTP SpinApps cannot be reached: KubeSwift v0.15.1 sandboxes have no
  inbound port exposure.
- No secret delivery, private application registries or autoscaling.
- Not yet tested end to end on KVM.
