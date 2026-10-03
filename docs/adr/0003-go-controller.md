# 0003: Implement the controller in Go

Status: accepted

## Context

Spin and Cloud Hypervisor are written in Rust. The component built here is
a Kubernetes controller that adapts one Kubernetes API to another.

## Decision

Write the controller and the runtime entrypoint in Go, using
controller-runtime and the upstream Spin Operator Go API
(`github.com/spinkube/spin-operator` v0.6.1). The KubeSwift sandbox API is
declared in `internal/sandboxapi` and checked against the KubeSwift v0.15.1
Go types and CRDs by a contract test (see
[ADR 0011](0011-apache-license-and-sandbox-api.md)). Tests use envtest with
the CRDs shipped in both upstream modules.

## Consequences

- SpinKube types are used directly; a unit test fails when the SpinApp spec
  gains an unclassified field. The KubeSwift subset is hand-written but
  verified field by field against upstream.
- Kubernetes quantity handling, owner references, informers and envtest
  come from the standard libraries.
- The example applications are Rust because that is Spin's primary SDK;
  the controller language does not constrain them.
