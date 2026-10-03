# 0008: Executor profiles as labels and annotations, no new CRD

Status: accepted

## Context

Operators need several KubeSwift execution profiles (network mode, warm
pool, kernel, runtime image). SpinAppExecutor has no extension fields.

## Decision

Ownership is the label `spin.kubeswift.io/managed-by: kubeswift-spin`.
Profile settings are annotations under `spin.kubeswift.io/`, validated
strictly: unknown keys and invalid values make the executor invalid. Each
profile is a separate SpinAppExecutor, for example `kubeswift`,
`kubeswift-open`, `kubeswift-warm`. Controller-wide defaults are flags set by
the Helm chart.

## Consequences

- No CRD to install, version or migrate.
- Annotations are untyped; strict validation and the Helm values schema
  compensate.
- A CRD would be reconsidered only if profiles need structure that
  annotations cannot express clearly, after a new ADR.
