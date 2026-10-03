# 0007: Warm pools are optional capacity, checked for compatibility

Status: accepted

## Context

SwiftSandboxPool keeps pre-booted microVMs for fast checkout. KubeSwift
v0.15.1 checkout compares only image, network mode and verification key.

## Decision

An executor profile may name a pool. Each replica is still its own
SwiftSandbox with `poolRef`; the pool is never the replica set.
kubeswift-spin compares the full slot shape (image, CPU, memory, network
mode, rootfs mode, kernel profile, verify key, node selector, no GPU, no
model) and refuses a missing or incompatible pool with
`WarmPoolIncompatible`. When the pool is compatible but empty, KubeSwift's
cold fallback applies.

## Consequences

- Pools never silently change a replica's shape.
- The full-shape check duplicates logic KubeSwift should own; proposed in
  [kubeswift-sandbox-pool-shape-enforcement.md](../upstream/kubeswift-sandbox-pool-shape-enforcement.md).
