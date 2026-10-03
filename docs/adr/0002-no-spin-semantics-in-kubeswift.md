# 0002: Keep Spin semantics out of KubeSwift

Status: accepted

## Context

Running Spin in sandboxes exposed missing KubeSwift features: port
exposure, probes, secret projection, artifact projection, egress allowlists.
It would be faster to add Spin-shaped fields to KubeSwift, or to patch
around the gaps from this controller.

## Decision

The dependency direction is SpinKube, then kubeswift-spin, then KubeSwift.
KubeSwift stays unaware of Spin. Rule: a capability that is useful without
Spin belongs in KubeSwift; a capability that exists only because of Spin
belongs in kubeswift-spin. Gaps of the first kind are written up as generic
proposals in [docs/upstream](../upstream/) and are not implemented here.
kubeswift-spin uses only the public SwiftSandbox API, never KubeSwift
internals.

## Consequences

- With KubeSwift v0.15.1, HTTP SpinApps run but cannot be reached; the
  status says so.
- Each upstream feature, once available, benefits every sandbox user, and
  kubeswift-spin adopts it by setting public fields.
