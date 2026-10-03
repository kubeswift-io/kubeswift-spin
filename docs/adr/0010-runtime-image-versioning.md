# 0010: Version the runtime image independently of the controller

Status: accepted (from the first architecture review)

## Context

The runtime image is part of every SwiftSandbox spec, and specs are
immutable, so a new runtime image digest replaces every replica that uses
it. Versioning the runtime image with the controller would make every
controller release, including a patch release with no runtime change, roll
every SpinApp.

## Decision

- The runtime image tag lives in `runtime/VERSION` (for example
  `spin-4.2.1-r1`) and in the chart value `runtimeImage.tag`; a check keeps
  them equal.
- The release workflow builds the runtime image only when that tag does not
  exist in the registry, and otherwise reuses the existing digest, which the
  packaged chart pins.
- The tag is bumped only when Spin, the entrypoint, `internal/runtimecontract`
  or the base image changes, with an upgrade note.
- A golden test pins the revision of a reference SpinApp, so any change to
  the rendered sandbox spec, including one caused by a dependency bump that
  alters JSON encoding of upstream types, fails CI until it is acknowledged.

## Consequences

- Controller upgrades normally replace no replicas.
- Runtime updates are explicit, reviewed events.
