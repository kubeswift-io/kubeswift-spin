# 0001: Realize SpinApps as an external SpinKube executor

Status: accepted

## Context

SpinKube's `SpinAppExecutor` has `spec.createDeployment`. When it is false,
Spin Operator (v0.6.1) leaves the workload to someone else: it writes no
status, creates no Deployment and only applies the SpinApp Service.
Developers already use `SpinApp`; asking them to learn a different resource
to get microVM isolation would split the ecosystem.

## Decision

kubeswift-spin is an external executor. It watches SpinApps whose executor
is labelled `spin.kubeswift.io/managed-by: kubeswift-spin` and has
`createDeployment: false`, and realizes them as SwiftSandboxes. There is no
`SwiftSpin` or similar application CRD.

## Consequences

- The user API is the standard SpinApp. Switching an app between the
  containerd shim and KubeSwift is a change of `spec.executor`.
- kubeswift-spin owns SpinApp status for its executors and must coexist
  with Spin Operator's Service and webhooks (verified in the kind
  integration test).
- SpinApp fields that only make sense for pods must be handled explicitly
  (see [compatibility.md](../compatibility.md)).
