# 0003: Implement the controller in Go

Status: accepted

## Context

Spin and Cloud Hypervisor are written in Rust. The component built here is
a Kubernetes controller that adapts one Kubernetes API to another.

## Decision

Write the controller and the runtime entrypoint in Go, using
controller-runtime, the upstream Spin Operator Go API
(`github.com/spinkube/spin-operator` v0.6.1) and the KubeSwift Go API
(`github.com/kubeswift-io/kubeswift` v0.15.1). Tests use envtest with the
CRDs shipped in those modules.

## Consequences

- No hand-maintained copies of upstream types; a unit test fails when the
  SpinApp spec gains an unclassified field.
- Kubernetes quantity handling, owner references, informers and envtest
  come from the standard libraries.
- The example applications are Rust because that is Spin's primary SDK;
  the controller language does not constrain them.
