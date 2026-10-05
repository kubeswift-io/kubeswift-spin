# 0012: Use KubeSwift features only when the schema shows them

Status: accepted

## Context

KubeSwift v0.16.0 added the SwiftSandbox features kubeswift-spin needs:
port exposure, probes, launcher pod metadata, Secret delivery and an egress
allowlist. Clusters may still run v0.15.1. The API server silently prunes
fields that a CRD schema does not know, so a sandbox spec that sets them on
an older KubeSwift would be accepted and then behave differently from what
the controller assumed: no exposure, a dropped Secret reference. The
detected features also shape every sandbox spec, and the spec hash decides
when replicas are replaced.

## Decision

- Read the published OpenAPI v3 schema of `sandbox.kubeswift.io/v1alpha1`
  (no RBAC beyond default discovery) and derive three features: exposure
  (`spec.network.ports`, `spec.readinessProbe` and `spec.podMetadata`),
  secrets (`spec.secretFiles`; the same release honors `secretKeyRef` env)
  and egress (`spec.network.egress`). Do not infer features from a version
  number.
- Set a feature's fields only when it is detected. Without it, reject
  configuration that needs it (`UnsupportedConfiguration`,
  `ExecutorInvalid`) or, for exposure, report `NetworkUnavailable`.
- Do not start reconciling until one detection has succeeded: the
  controller exits if the schema cannot be read at startup, and logs
  "SwiftSandbox features detected" with the result.
- Re-read the schema at most every `--capability-refresh` (default 5
  minutes). A failed read keeps the last successful result, so a transient
  discovery error never changes the rendered spec.

## Consequences

- One controller build works on KubeSwift v0.15.1 (degraded) and v0.16.0.
- When KubeSwift is upgraded and the features appear, the rendered spec
  changes and every replica is replaced once, one at a time. This is
  expected and documented in the executor contract.
- A cluster that does not publish OpenAPI v3 for the sandbox API cannot run
  the controller.
- Each new KubeSwift feature needs a schema check here before it is used.
