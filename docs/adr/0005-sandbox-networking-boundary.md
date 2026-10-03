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
