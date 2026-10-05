# 0005: Do not work around the sandbox ingress boundary

Status: accepted

## Context

KubeSwift denies all ingress to sandboxes and offers no port exposure.
kubeswift-spin could make HTTP SpinApps reachable by adding allow-ingress
NetworkPolicies, labelling launcher pods, programming NAT rules or running
a proxy.

## Decision

Do none of these. Report `Available=False` with reason
`NetworkUnavailable`, keep `readyReplicas` at 0, detect the proposed
upstream fields through the OpenAPI schema, and specify the generic feature
in [kubeswift-sandbox-service-exposure.md](../upstream/kubeswift-sandbox-service-exposure.md).

## Consequences

- No reachable HTTP SpinApps until KubeSwift ships the feature.
- No hidden coupling to KubeSwift internals and no bypass of its isolation.
- The proposal is shaped so Spin Operator's existing Service works
  unchanged once the feature exists.

## Update (KubeSwift v0.16.0, 2026-10-05)

The decision stands, and KubeSwift now provides the generic feature it
asked for. KubeSwift v0.16.0 added `spec.network.ports`,
`spec.network.ingress`, `spec.podMetadata` and sandbox readiness and
liveness probes (kubeswift-io/kubeswift#729). kubeswift-spin sets these
public fields when it detects them ([ADR 0012](0012-feature-detection.md)),
and Spin Operator's unchanged SpinApp Service routes to the launcher pods;
the KVM e2e lab run reached HTTP SpinApps that way. kubeswift-spin still
creates no NetworkPolicy, Service, EndpointSlice or proxy and never
modifies launcher pods. With KubeSwift v0.15.1 the original consequences
still apply. See [networking.md](../networking.md).
