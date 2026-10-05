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
| `API group version sandbox.kubeswift.io/v1alpha1 is not served` | KubeSwift sandbox CRDs missing | Install KubeSwift v0.16.0 or later (v0.15.1 runs in a degraded mode, see [networking.md](networking.md#kubeswift-v0151)). |
| `detect SwiftSandbox features from the OpenAPI v3 schema of sandbox.kubeswift.io/v1alpha1` | the API server does not publish the OpenAPI v3 schema of the sandbox API, or it could not be read | Check that `kubectl get --raw /openapi/v3/apis/sandbox.kubeswift.io/v1alpha1` works with the controller's credentials. The controller does not reconcile until it knows the SwiftSandbox features. |
| `the controller service account is missing permissions` | RBAC incomplete, for example a hand-written ClusterRole | Use the chart's RBAC; the message lists each missing verb and resource. |
| `--runtime-image: ... must include a registry and an explicit tag or digest` | invalid runtime image flag | Set `runtimeImage` in the Helm values to a fully qualified reference. |
| `timed out waiting for cache to be synced` | an informer cannot list or watch | Check RBAC and API server connectivity. |

When it starts, the controller logs which KubeSwift features it found:

```bash
kubectl -n kubeswift-spin-system logs deploy/kubeswift-spin | grep "SwiftSandbox features detected"
```

`exposure`, `secrets` and `egress` are all `true` on KubeSwift v0.16.0 and
all `false` on v0.15.1.

## The SpinApp is refused by admission

```
... httpHeaders ... must be of type array ...
```

Spin Operator v0.6.1's defaulting webhook writes `httpHeaders: null` into an
`httpGet` check that has none, and its own validation then rejects it. Set
`httpHeaders: []` in every `spec.checks.<kind>.httpGet` (see
[compatibility.md](compatibility.md#known-upstream-issues)).

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
| `UnsupportedConfiguration` | The message names each field. Remove it or see [compatibility.md](compatibility.md). A `spec.image` without a registry host (`org/app:tag` instead of `docker.io/org/app:tag`), `configMapKeyRef`, literal credentials, `loadFromSecret` combined with other runtime-config fields, `volumes`, `serviceAccountName`, `enableAutoscaling` and SpinApp names over 52 characters are not supported. On KubeSwift before v0.16.0, Secret references, `imagePullSecrets` and `podLabels` are rejected too. |
| `ExecutorInvalid` | The message lists every problem with the executor: `createDeployment: true`, `deploymentConfig` fields (including `caCertSecret`), unknown `spin.kubeswift.io/` annotations, network mode `none`, `egress-allow` with network mode `open`, `egress-allow` or `ingress-from` on KubeSwift before v0.16.0, invalid values. |
| `ExecutorNotFound` | The executor was deleted. Existing sandboxes keep running; recreate the executor. |
| `WarmPoolIncompatible` | The pool is missing or its shape differs; the message names each mismatching field. Align the pool with the runtime image, CPU, memory, network mode, `network.ports` (on KubeSwift v0.16.0, `http-app` port 3000), egress allowlist and ingress peers the SpinApp gets (see [executor-contract.md](executor-contract.md#warm-pools)). The image must be the exact reference in the controller's `--runtime-image` argument; the released chart passes it by digest, so a tag does not match. If the message says the SwiftSandboxPool API was not installed at startup, restart the controller. |
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
  artifact (`spin registry push`) on a registry reachable from the guest.
  Under the `restricted` network mode the registry must have a public
  address or an `egress-allow` entry. A private registry needs
  `spec.imagePullSecrets` naming Secrets of type
  `kubernetes.io/dockerconfigjson` in the SpinApp's namespace; the
  entrypoint fails with `is not a Docker config with an auths section` when
  a Secret has another format. Spin pulls only over HTTPS and trusts only
  the runtime image's CA bundle, so a registry with a private CA does not
  work.
- **A secret file is missing.** `loadFromSecret` and `imagePullSecrets` use
  KubeSwift secret files, which need the sandbox SwiftKernel 6.6.14 or
  later. `loadFromSecret` needs the key `runtime-config.toml` in the Secret.
- **A selected component chains to an unselected one.** With
  `spec.components`, Spin refuses to start if a selected component calls
  another component through `*.spin.internal` that is not selected.
- **A runtime-config store type is unknown or misconfigured.** Spin reports
  the table and key.
- **A variable has no value.** Spin requires every application variable
  without a default; set it in `spec.variables`.

## Available=False, ApplicationNotReady

The sandboxes run, but too few pass the readiness probe. Right after
creation this is normal for a few seconds while Spin pulls the application
and starts listening. If it persists:

```bash
kubectl -n <namespace> get swiftsandbox <app>-0 -o jsonpath='{.status.conditions[?(@.type=="WorkloadReady")]}'
```

- With `spec.checks.readiness`, the application must answer the check's
  path successfully on port 3000 in the guest. Spin routes the path like any
  request, so a path without a route returns 404 and never becomes ready.
- Without it, the TCP check fails until Spin listens. Read the guest console
  and look for `Serving http://0.0.0.0:3000`:

```bash
swiftctl -n <namespace> sandbox logs <app>-0
```

## Available=False, NetworkUnavailable

Expected only with KubeSwift before v0.16.0. The sandboxes run, but
SwiftSandbox has no inbound port exposure or readiness probes, so the
application cannot be reached and no replica is counted as ready. Upgrade
KubeSwift; see [networking.md](networking.md#kubeswift-v0151).

## The Service has no endpoints or requests fail

```bash
kubectl -n <namespace> get endpointslices -l kubernetes.io/service-name=<app>
```

Endpoints exist only for ready replicas. If replicas are ready but clients
cannot connect, check the executor's `spin.kubeswift.io/ingress-from`
annotation: only the listed peers may reach the Spin port. The Service is
created by Spin Operator; kubeswift-spin does not manage it.

## Outbound requests time out

Under the `restricted` network mode, packets to blocked destinations
(cluster addresses, RFC1918 ranges, `169.254.0.0/16`) are dropped, so the
application sees a timeout, not a refused connection. To allow one
in-cluster Service, add it to the executor's `spin.kubeswift.io/egress-allow`
annotation (KubeSwift v0.16.0, network mode `restricted` only). For outbound
HTTP from a component, the host must also be in `allowed_outbound_hosts`.
See
[networking.md](networking.md#egress-allowlist).

## A rotated Secret is not used

Secret values are delivered when a sandbox starts, and rotating a Secret
does not replace replicas. Replace them by changing the SpinApp spec, for
example a variable, or delete the sandboxes one at a time; kubeswift-spin
recreates them.

## Replicas are replaced unexpectedly

Any change to the rendered sandbox spec rolls the replicas, including
executor profile changes, a new controller default runtime image, and a
KubeSwift upgrade that adds sandbox features (for example v0.15.1 to
v0.16.0, which adds ports, probes and launcher pod labels). The list is in
[executor-contract.md](executor-contract.md#what-triggers-replacement).
The `spin.kubeswift.io/revision` label shows which revision each sandbox
runs.

## Deleting a SpinApp leaves sandboxes

Sandboxes are removed by the garbage collector through owner references.
If they persist, check that the garbage collector is running and that the
sandboxes still have an owner reference to the SpinApp. kubeswift-spin adds
no finalizers that could block deletion.
