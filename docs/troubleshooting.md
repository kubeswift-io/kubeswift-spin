# Troubleshooting

Start with the SpinApp conditions and Events:

```bash
kubectl -n <namespace> describe spinapp <name>
```

```bash
kubectl -n <namespace> get swiftsandbox -l spin.kubeswift.io/app=<name>
```

## The controller does not start

The controller exits with an explicit message instead of running degraded.

| Log message contains | Cause | Fix |
|---|---|---|
| `API group version core.spinkube.dev/v1alpha1 is not served` | Spin Operator CRDs missing | Install the Spin Operator v0.6.1 CRDs. |
| `API group version sandbox.kubeswift.io/v1alpha1 is not served` | KubeSwift sandbox CRDs missing | Install KubeSwift v0.15.1 or later. |
| `the controller service account is missing permissions` | RBAC incomplete, for example a hand-written ClusterRole | Use the chart's RBAC; the message lists each missing verb and resource. |
| `--runtime-image: ... must include a registry and an explicit tag or digest` | invalid runtime image flag | Set `runtimeImage` in the Helm values to a fully qualified reference. |
| `timed out waiting for cache to be synced` | an informer cannot list or watch | Check RBAC and API server connectivity. |

## Nothing happens for a SpinApp

kubeswift-spin ignores SpinApps whose executor it does not manage, and
writes no status for them. Check the executor:

```bash
kubectl -n <namespace> get spinappexecutor <executor> -o yaml
```

It needs the label `spin.kubeswift.io/managed-by: kubeswift-spin` and
`spec.createDeployment: false`, in the same namespace as the SpinApp. A
labelled executor with `createDeployment: true` produces only an
`ExecutorInvalid` Event: Spin Operator realizes such executors and owns
their SpinApp status.

## Progressing=False

| Reason | What to do |
|---|---|
| `UnsupportedConfiguration` | The message names each field. Remove it or see [compatibility.md](compatibility.md). Secret-backed values, `imagePullSecrets`, `volumes`, `podLabels`, `serviceAccountName` and `enableAutoscaling` are not supported. |
| `ExecutorInvalid` | The message lists every problem with the executor: `createDeployment: true`, `deploymentConfig` fields, unknown `spin.kubeswift.io/` annotations, network mode `none`, invalid values. |
| `ExecutorNotFound` | The executor was deleted. Existing sandboxes keep running; recreate the executor. |
| `WarmPoolIncompatible` | The pool is missing or its shape differs; the message names each mismatching field. Align the pool with the runtime image, CPU, memory and network mode the SpinApp gets. If the message says the SwiftSandboxPool API was not installed at startup, restart the controller. |
| `SandboxConflict` | A SwiftSandbox with the needed name exists and is not owned by the SpinApp. Delete or rename it. |
| `RuntimeImageUnavailable` | KubeSwift could not pull, verify or materialize the runtime image. Check the image reference, pull secret and cosign key. |
| `SandboxFailed` | Spin exited or the guest failed. Read the guest console (below). The replica is replaced after a backoff of up to 5 minutes. |

## Sandboxes stay Pending

The `Progressing` message carries KubeSwift's reason:

- `KernelNotFound` or `KernelNotReady`: create a SwiftKernel named
  `sandbox` (or the executor's kernel profile) in the SpinApp's namespace
  and wait until it is `Ready`.
- No reason, pod unschedulable: no node is labelled
  `kubeswift.io/kernel-node=true`, or the executor's node selector matches
  none. Check the launcher pod, which has the sandbox's name:

```bash
kubectl -n <namespace> describe pod <app>-0
```

## Spin exits right after start

```bash
swiftctl -n <namespace> sandbox logs <app>-0
```

Common causes:

- **The application cannot be pulled.** The artifact must be a Spin OCI
  artifact (`spin registry push`), on a registry reachable from the guest
  without credentials. Under the `restricted` network mode the registry
  must have a public address; in-cluster registries need an `open`
  executor.
- **A selected component chains to an unselected one.** With
  `spec.components`, Spin refuses to start if a selected component calls
  another component through `*.spin.internal` that is not selected.
- **A runtime-config store type is unknown or misconfigured.** Spin reports
  the table and key.
- **A variable has no value.** Spin requires every application variable
  without a default; set it in `spec.variables`.

## Available=False, NetworkUnavailable

Expected with KubeSwift v0.15.1. The sandboxes run, but SwiftSandbox has no
inbound port exposure or readiness probes, so the application cannot be
reached and no replica is counted as ready. See
[networking.md](networking.md). To confirm Spin is serving inside the
guest, look for `Serving http://0.0.0.0:3000` in the guest console.

## Replicas are replaced unexpectedly

Any change to the rendered sandbox spec rolls the replicas, including
executor profile changes and a new controller default runtime image. The
list is in [executor-contract.md](executor-contract.md#what-triggers-replacement).
The `spin.kubeswift.io/revision` label shows which revision each sandbox
runs.

## Deleting a SpinApp leaves sandboxes

Sandboxes are removed by the garbage collector through owner references.
If they persist, check that the garbage collector is running and that the
sandboxes still have an owner reference to the SpinApp. kubeswift-spin adds
no finalizers that could block deletion.
