# KubeSwift requirement: a re-dispatched warm workload can become ready

Status: proposal, not filed. Found on the KVM lab with KubeSwift v0.19.1
while measuring compiled caches ([ADR 0014](../adr/0014-compiled-wasmtime-cache.md)).

## User problem

When a warm checkout's optional artifact is not on the slot's node and its
`fetchTimeout` does not allow a fetch, KubeSwift skips the artifact
(`status.artifacts[].state: Skipped`, reason `NotOnNode`) and dispatches
the workload again without it, under the action id `<sandbox UID>-2`
(`actionIDFor` in `checkout.go`). swiftletd runs that workload and reports
its readiness on the pod's readiness gate with the message prefix
`workload <sandbox UID>-2: `.

The controller accepts a gate verdict only for `<sandbox UID>` or
`<sandbox UID>-fetched` (`gateMessage` in `probes.go`). The verdict of the
re-dispatched workload is ignored, so `WorkloadReady` stays `False`
(`Probing`) and the launcher pod never becomes Ready, although the
workload answers. On the lab, every such sandbox stayed not ready until it
was deleted (10 of 10 runs; the launcher log shows
`workload_ready=true` within 3 seconds).

An optional artifact is meant never to stop its workload; here it stops
its readiness.

## Proposed behavior

`gateMessage` accepts the workload id of every dispatch attempt of the
sandbox (`<sandbox UID>-<n>`), or the controller records the id of the
current dispatch and accepts exactly that one.

## kubeswift-spin until then

- A replica from a warm pool gets the compiled cache only when the pool's
  placement reports every node prepared, so the checkout finds the entry
  and dispatches once.
- A warm replica that carried the cache, had it skipped after the checkout
  and is not ready a minute later is replaced, and the SpinApp's new
  replicas start without the cache for 30 minutes (`DeliveryFailed`).
