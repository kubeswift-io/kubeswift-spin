# Networking

## Summary

With KubeSwift v0.15.1, a SpinApp realized by kubeswift-spin runs, but its
HTTP listener cannot be reached from outside the sandbox. SwiftSandbox has
no inbound port exposure. kubeswift-spin reports this honestly instead of
working around it:

```
$ kubectl get spinapp hello-http
NAME         READY   DESIRED   EXECUTOR
hello-http   0       1         kubeswift
```

`Available` is `False` with reason `NetworkUnavailable` and a message
explaining why.

## Inbound traffic

KubeSwift creates a NetworkPolicy named `<sandbox>-restricted` for every
networked sandbox. It selects the launcher pod and has no ingress rules, so
all inbound traffic is denied. Behind the launcher pod, the guest has a
private address on a bridge and reaches the pod network through NAT. There
is no port forwarding from the pod to the guest, and no field in the
SwiftSandbox API to request one. KubeSwift's `SwiftGuest.spec.network.ports`
exposes VM ports through Services, but that feature exists only for
SwiftGuest.

A Kubernetes Service cannot help either: Spin Operator's Service selects
pods labelled `core.spinkube.dev/app.<name>.status=ready`, and KubeSwift
launcher pods carry only KubeSwift's own label. Even with matching labels,
the NetworkPolicy and the missing port forward would drop the traffic.

kubeswift-spin deliberately does not:

- create NetworkPolicies that allow ingress to launcher pods,
- modify, label or annotate KubeSwift launcher pods,
- program iptables in launcher pods or inject sidecars,
- create EndpointSlices pointing at launcher pod IPs,
- run a proxy of its own.

Each of these would bypass KubeSwift's isolation posture from outside
KubeSwift and couple this project to KubeSwift internals. The capability
belongs in KubeSwift, for every sandbox workload, not only Spin. It is
specified in
[upstream/kubeswift-sandbox-service-exposure.md](upstream/kubeswift-sandbox-service-exposure.md)
together with readiness probes
([upstream/kubeswift-sandbox-health-probes.md](upstream/kubeswift-sandbox-health-probes.md)).

## Capability detection

kubeswift-spin reads the published OpenAPI v3 schema of
`sandbox.kubeswift.io/v1alpha1` (no RBAC beyond default discovery access is
needed) and looks for the fields proposed upstream: `spec.network.ports`,
`spec.readinessProbe` and `spec.podMetadata`. The result is cached for
`--capability-refresh` (default 5 minutes).

| Detected | This release | SpinApp status |
|---|---|---|
| no | cannot expose | `NetworkUnavailable`: "the installed KubeSwift SwiftSandbox API has no inbound port exposure or readiness probes ..." |
| yes | cannot expose (built against the v0.15.1 API) | `NetworkUnavailable`: "... this kubeswift-spin release does not use them yet; upgrade kubeswift-spin" |

In neither case is a replica reported ready.

## How it would work once KubeSwift supports it

The upstream proposal is shaped so that SpinKube needs no change:

1. kubeswift-spin sets, on each SwiftSandbox, a port named `http-app`
   targeting guest port 3000, a readiness probe from `spec.checks.readiness`
   (or a TCP check), and the pod label
   `core.spinkube.dev/app.<name>.status: ready`.
2. KubeSwift propagates the label to the launcher pod, exposes the named
   container port, forwards it to the guest, allows ingress to that port
   only, and gates pod readiness on the probe.
3. Spin Operator's existing Service then selects the launcher pods and
   targets `http-app`; endpoints follow readiness.

`kubectl get spinapp`, `kubectl get service` and `kubectl get swiftsandbox`
would then tell one consistent story.

## Outbound traffic

Two independent controls apply to outbound requests from a Spin component:

1. **Spin**: `allowed_outbound_hosts` in the application manifest. Spin
   refuses requests to any other host.
2. **KubeSwift network mode**, from the executor profile:

| Mode | Egress from the guest | kubeswift-spin |
|---|---|---|
| `restricted` (default) | DNS and the public internet; RFC1918 cluster, pod and service ranges and `169.254.0.0/16` (cloud metadata) are blocked | supported |
| `open` | unrestricted, including the cluster | supported; use only for trusted applications |
| `none` | no network | rejected: Spin pulls the application artifact over the network at start |

The mode descriptions come from the KubeSwift v0.15.1 documentation and
source; kubeswift-spin has not measured them.

Practical consequences:

- The application registry must be reachable from the guest. Under
  `restricted` that means a public registry address; an in-cluster
  registry needs `open`.
- In-cluster services (databases, inference endpoints, OpenTelemetry
  collectors) need `open`, which removes all egress restriction. A narrower
  option, an egress allowlist, is proposed in
  [upstream/kubeswift-sandbox-egress-allowlist.md](upstream/kubeswift-sandbox-egress-allowlist.md).

## Service chaining

Spin local service chaining (`http://<component>.spin.internal`) happens
inside the Spin process and does not use the network, so it works in a
sandbox like anywhere else. When `spec.components` selects a subset, Spin
refuses to start if a selected component chains to an unselected one.
