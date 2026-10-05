# 0006: Reject secret-backed configuration instead of copying values

Status: accepted

## Context

SpinApps can reference Secrets for variables, runtime configuration and
registry credentials. KubeSwift v0.15.1 stores sandbox environment, command
and arguments in a plain ConfigMap and drops `env[].valueFrom`.

## Decision

kubeswift-spin never reads Secrets and never writes secret values anywhere.
Secret-backed fields are rejected with `UnsupportedConfiguration`;
credential-named runtime-config options must be empty; URLs with embedded
credentials are rejected. The controller has no Secret permissions.

## Consequences

- Applications that need credentials cannot run on the KubeSwift executor
  yet; the gap is specified in
  [kubeswift-sandbox-secret-projection.md](../upstream/kubeswift-sandbox-secret-projection.md).
- A compromise of the controller does not expose Secrets.

## Update (KubeSwift v0.16.0, 2026-10-05)

The decision stands: kubeswift-spin still never reads Secrets, never writes
secret values anywhere and has no Secret permissions. KubeSwift v0.16.0
now delivers Secrets to the guest itself (kubeswift-io/kubeswift#730): it
honors `env[].valueFrom.secretKeyRef` and adds `spec.secretFiles`, reading
the Secrets with a per-sandbox ServiceAccount in the launcher and writing
the values to no object, log or node disk. When that feature is detected
([ADR 0012](0012-feature-detection.md)), kubeswift-spin passes Secret
references instead of rejecting them: `secretKeyRef` variables and
runtime-config options, `runtimeConfig.loadFromSecret` and
`imagePullSecrets` are supported. Runtime-config options become
placeholders that the entrypoint fills in the guest. `configMapKeyRef`,
literal credentials and `caCertSecret` are still rejected, and on KubeSwift
v0.15.1 every Secret reference is. New consequences: any SpinApp author in a
namespace can expose that namespace's Secrets to their application, as with
a Pod's `secretKeyRef`, and rotating a Secret does not replace replicas.
See [security-model.md](../security-model.md#secrets).
