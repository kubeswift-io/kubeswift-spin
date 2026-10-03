# KubeSwift requirement: inbound port exposure for SwiftSandbox

Status: proposal for KubeSwift. Not implemented in KubeSwift v0.15.1.

## User problem

A SwiftSandbox can run a long-lived network server (an HTTP API, an MCP tool
server, an AI agent endpoint, a development or preview environment), but
nothing outside the sandbox can connect to it. Users who want a
hardware-isolated server today must use SwiftGuest, which brings disk
images, cloud-init and VM lifecycle management that an OCI-image workload
does not need.

## Why this is generic KubeSwift functionality

Exposing a guest port through the standard Kubernetes Service model is
useful to any sandbox workload that serves requests. It is the sandbox
counterpart of `SwiftGuest.spec.network.ports`, which KubeSwift already
implements for guests. Nothing in it is specific to Spin.

## Why it does not belong in kubeswift-spin

Exposure requires changing the launcher pod (container ports, labels,
readiness), the guest NAT (forwarding a pod port to the guest) and the
sandbox NetworkPolicy (allowing ingress to that port). All three are
KubeSwift's implementation and security boundary. An integration controller
that patched launcher pods, added iptables rules or created allow-ingress
NetworkPolicies would bypass KubeSwift's isolation posture from outside and
break whenever KubeSwift's internals change. kubeswift-spin therefore reports
`NetworkUnavailable` instead.

## Current behavior (v0.15.1)

- `spec.network` has only `mode` (`restricted`, `open`, `none`).
- Networked modes create `<sandbox>-restricted`, a NetworkPolicy with no
  ingress rules selecting the launcher pod.
- The guest has a bridge address behind the launcher pod's NAT;
  `status.network.primaryIP` has scope `Pod` and is reachable only from
  inside the launcher pod.
- The launcher pod carries only `sandbox.kubeswift.io/sandbox=<name>` (and
  `sandbox.kubeswift.io/slot` for pool slots). Labels on the SwiftSandbox are
  not propagated.
- There is no readiness signal for the workload (see
  [kubeswift-sandbox-health-probes.md](kubeswift-sandbox-health-probes.md)).

## Proposed API

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
spec:
  network:
    mode: restricted
    ports:
      - name: http-app        # IANA_SVC_NAME, unique per sandbox
        port: 3000            # guest port; also the launcher containerPort
        protocol: TCP         # TCP only in the first version
    ingress:                  # optional; default: allow from anywhere to the declared ports
      from:
        - namespaceSelector: {}
  readinessProbe:             # see kubeswift-sandbox-health-probes.md
    httpGet: {path: /healthz, port: http-app}
  podMetadata:                # labels and annotations for the launcher pod
    labels:
      core.spinkube.dev/app.hello.status: ready
```

Design notes:

- **Named ports** let an existing Service target the guest by name
  (`targetPort: http-app`) without knowing that the backend is a microVM.
- **`podMetadata.labels`** let any controller place sandboxes behind its own
  Service. KubeSwift must reject keys under its own prefixes
  (`kubeswift.io/`, `sandbox.kubeswift.io/`) so a user cannot impersonate a
  slot or another sandbox.
- **Same port inside and outside** keeps the first version simple; a
  separate `targetPort` can be added later.
- KubeSwift does not create a Service. Callers already own their Service
  shape (Spin Operator creates one per SpinApp), and a per-sandbox Service
  does not load-balance across replicas.

## Controller behavior

For a networked sandbox with `ports`:

1. Add a `containerPort` per entry to the launcher container, with the same
   name and number.
2. Forward each pod port to the same guest port (DNAT on the pod side of the
   NAT), for TCP.
3. Replace the empty ingress list of `<sandbox>-restricted` with one rule
   allowing the declared ports, from `ingress.from` peers when set. Egress
   handling is unchanged.
4. Apply `podMetadata.labels` and `podMetadata.annotations` to the launcher
   pod; on warm-pool checkout, patch them onto the claimed slot pod.
5. With a `readinessProbe`, set a pod readiness gate or probe so the pod is
   Ready only while the guest passes the probe.

`ports` with `mode: none` is invalid. Changing `ports` follows the existing
immutability rule.

## Security considerations

- Ingress opens only the declared ports, only for networked modes, and
  only when the creator asks for it. The default remains deny-all.
- The DNAT targets the guest only; launcher-side services (swiftletd, the
  Cloud Hypervisor API socket) must stay unreachable. Tests must assert
  this.
- Label propagation must not allow KubeSwift-reserved keys, or a sandbox
  could be selected by KubeSwift's own NetworkPolicies or controllers.
- `restricted` egress hardening must be unaffected; reply traffic for
  accepted inbound connections is the only new flow.

## Compatibility implications

All fields are optional additions to `v1alpha1`; existing sandboxes are
unchanged. Warm pools need `ports` (and the ingress rule) as part of the
slot shape, or must apply them at checkout. kubeswift-spin detects the
feature by the presence of `spec.network.ports`, `spec.readinessProbe` and
`spec.podMetadata` in the published OpenAPI schema and will use it only in a
release built against the KubeSwift API that defines it.

## Tests required

- A TCP server in the guest is reachable through a Service that selects
  the launcher pod by a `podMetadata` label and targets the named port.
- Undeclared guest ports and launcher-side ports stay unreachable.
- `restricted` egress rules are unchanged with ports declared.
- The launcher pod is not Ready until the readiness probe passes, and
  Endpoints follow probe state.
- Reserved label prefixes in `podMetadata` are rejected by the webhook.
- Warm-pool checkout applies labels and ports, or a slot of another shape
  is not claimed.
- `mode: none` with `ports` is rejected.
