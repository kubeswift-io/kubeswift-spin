# Placement readiness while a pool has no pod

Status: proposal for KubeSwift, not filed. Observed on KubeSwift v0.20.0.

## Problem

A `SwiftArtifactPlacement` prepares artifacts on the nodes of a
`SwiftSandboxPool`. KubeSwift derives those target nodes from the pods
that carry the pool's label (`targetNodes` in the placement controller).
A pool whose warm slots are all checked out, for example a pool with
`minWarm: 1` between a checkout and its refill, has no such pod for a
moment. The placement then reports `Prepared=False/NoTargetNodes` and
`targetNodes: 0`, although every node it prepared still holds the
artifacts in its node cache, and it returns to `Prepared` once the refill
pod is scheduled.

A consumer that attaches an optional artifact only while the placement is
`Prepared` gives a replica created in that moment no artifact, although
the node the replica lands on holds it. In the kubeswift-spin benchmark
of local applications (one-slot pool, 30 warm starts), one start in 30
hit this window and started from the registry.

## Proposal

Keep the target node set stable across such moments: count the nodes of
the pool's checked-out sandboxes as well as its slot pods, or keep the
last non-empty set (and its prepared state) for as long as the nodes
exist and match the pool's node selector. `NoTargetNodes` would then mean
that the pool can place nothing, not that it is between a checkout and a
refill.

## Workaround in kubeswift-spin

None. The controller attaches the application artifact only to replicas
created while the placement is `Prepared`, so the cost is an occasional
start from the registry, which is correct. Pools with more than one warm
slot hit the window less often.
