# Architecture decision records

| ADR | Decision |
|---|---|
| [0001](0001-external-spinappexecutor.md) | Realize SpinApps as an external SpinKube executor |
| [0002](0002-no-spin-semantics-in-kubeswift.md) | Keep Spin semantics out of KubeSwift; propose generic features upstream |
| [0003](0003-go-controller.md) | Implement the controller in Go with controller-runtime |
| [0004](0004-runtime-rootfs-and-application-artifact.md) | One runtime rootfs image plus the unchanged Spin application artifact |
| [0005](0005-sandbox-networking-boundary.md) | Do not work around the sandbox ingress boundary |
| [0006](0006-secrets.md) | Reject secret-backed configuration instead of copying values |
| [0007](0007-warm-pools.md) | Warm pools are optional capacity, checked for compatibility |
| [0008](0008-executor-profiles-without-crd.md) | Executor profiles as labels and annotations, no new CRD |
| [0009](0009-replicas-status-and-deletion.md) | StatefulSet-like replicas, owned status fields, no finalizer |
