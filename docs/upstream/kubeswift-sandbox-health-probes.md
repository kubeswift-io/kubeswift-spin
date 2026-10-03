# KubeSwift requirement: workload readiness and liveness probes for SwiftSandbox

Status: proposal for KubeSwift. Not implemented in KubeSwift v0.15.1. Tracked in [kubeswift-io/kubeswift#729](https://github.com/kubeswift-io/kubeswift/issues/729).

## User problem

`SwiftSandbox` phase `Running` means the microVM booted. It says nothing
about whether the workload inside is serving: a server may still be
downloading data, binding its port, or stuck. Controllers that place
sandboxes behind a Service, gate rollouts, or report application readiness
need a workload-level signal. A workload that hangs without exiting is never
replaced, because the sandbox stays `Running`.

## Why this is generic KubeSwift functionality

Readiness and liveness of the process inside a sandbox matter to every
long-running sandbox workload, exactly as container probes matter to every
pod. Only KubeSwift can reach the guest: the guest address is private to the
launcher pod.

## Why it does not belong in kubeswift-spin

kubeswift-spin has no network path into the guest (its address is scoped to
the launcher pod) and must not exec into launcher pods or use KubeSwift's
vsock exec channel, which is an interactive debugging interface, not a
health API. Probing from the controller would also not scale and would put
an integration controller on the data path.

## Current behavior (v0.15.1)

There is no probe field. Status reports phase, the `GuestRunning` condition
and, after exit, the workload exit code. The launcher pod has no readiness
probe tied to the workload.

## Proposed API

Probe handlers follow `corev1.Probe`, restricted to what can run against
the guest:

```yaml
spec:
  readinessProbe:
    httpGet: {path: /healthz, port: http-app}   # or tcpSocket
    initialDelaySeconds: 2
    periodSeconds: 5
    timeoutSeconds: 1
    failureThreshold: 3
  livenessProbe:
    tcpSocket: {port: http-app}
    periodSeconds: 10
    failureThreshold: 3
status:
  conditions:
    - type: WorkloadReady    # new condition
      status: "True"
```

`exec` probes are out of scope for the first version.

## Controller behavior

- Probes run from the launcher pod against the guest address, so they work
  with or without port exposure.
- Readiness sets a `WorkloadReady` condition on the SwiftSandbox and, when
  ports are exposed, the launcher pod's readiness (so Endpoints follow it).
- A failed liveness probe terminates the workload and the sandbox becomes
  `Failed` with reason `LivenessProbeFailed`, consistent with
  `restartPolicy: Never`. The owner decides whether to replace it.

## Security considerations

Probes originate inside the launcher pod and target only the guest; they do
not open new network paths. Probe headers come from the sandbox spec and are
stored in plain text, so they must not be used for credentials, as with pod
probes.

## Compatibility implications

Optional fields; no change for existing sandboxes. Warm-pool checkout must
start probing after the workload is injected. kubeswift-spin would map
`SpinApp.spec.checks` onto these probes (Spin Operator maps them onto
container probes on port 80 today) and count `WorkloadReady` sandboxes in
`readyReplicas`.

## Tests required

- `WorkloadReady` is False until an HTTP server in the guest answers, then
  True; it returns to False when the server stops answering.
- A liveness failure terminates the sandbox with reason
  `LivenessProbeFailed`.
- Probes work in `restricted` and `open` modes and on warm-pool checkouts.
- Probe traffic does not reach launcher-side services.
