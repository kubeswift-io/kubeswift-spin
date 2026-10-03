# KubeSwift requirement: enforce the full slot shape on warm-pool checkout

Status: proposal for KubeSwift. Observed in KubeSwift v0.15.1.

## User problem

`SwiftSandboxPoolSpec` documents that a claiming SwiftSandbox "must match"
the pool's CPU, memory and rootfs mode. Checkout only enforces image,
network mode and verification key. A SwiftSandbox that asks for 2 vCPUs and
1Gi can therefore claim a 1 vCPU, 512Mi slot and run with less than it
requested, without any condition or Event saying so.

## Why this is generic KubeSwift functionality

Any user of warm pools relies on the sandbox spec being honored. The fix is
in KubeSwift's checkout path and benefits every pool user.

## Why it does not belong in kubeswift-spin

kubeswift-spin compensates by comparing the full shape itself before it sets
`poolRef` and refusing incompatible pools (`WarmPoolIncompatible`). That
protects SpinApps but not other pool users, and duplicates logic KubeSwift
owns: if KubeSwift adds a slot property, kubeswift-spin's check is
incomplete until it is updated.

## Current behavior (v0.15.1)

`slotProfile` in `internal/controller/swiftsandbox/slot_profile.go` compares
`image`, `network.mode` and `verifyKeySecretRef`. A mismatch falls back to a
cold boot with a `PoolColdFallback` Event. CPU, memory, rootfs mode, kernel
profile, node selector, model and GPU profile are not compared.

## Proposed behavior

Include every slot-shape field of `SwiftSandboxPoolSpec` in the slot
profile: `cpu`, `memory`, `rootfsMode`, `kernelProfileRef`, `nodeSelector`,
`model` and `gpuProfileRef`, in addition to the current three. Keep the
existing cold fallback on mismatch, and name the mismatching fields in the
`PoolColdFallback` Event.

No API change is needed.

## Security considerations

A sandbox could otherwise run on a node outside its node selector or under
another kernel profile than requested, which matters when node selection
expresses a trust or compliance boundary.

## Compatibility implications

Sandboxes that silently claimed mismatched slots today would boot cold
instead. That is slower but correct, and the Event explains it.
kubeswift-spin's own check can then be reduced to reporting.

## Tests required

- A sandbox whose CPU, memory, rootfs mode, kernel profile or node selector
  differs from the pool boots cold, with each mismatch named in the Event.
- A sandbox with an identical shape is checked out warm.
