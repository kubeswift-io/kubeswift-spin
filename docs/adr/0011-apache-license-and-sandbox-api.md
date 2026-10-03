# 0011: Apache-2.0 license and a project-owned sandbox API subset

Status: accepted

## Context

kubeswift-spin is licensed under the Apache License 2.0, like Spin and
SpinKube. KubeSwift is licensed under the AGPL-3.0. Importing KubeSwift's Go
API package would link AGPL code into the controller binary, so the
distributed binary would carry AGPL obligations regardless of the license of
this repository's source.

## Decision

- Declare the part of the `sandbox.kubeswift.io/v1alpha1` API that
  kubeswift-spin uses in `internal/sandboxapi`: the SwiftSandbox fields it
  sets and reads, and the SwiftSandboxPool fields it reads. Deepcopy code is
  generated with controller-gen (`make generate`).
- Keep KubeSwift as a test-only module dependency. A contract test compares
  every declared field (JSON name, tag options, Go type), every constant,
  and the CRD's required spec fields with the pinned KubeSwift release, and
  round-trips an object through the upstream types.
- Never update a SwiftSandbox or SwiftSandboxPool, so fields not declared
  here are never dropped from objects KubeSwift owns.

## Consequences

- The controller and entrypoint binaries link no AGPL code
  (`go list -deps ./cmd/...` shows no KubeSwift package).
- A KubeSwift upgrade that renames or retypes a used field fails the
  contract test; new upstream fields need no change unless kubeswift-spin
  starts using them.
- Using a new SwiftSandbox feature (for example port exposure) means adding
  its fields to `internal/sandboxapi`, where the contract test checks them.
