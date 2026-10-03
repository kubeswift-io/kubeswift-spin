# Executor contract

This document specifies what kubeswift-spin does with a SpinApp and what it
expects from SpinKube and KubeSwift. It describes the implemented behavior
of this repository; where a behavior is limited by an upstream API, the
limitation is stated.

## Ownership

kubeswift-spin realizes a SpinApp when the SpinAppExecutor named in
`spec.executor`, in the SpinApp's namespace:

1. carries the label `spin.kubeswift.io/managed-by: kubeswift-spin`, and
2. has `spec.createDeployment: false`.

The executor name is never used to infer ownership. An executor with the
label and `createDeployment: true` is reported as invalid instead of
realized, because Spin Operator would also create a Deployment.

SpinApps whose executor lacks the label are ignored entirely: kubeswift-spin
creates nothing and writes no status for them.

## What Spin Operator still does

Verified against the Spin Operator v0.6.1 source and in the kind
integration test. For a SpinApp whose executor has `createDeployment: false`,
Spin Operator:

- writes no SpinApp status at all,
- deletes a Deployment named after the SpinApp if one exists (left over from
  a previous executor),
- server-side applies a ClusterIP Service named after the SpinApp, port 80
  to target port `http-app`, selecting pods labelled
  `core.spinkube.dev/app.<name>.status=ready`, with
  `spec.serviceAnnotations`,
- creates no runtime-config Secret, CA Secret, HPA or ConfigMap,
- adds the finalizer `core.spinkube.dev/finalizer` to executors and blocks
  executor deletion while SpinApps still reference it.

kubeswift-spin therefore owns the SpinApp status for its executors, and the
Service exists but has no endpoints until KubeSwift can expose sandboxes
(see [networking.md](networking.md)).

## Executor profiles

SpinAppExecutor has no extension fields, so a KubeSwift execution profile is
expressed as annotations. Several executors can carry different profiles,
for example `kubeswift`, `kubeswift-open` and `kubeswift-warm`.

| Annotation | Values | Default |
|---|---|---|
| `spin.kubeswift.io/runtime-image` | image reference with registry and tag or digest | controller `--runtime-image` |
| `spin.kubeswift.io/runtime-image-pull-secret` | Secret name, used by KubeSwift to pull the runtime image | none |
| `spin.kubeswift.io/runtime-image-verify-key-secret` | Secret name holding `cosign.pub`, used by KubeSwift to verify the runtime image | none |
| `spin.kubeswift.io/network-mode` | `restricted`, `open` | `restricted` |
| `spin.kubeswift.io/rootfs-mode` | `block`, `virtiofs` | KubeSwift default (`block`) |
| `spin.kubeswift.io/kernel-profile` | SwiftKernel name | KubeSwift default (`sandbox`) |
| `spin.kubeswift.io/sandbox-pool` | SwiftSandboxPool name in the same namespace | none |
| `spin.kubeswift.io/node-selector` | `key=value,key=value` | none |
| `spin.kubeswift.io/default-cpu` | quantity | controller `--default-cpu` (1) |
| `spin.kubeswift.io/default-memory` | quantity | controller `--default-memory` (512Mi) |

Any other annotation with the `spin.kubeswift.io/` prefix makes the executor
invalid, so a misspelled key is reported rather than ignored. Network mode
`none` is rejected because Spin downloads the application over the network
when it starts.

kubeswift-spin never reads the Secrets named by the pull-secret and
verify-key annotations. It passes their names to KubeSwift, which reads them
in the SpinApp's namespace.

## Sandboxes

Each replica is one SwiftSandbox in the SpinApp's namespace:

- **Name**: `<spinapp>-<ordinal>`, for example `hello-0`. Names longer than
  58 characters, or containing dots, are truncated and suffixed with a hash
  of the full SpinApp name so that the result is a valid DNS label of at
  most 63 characters and never collides with another SpinApp.
- **Owner**: a controller owner reference to the SpinApp, so deleting the
  SpinApp deletes its sandboxes through garbage collection. No finalizer is
  used.
- **Labels**:

  | Label | Value |
  |---|---|
  | `app.kubernetes.io/managed-by` | `kubeswift-spin` |
  | `spin.kubeswift.io/app` | SpinApp name (label-safe form) |
  | `core.spinkube.dev/app-name` | SpinApp name (label-safe form) |
  | `spin.kubeswift.io/ordinal` | replica ordinal |
  | `spin.kubeswift.io/revision` | hash of the sandbox spec |
  | `spin.kubeswift.io/executor` | executor name (label-safe form) |

  Labels are used for selection only. A sandbox is owned only if its
  controller owner reference points to the SpinApp's UID, so an object with
  matching labels but another owner is never adopted, modified or deleted.

### Sandbox spec

| SwiftSandbox field | Value |
|---|---|
| `image` | runtime image (profile or controller default) |
| `imagePullSecret` | profile `runtime-image-pull-secret` |
| `verifyKeySecretRef` | profile `runtime-image-verify-key-secret` |
| `cpu` | see [CPU and memory](#cpu-and-memory) |
| `memory` | see [CPU and memory](#cpu-and-memory) |
| `command` | `/usr/local/bin/kubeswift-spin-entrypoint` |
| `args` | the Spin command line below |
| `env` | variables, OpenTelemetry endpoints, invocation memory limit, runtime config |
| `network.mode` | profile network mode |
| `rootfsMode` | profile rootfs mode |
| `kernelProfileRef` | profile kernel profile |
| `nodeSelector` | profile node selector |
| `poolRef` | profile sandbox pool |

`timeout` and `ttl` are never set: a Spin server runs until it is replaced.
GPU fields, `scratchDisk` and `model` are never set.

### Spin command line

The entrypoint receives the arguments of `spin up`:

```
up --from=<spec.image> --listen=0.0.0.0:3000
   [--runtime-config-file=/var/lib/kubeswift-spin/runtime-config.toml]
   [--component-id=<component> ...]
```

User-derived values are passed in `--flag=value` form so that a value
starting with `-` cannot become a separate flag. `--state-dir` is not
passed: for a registry application Spin then keeps its default key-value
store and SQLite database in memory and writes component output only to the
console, not to log files in guest memory.

Port 3000 is unprivileged, so Spin needs no capability after the entrypoint
drops root (see [runtime-image.md](runtime-image.md)).

### Environment

Only literal values reach the sandbox environment:

| Variable | Source |
|---|---|
| `SPIN_VARIABLE_<NAME>` | `spec.variables[].value`; Spin's environment provider reads these |
| `OTEL_EXPORTER_OTLP_ENDPOINT` and the traces, metrics and logs variants | executor `deploymentConfig.otel` |
| `SPIN_MAX_INSTANCE_MEMORY` | `spec.invocationLimits.memory`, in bytes |
| `KUBESWIFT_SPIN_RUNTIME_CONFIG_B64` | rendered runtime configuration, base64 |

KubeSwift v0.15.1 writes the sandbox environment into a plain ConfigMap (the
runtime intent) and ignores `valueFrom`. kubeswift-spin therefore never puts
a secret value or a `valueFrom` reference in a sandbox spec; see
[security-model.md](security-model.md#secrets).

### Runtime configuration

When `spec.runtimeConfig` sets `keyValueStores`, `sqliteDatabases` or
`llmCompute`, kubeswift-spin renders a Spin runtime-config TOML document in
the same layout Spin Operator uses: one table per store with a `type` key and
the options as strings. The entrypoint writes it to
`/var/lib/kubeswift-spin/runtime-config.toml` (mode 0600, owned by the Spin
user) and removes the variable from Spin's environment.

Options whose name denotes a credential (`token`, `password`, `key`,
`secret`, `credentials`, `connection_string`, and names ending in `_token`,
`_password`, `_secret`, `_key` or `_credentials`) must be empty or absent.
URLs with embedded user information are rejected. Spin requires an
`auth_token` key for `llm_compute` type `remote_http`; an empty value is
accepted.

### CPU and memory

KubeSwift sizes a microVM with an integer vCPU count and a memory quantity.
kubeswift-spin converts SpinApp resources as follows.

**CPU**: take `resources.limits.cpu`, else `resources.requests.cpu`, else the
profile default. Round up to a whole CPU. `100m`, `250m`, `500m` and `1` all
give 1 vCPU; `1500m` gives 2. Zero or negative values are rejected, and so
are values above `--max-vcpus` (default 8) after rounding.

**Memory**: take `resources.limits.memory`, else `resources.requests.memory`,
else the profile default. Convert to bytes with the quantity's own suffix
semantics (`Mi` and `Gi` are binary, `M` and `G` decimal) and round up to a
whole MiB. `1Gi` gives `1Gi` (1024Mi); `1G` gives `954Mi`. Values below
`--min-memory` (default 256Mi) or above `--max-memory` (default 16Gi) are
rejected, not adjusted.

A limit is preferred because a microVM's vCPUs and RAM are hard ceilings
reserved for its lifetime, the role a container limit plays. When both a
request and a limit are set, the request may not exceed the limit.

## Replica lifecycle

The planner (`internal/rollout`) is a pure function of the observed
sandboxes; every transition below is covered by unit and envtest tests.

- **Create and scale up**: missing ordinals below `spec.replicas` are
  created.
- **Scale down**: ordinals at or above `spec.replicas` are deleted, highest
  first.
- **Replacement**: SwiftSandbox specs are immutable except `ttl`, so a
  change is applied by deleting a replica and recreating it at the new
  revision. An outdated replica that is not running is replaced at once. A
  running outdated replica is replaced only while every desired replica is
  running, one at a time, highest ordinal first. A broken new revision
  therefore stops the rollout after one replica.
- **Failure**: KubeSwift launcher pods never restart, so when Spin exits the
  sandbox becomes `Completed` or `Failed`. kubeswift-spin replaces it after a
  backoff of 10 seconds, doubling per consecutive failure up to 5 minutes,
  and resets the backoff once the replica runs again. The backoff state is in
  memory and restarts from 10 seconds after a controller restart.
- **Deletion**: sandboxes are deleted with foreground propagation, so a
  replacement with the same name is created only after KubeSwift's launcher
  pod, runtime-intent ConfigMap and NetworkPolicy are gone.
- **Stale sandboxes**: an owned sandbox whose name does not match its
  ordinal label is deleted.
- **Conflicts**: if a sandbox name is taken by an object the SpinApp does not
  control, that ordinal is skipped and `Progressing` reports
  `SandboxConflict`.

While a SpinApp has unsupported configuration, an invalid executor or an
incompatible warm pool, kubeswift-spin creates and deletes nothing for it;
existing sandboxes keep running.

## What triggers replacement

The revision label is a hash of the full sandbox spec. A change to any of
these replaces the replicas (one at a time, as above):

- `spec.image`, `spec.components`, `spec.variables`,
  `spec.runtimeConfig`, `spec.invocationLimits`, `spec.resources` (when the
  rounded vCPU count or MiB value changes)
- the executor profile: runtime image, pull secret, verify key, network
  mode, rootfs mode, kernel profile, node selector, pool, default CPU or
  memory (when used), OpenTelemetry endpoints
- a different executor that kubeswift-spin manages

These do not replace replicas:

- `spec.replicas`, labels and annotations on the SpinApp,
  `spec.serviceAnnotations`
- restarting or upgrading the kubeswift-spin controller, as long as the new
  version renders the same sandbox spec. A release that changes the
  rendered spec (for example the Spin command line or the entrypoint
  contract) rolls every replica and says so in [CHANGELOG.md](../CHANGELOG.md).

## Executor changes

| Event | Behavior |
|---|---|
| `spec.executor` changes to an executor kubeswift-spin does not manage | All sandboxes of the SpinApp are deleted; status is left to the new executor. |
| The managed-by label is removed from the executor | Same as above, for every SpinApp using it. |
| The executor is deleted or missing | Sandboxes are kept; `Progressing=False`, reason `ExecutorNotFound`. Spin Operator normally blocks executor deletion while SpinApps use it. |
| The executor becomes invalid | Sandboxes are kept; `Progressing=False`, reason `ExecutorInvalid`. |

## Warm pools

When a profile sets `spin.kubeswift.io/sandbox-pool`, each sandbox gets
`poolRef` and KubeSwift checks out a pre-booted slot instead of booting
cold.

KubeSwift v0.15.1 compares only image, network mode and verification key at
checkout, and on a mismatch falls back to a cold boot. It does not compare
CPU, memory, rootfs mode, kernel profile or node selector, so a slot of a
different shape could be claimed. kubeswift-spin therefore checks the full
shape itself and refuses to create sandboxes for an incompatible or missing
pool (`Progressing=False`, reason `WarmPoolIncompatible`, with the
mismatching fields in the message). When the pool is compatible but has no
free slot, KubeSwift's own cold fallback applies and is recorded by KubeSwift
as a `PoolColdFallback` Event on the SwiftSandbox. The pool is a capacity
mechanism only: kubeswift-spin still creates one SwiftSandbox per replica.

## Status

kubeswift-spin writes `status.activeScheduler` (the executor name),
`status.readyReplicas`, and the `Available` and `Progressing` conditions,
using a JSON merge patch with the resource version it read. Conditions of
other types are preserved.

`readyReplicas` counts replicas whose application readiness was verified.
A running microVM does not prove that Spin is accepting requests, and
KubeSwift v0.15.1 has no way to check, so `readyReplicas` stays 0.

| Condition | Status | Reason | Meaning |
|---|---|---|---|
| Progressing | True | `SandboxCreating` | sandboxes are being created or are pending |
| Progressing | True | `SandboxMaterializing` | KubeSwift is building the runtime rootfs |
| Progressing | True | `RollingUpdate` | replicas are being replaced at a new revision |
| Progressing | True | `SandboxRunning` | all desired sandboxes run at the current revision |
| Progressing | False | `UnsupportedConfiguration` | the SpinApp uses a field that cannot be realized |
| Progressing | False | `ExecutorInvalid` | the executor profile is invalid |
| Progressing | False | `ExecutorNotFound` | the executor no longer exists |
| Progressing | False | `WarmPoolIncompatible` | the selected pool is missing or has another shape |
| Progressing | False | `SandboxConflict` | a sandbox name is taken by a foreign object |
| Progressing | False | `RuntimeImageUnavailable` | KubeSwift could not pull, verify or materialize the runtime image |
| Progressing | False | `SandboxFailed` | Spin exited or the guest failed; the replica is replaced after backoff |
| Available | True | `ApplicationReady` | at most one desired replica is not ready |
| Available | False | `NetworkUnavailable` | sandboxes run, but readiness cannot be verified and the app cannot be exposed |
| Available | False | same as Progressing | nothing is running yet |

Messages name fields, variables and objects, never values.

## Events

Events are recorded on the SpinApp with the `events.k8s.io/v1` API:
`SandboxCreated`, `SandboxDeleted`, `SandboxFailed`, `UnsupportedConfiguration`,
`PartiallySupported`, `ExecutorInvalid`, `ExecutorNotFound`,
`WarmPoolIncompatible`, `SandboxConflict`, `ExecutorChanged`. Configuration
Events fire once per SpinApp generation or when the message changes.

## Namespace prerequisites

KubeSwift resolves the sandbox kernel and its Secrets in the sandbox's
namespace. Each namespace that runs SpinApps therefore needs:

- a Ready SwiftKernel named `sandbox` (or the profile's kernel profile),
- a kubeswift-spin executor,
- the runtime image pull secret and verify key Secrets, if the profile names
  them.

Until the kernel exists, sandboxes stay pending and `Progressing` reports
the KubeSwift reason (`KernelNotFound` or `KernelNotReady`) in its message.
