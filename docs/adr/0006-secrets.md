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
