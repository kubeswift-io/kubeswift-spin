# KubeSwift requirement: a bounded drain when an exposed sandbox is deleted

Status: proposal; not implemented in KubeSwift v0.16.2.

## Problem

When a SwiftSandbox behind a Service is deleted, two things start at once:
the endpoint controller marks the launcher pod's endpoint as terminating
(not ready, still serving), and the kubelet sends SIGTERM to the launcher.
Each node's kube-proxy stops sending new connections to the endpoint only
at its next rule sync, which with the default `minSyncPeriod` of 1 s can be
up to about a second later, and longer on a busy node.

A workload behind a Service has to keep serving during that window, or
connections routed in it fail. For pods this is usually done with a
`preStop` delay. A launcher pod has no hook, and the sandbox spec cannot
set one.

What happens on KubeSwift v0.16.2, observed on the lab cluster
(Kubernetes v1.34.3, `kernels/sandbox:6.6.16`):

- swiftletd handles SIGTERM by pressing the guest's ACPI power button.
- The sandbox guest does not shut down. The workload keeps serving.
- The kubelet kills the launcher at the end of the 30 s grace period, and
  the pod ends in phase `Failed`.
- The endpoint stays terminating and serving for those 30 s
  (`ready=false serving=true terminating=true`), so no request failed in
  946,592 requests across six rolling updates of two replicas (three with
  cold sandboxes, three from a warm pool), measured with kubeswift-spin
  v0.1.0-rc4 on 2026-10-10.

The drain therefore works by accident: it depends on the guest ignoring
the power button. Every sandbox deletion takes the full grace period, and
every deleted launcher ends `Failed`. If the guest started to honor the
power button, the window between SIGTERM and the kube-proxy sync would
fail connections.

## Proposal (generic, not Spin-specific)

For a sandbox that exposes ports (`spec.network.ports`):

1. On SIGTERM, keep the guest running for a bounded drain period, so the
   workload serves connections that are still routed to it.
2. Then stop the workload and the guest promptly, so the launcher exits
   on its own well before the grace period and does not end `Failed`.
3. Make the drain period configurable with a safe default (for example a
   few seconds), bounded by the launcher's termination grace period.

A sandbox without exposed ports needs no drain and can stop at once.

The sandbox's NetworkPolicy must cover the whole drain. Today it is a
controlled child of the SwiftSandbox, so deleting the sandbox removes it at
once, while the launcher pod keeps serving for its grace period. In that
window no sandbox policy selects the pod, and the port is reachable from
any pod in the cluster (the `ingress` peers are not enforced). The policy
should live as long as the launcher pod, for example by making the pod its
owner, or by holding it with a finalizer until the pod is gone.

Alternatives:

- **A `preStop` sleep on the launcher container** (Kubernetes sleep
  action). Generic and simple, but the delay then also applies to warm
  slots that serve nothing.
- **Leaving it to callers.** A caller cannot remove a sandbox from its
  Service before deleting it without patching the launcher pod, which a
  caller must not do.

## How kubeswift-spin would use it

Rolling replacement ([ADR 0013](../adr/0013-surge-replacement.md)) deletes
an old replica only after its replacement is ready. With an explicit
drain, the old replica keeps serving until the Service no longer routes to
it, without relying on the guest ignoring the power button, and replicas
are cleaned up in seconds instead of 30.
