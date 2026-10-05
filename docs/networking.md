# Networking

## Summary

On KubeSwift v0.16.0, an HTTP SpinApp realized by kubeswift-spin is reached
through the Service that Spin Operator creates for every SpinApp, exactly as
with Spin Operator's own executors. kubeswift-spin gets there by setting
public SwiftSandbox fields; KubeSwift implements the port forwarding,
NetworkPolicy and probing.

```bash
kubectl -n demo get spinapp,service,endpointslices
```

```bash
kubectl -n demo run curl --rm -i --restart=Never --image=curlimages/curl:8.16.0 --command -- curl -sS http://hello-http.demo.svc/hello
```

On KubeSwift v0.15.1, which has none of these fields, SpinApps run but
cannot be reached; see [KubeSwift v0.15.1](#kubeswift-v0151).

## Inbound traffic through the SpinApp Service

Spin Operator applies a ClusterIP Service named after the SpinApp even when
the executor has `createDeployment: false`: port 80 to target port
`http-app`, selecting pods labelled
`core.spinkube.dev/app.<name>.status=ready`. kubeswift-spin does not change
it. It sets, on every SwiftSandbox:

| SwiftSandbox field | Value |
|---|---|
| `spec.network.ports` | `[{name: http-app, port: 3000, protocol: TCP}]`, the Spin HTTP listener |
| `spec.podMetadata.labels` | `core.spinkube.dev/app.<name>.status: ready`, `core.spinkube.dev/app-name: <name>`, plus `spec.podLabels` |
| `spec.readinessProbe` | from `spec.checks.readiness`, else a TCP check on `http-app` |
| `spec.livenessProbe` | from `spec.checks.liveness`, if set |
| `spec.network.ingress.from` | executor annotation `spin.kubeswift.io/ingress-from`, if set |

KubeSwift v0.16.0 then puts the labels on the launcher pod, exposes the
named port on it and forwards it to the guest, allows ingress to that port,
runs the probes from the launcher against the guest, and reports the result
as the `WorkloadReady` condition. The Service selects the launcher pods, and
its endpoints follow readiness, so a replica receives traffic only while its
readiness probe passes.

```
client --> Service <app>:80 --> launcher pod <app>-<i>, port http-app --> guest :3000 (Spin)
```

The probe details are in
[executor-contract.md](executor-contract.md#probes). Spin Operator v0.6.1
rejects an `httpGet` check without `httpHeaders`; set `httpHeaders: []`.

A SpinApp name of more than 52 characters is rejected when exposure is used:
the label key `core.spinkube.dev/app.<name>.status` would exceed the
63-character limit of a label name.

### Who may connect

Without `spec.network.ingress`, KubeSwift allows any source to reach the
declared port, and only that port. The executor annotation
`spin.kubeswift.io/ingress-from` narrows it to a JSON list of 1 to 16
NetworkPolicy peers, which kubeswift-spin copies to
`spec.network.ingress.from`:

```yaml
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinAppExecutor
metadata:
  name: kubeswift-frontend-only
  labels:
    spin.kubeswift.io/managed-by: kubeswift-spin
  annotations:
    spin.kubeswift.io/ingress-from: '[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"frontend"}}}]'
spec:
  createDeployment: false
```

The annotation is unit-tested but has not been exercised on a cluster.

### Readiness and status

A replica counts in `readyReplicas` only when its sandbox is `Running`, has
a readiness probe and KubeSwift reports `WorkloadReady=True`. While running
sandboxes do not pass the probe yet, `Available` is `False` with reason
`ApplicationNotReady`. See [executor-contract.md](executor-contract.md#status).

## Feature detection

kubeswift-spin reads the published OpenAPI v3 schema of
`sandbox.kubeswift.io/v1alpha1`. That needs no RBAC beyond the default
discovery access, and no CRD read permission.

| Feature | Schema fields | Used for |
|---|---|---|
| exposure | `spec.network.ports`, `spec.readinessProbe` and `spec.podMetadata` | ports, probes, launcher pod labels, `ingress-from` |
| secrets | `spec.secretFiles` | Secret references (see [security-model.md](security-model.md#secrets)) |
| egress | `spec.network.egress` | `egress-allow` |

The controller does not start reconciling until one detection has succeeded;
if the schema cannot be read it exits with
`detect SwiftSandbox features from the OpenAPI v3 schema of sandbox.kubeswift.io/v1alpha1`.
On success it logs the result:

```bash
kubectl -n kubeswift-spin-system logs deploy/kubeswift-spin | grep "SwiftSandbox features detected"
```

The line carries `exposure`, `secrets` and `egress` as `true` or `false`.
The schema is re-read at most every `--capability-refresh` (default 5
minutes). A failed read keeps the last successful result, because the
features shape every sandbox spec and a transient error must not replace
replicas. When an upgrade of KubeSwift adds the features, the rendered spec
changes and every replica is replaced once, one at a time.

## KubeSwift v0.15.1

KubeSwift v0.15.1 has no sandbox port exposure, probes or launcher pod
metadata. Every networked sandbox gets a NetworkPolicy without ingress
rules, and the guest sits behind the launcher pod's NAT with no port
forward. kubeswift-spin then sets none of the fields above: SpinApps run,
`readyReplicas` stays 0, and `Available` is `False` with reason
`NetworkUnavailable`:

```
the installed KubeSwift SwiftSandbox API has no inbound port exposure or readiness probes, so the Spin HTTP listener cannot be reached or verified; upgrade KubeSwift to v0.16.0 or later
```

`spec.checks` are accepted with a Warning Event but not enforced;
`spec.podLabels` and the `ingress-from` and `egress-allow` annotations are
rejected. This mode is covered by unit and envtest tests, not by a run on a
v0.15.1 cluster.

## Outbound traffic

Two independent controls apply to outbound requests from a Spin component:

1. **Spin**: `allowed_outbound_hosts` in the application manifest. Spin
   refuses requests to any other host.
2. **KubeSwift network mode and egress allowlist**, from the executor
   profile:

| Mode | Egress from the guest | kubeswift-spin |
|---|---|---|
| `restricted` (default) | DNS and the public internet, plus the destinations in `spin.kubeswift.io/egress-allow`. Cluster pod and service ranges, RFC1918 addresses and `169.254.0.0/16` (cloud metadata) are blocked | supported |
| `open` | unrestricted, including the cluster | supported; use only for trusted applications |
| `none` | no network | rejected: Spin pulls the application artifact over the network at start |

### Egress allowlist

`spin.kubeswift.io/egress-allow` (KubeSwift v0.16.0) is a JSON list of 1 to
32 KubeSwift egress rules, valid only with network mode `restricted`. Each
rule names a Service (`{"name", "namespace"}`; KubeSwift allows its
ClusterIP) or an IPv4 CIDR, optionally narrowed to ports with protocol `TCP`
or `UDP`. A Service without a namespace is in the SpinApp's namespace.
`169.254.0.0/16` stays blocked. `config/executor/kubeswift-egress.yaml`
allows one inference Service:

```yaml
metadata:
  annotations:
    spin.kubeswift.io/egress-allow: '[{"service":{"name":"llm","namespace":"inference"},"ports":[{"port":8000}]}]'
```

A component that makes outbound HTTP requests still needs the host in
`allowed_outbound_hosts`. Requests Spin itself makes for `llm_compute`
type `remote_http` are not subject to it: the serverless-ai example has an
empty `allowed_outbound_hosts`.

### What was measured

On the KubeSwift v0.16.0 lab cluster (see
[compatibility.md](compatibility.md#kvm-e2e-lab-run-2026-10-05)):

- Spin pulled the example applications from `ghcr.io` under `restricted`.
- A restricted sandbox with an allowlist entry for an in-cluster Service
  reached it.
- A restricted sandbox without the entry could not reach the same Service:
  packets are dropped, so the request timed out rather than being refused.

`open` mode and CIDR rules were not measured. The remaining mode
descriptions come from the KubeSwift documentation and source.

Practical consequences:

- The application registry must be reachable from the guest. Under
  `restricted`, a registry with a public address works; an in-cluster
  registry needs an allowlist entry or `open`.
- In-cluster services (databases, inference endpoints, OpenTelemetry
  collectors) need an allowlist entry; `open` removes every egress
  restriction, including the cloud metadata block.
- A blocked destination shows up as a timeout in the application, not as a
  connection error.

## What kubeswift-spin does not do

kubeswift-spin never patches, labels, annotates or execs into KubeSwift
launcher pods, and never creates NetworkPolicies, Services, EndpointSlices,
proxies or sidecars. Labels reach the launcher pod only through
`spec.podMetadata`, which KubeSwift applies, and the Service is Spin
Operator's. Doing any of this from outside KubeSwift would bypass its
isolation posture and couple this project to KubeSwift internals
([ADR 0005](adr/0005-sandbox-networking-boundary.md)).

## Service chaining

Spin local service chaining (`http://<component>.spin.internal`) happens
inside the Spin process and does not use the network, so it works in a
sandbox like anywhere else. When `spec.components` selects a subset, Spin
refuses to start if a selected component chains to an unselected one.
