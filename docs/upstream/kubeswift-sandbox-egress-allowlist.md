# KubeSwift requirement: egress allowlist for restricted sandboxes

Status: implemented in KubeSwift v0.16.0 (`spec.network.egress.allow`, restricted mode only), tracked in [kubeswift-io/kubeswift#732](https://github.com/kubeswift-io/kubeswift/issues/732). kubeswift-spin uses it through the executor annotation `spin.kubeswift.io/egress-allow` (see [networking.md](../networking.md#egress-allowlist)). The text below is the original proposal; the shipped API may differ in detail.

## User problem

The `restricted` network mode blocks all cluster-internal destinations,
which is the right default for untrusted code. A sandbox that must call one
in-cluster service (an inference endpoint, an OpenTelemetry collector, a
database, an in-cluster registry) has only one alternative: `open`, which
removes every egress restriction, including the cloud metadata block.

## Why this is generic KubeSwift functionality

"Untrusted code that may talk to exactly these services" is a common shape
for agents, CI jobs and serverless functions. Egress filtering for the guest
is implemented in KubeSwift's in-pod rules (the NetworkPolicy cannot filter
the guest separately from swiftletd), so only KubeSwift can add exceptions.

## Why it does not belong in kubeswift-spin

kubeswift-spin cannot program the launcher's egress rules, and a
NetworkPolicy created by kubeswift-spin would affect the whole launcher pod,
including KubeSwift's own control traffic. Spin's `allowed_outbound_hosts`
already restricts destinations at the Wasm level, but a second,
hypervisor-side control is the point of running Spin in a microVM.

## Current behavior (v0.15.1)

- `restricted`: DNS and public internet allowed; RFC1918, cluster pod and
  service ranges and `169.254.0.0/16` blocked, enforced by FORWARD-chain
  rules on the guest's pre-NAT source.
- `open`: no egress restriction.
- No way to allow a specific destination under `restricted`.

## Proposed API

```yaml
spec:
  network:
    mode: restricted
    egress:
      allow:
        - service: {name: llm, namespace: inference, ports: [8000]}
        - cidr: 10.20.0.0/24
          ports: [5432]
```

`service` entries resolve to the Service's ClusterIP (and, optionally, its
endpoints); `cidr` entries are explicit. The metadata range stays blocked
regardless.

## Controller behavior

- Render allow rules ahead of the restricted block rules in the launcher's
  guest egress chain.
- Re-resolve Service entries when the Service changes, or document that they
  are resolved at launch.
- Reflect the effective allowlist in status for debugging.

## Security considerations

- Default behavior is unchanged.
- Referencing a Service in another namespace should require the sandbox
  creator to be allowed to do so, or be limited to the sandbox namespace in
  the first version.
- The metadata endpoint must never be allowable.

## Compatibility implications

Additive. kubeswift-spin would expose it as an executor profile setting so
that, for example, the serverless-ai example could use `restricted` plus one
inference Service instead of `open`.

## Tests required

- An allowed Service is reachable under `restricted`; other cluster
  addresses and the metadata range stay blocked.
- Rules survive warm-pool checkout.
- Invalid entries are rejected by the webhook.
