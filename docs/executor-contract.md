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
label and `createDeployment: true` is not realized, because Spin Operator
creates a Deployment for it and owns the SpinApp status. kubeswift-spin
records an `ExecutorInvalid` Warning Event on each affected SpinApp and does
not write their status, so the two controllers never overwrite each other.

SpinApps whose executor lacks the label are ignored: kubeswift-spin creates
nothing and writes no status for them. Sandboxes it created while the
SpinApp used a managed executor are deleted (see
[Executor changes](#executor-changes)).

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

kubeswift-spin therefore owns the SpinApp status for its executors. On
KubeSwift v0.16.0 the Service routes to the replicas' launcher pods, which
carry the label it selects; on older KubeSwift it has no endpoints (see
[networking.md](networking.md)).

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
| `spin.kubeswift.io/egress-allow` | JSON list of 1 to 32 KubeSwift egress rules; only with network mode `restricted`; needs KubeSwift v0.16.0 | none |
| `spin.kubeswift.io/ingress-from` | JSON list of 1 to 16 NetworkPolicy peers allowed to reach the Spin HTTP port; needs KubeSwift v0.16.0 | any source |

Any other annotation with the `spin.kubeswift.io/` prefix makes the executor
invalid, so a misspelled key is reported rather than ignored. Network mode
`none` is rejected because Spin downloads the application over the network
when it starts. `egress-allow` and `ingress-from` on a cluster whose
KubeSwift lacks the feature also make the executor invalid
(`ExecutorInvalid`).

An egress rule names exactly one destination, a Service or an IPv4 CIDR,
and optionally ports (`protocol` `TCP` or `UDP`, default `TCP`). A Service
without `namespace` is in the SpinApp's namespace:

```yaml
metadata:
  annotations:
    spin.kubeswift.io/egress-allow: '[{"service":{"name":"llm","namespace":"inference"},"ports":[{"port":8000}]},{"cidr":"10.20.0.0/16"}]'
    spin.kubeswift.io/ingress-from: '[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"frontend"}}}]'
```

The Helm chart renders both from the executor values `egressAllow` and
`ingressFrom`. `config/executor/kubeswift-egress.yaml` is a complete
example.

kubeswift-spin never reads the Secrets named by the pull-secret and
verify-key annotations, or any other Secret. It passes their names to
KubeSwift, which reads them in the SpinApp's namespace.

## Sandboxes

Each replica is one SwiftSandbox in the SpinApp's namespace:

- **Name**: `<spinapp>-<ordinal>`, for example `hello-0`. Names longer than
  58 characters, names containing dots, and names that already end in `-`
  followed by 12 hexadecimal characters are truncated and suffixed with a
  12-character (48-bit) hash of the full SpinApp name. The result is a valid
  DNS label of at most 63 characters; because literal names never end in a
  hash suffix, two SpinApps in a namespace can share a prefix only through a
  hash collision.
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
| `env` | see [Environment](#environment) |
| `secretFiles` | `runtimeConfig.loadFromSecret` and `imagePullSecrets` (see below); KubeSwift v0.16.0 |
| `network.mode` | profile network mode |
| `network.egress.allow` | profile `egress-allow`; KubeSwift v0.16.0 |
| `network.ports` | `[{name: http-app, port: 3000, protocol: TCP}]`; KubeSwift v0.16.0 |
| `network.ingress.from` | profile `ingress-from`; unset means any source; KubeSwift v0.16.0 |
| `podMetadata.labels` | `core.spinkube.dev/app.<name>.status: ready`, `core.spinkube.dev/app-name: <name>`, plus `spec.podLabels`; KubeSwift v0.16.0 |
| `readinessProbe` | from `spec.checks.readiness`, else a TCP check; KubeSwift v0.16.0 |
| `livenessProbe` | from `spec.checks.liveness`, else unset; KubeSwift v0.16.0 |
| `rootfsMode` | profile rootfs mode |
| `kernelProfileRef` | profile kernel profile |
| `nodeSelector` | profile node selector |
| `poolRef` | profile sandbox pool |

Fields marked KubeSwift v0.16.0 are set only when the installed KubeSwift
serves them (see [networking.md](networking.md#feature-detection)); the API
server would otherwise drop them silently. `ports`, `podMetadata` and the
probes are set together, for every replica, whenever exposure is detected.

`timeout` and `ttl` are never set: a Spin server runs until it is replaced.
GPU fields, `scratchDisk`, `model` and `artifacts` are never set. Spin 4.2.1
can run an application only from a manifest, a `.wasm` file or a registry
reference, not from a local OCI layout, so KubeSwift's read-only artifact
mounts cannot replace the in-guest pull.

### Probes

A SpinKube check becomes a probe against the guest, run by the KubeSwift
launcher:

| Probe field | Value |
|---|---|
| `httpGet.path`, `httpGet.httpHeaders` | from `spec.checks.<kind>.httpGet` |
| `httpGet.port` | `http-app` |
| `httpGet.scheme` | `HTTP` |
| `initialDelaySeconds`, `timeoutSeconds`, `periodSeconds`, `successThreshold`, `failureThreshold` | copied from the check |

Without `spec.checks.readiness`, the readiness probe is a TCP check on
`http-app` with `periodSeconds: 2`, `timeoutSeconds: 1` and
`failureThreshold: 3`: the replica is ready once Spin accepts connections.
Spin Operator v0.6.1 rejects a check whose `httpGet` has no `httpHeaders`;
set `httpHeaders: []` (see
[compatibility.md](compatibility.md#known-upstream-issues)).

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

| Variable | Source |
|---|---|
| `SPIN_VARIABLE_<NAME>` | `spec.variables[]`: a literal `value`, or `valueFrom.secretKeyRef` copied as a reference; Spin's environment provider reads these |
| `OTEL_EXPORTER_OTLP_ENDPOINT` and the traces, metrics and logs variants | executor `deploymentConfig.otel` |
| `SPIN_MAX_INSTANCE_MEMORY` | `spec.invocationLimits.memory`, in bytes |
| `KUBESWIFT_SPIN_RUNTIME_CONFIG_B64` | rendered runtime configuration, base64 |
| `KUBESWIFT_SPIN_SECRET_<n>` | `valueFrom.secretKeyRef` of the n-th Secret-backed runtime-config option, as a reference |
| `KUBESWIFT_SPIN_RUNTIME_CONFIG_FILE` | `/run/kubeswift-spin/runtime-config.toml` when `runtimeConfig.loadFromSecret` is set |
| `KUBESWIFT_SPIN_REGISTRY_AUTH_FILES` | comma-separated registry credential files from `imagePullSecrets` |

KubeSwift writes literal environment values in plain text into the
sandbox's runtime-intent ConfigMap. A `valueFrom.secretKeyRef` stays a
reference in the SwiftSandbox: KubeSwift v0.16.0 resolves it in the
launcher, with its own per-sandbox ServiceAccount, and hands the value to
the guest without writing it to any object, log or node disk. KubeSwift
refuses every other `valueFrom` source. kubeswift-spin never reads a Secret
and never puts a Secret value in a sandbox spec; see
[security-model.md](security-model.md#secrets).

The entrypoint removes the `KUBESWIFT_SPIN_*` variables before it starts
Spin.

### Runtime configuration

When `spec.runtimeConfig` sets `keyValueStores`, `sqliteDatabases` or
`llmCompute`, kubeswift-spin renders a Spin runtime-config TOML document in
the same layout Spin Operator uses: one table per store with a `type` key and
the options as strings. The entrypoint writes it to
`/var/lib/kubeswift-spin/runtime-config.toml` (mode 0600, owned by UID
65532) and passes `--runtime-config-file` with that path.

**Secret-backed options** (KubeSwift v0.16.0). An option with
`valueFrom.secretKeyRef` is rendered as the placeholder
`kubeswift-spin-secret:KUBESWIFT_SPIN_SECRET_<n>`, and the sandbox gets the
environment variable `KUBESWIFT_SPIN_SECRET_<n>` with that `secretKeyRef`.
The entrypoint, still running as root, parses the TOML, replaces each
placeholder with the variable's value, re-encodes the document (so any value
is quoted correctly), and removes the variables before Spin starts. For
example:

```yaml
llmCompute:
  type: remote_http
  options:
    - name: url
      value: http://llm.inference.svc.cluster.local:8000
    - name: api_type
      value: open_ai
    - name: auth_token
      valueFrom:
        secretKeyRef:
          name: llm-token
          key: token
```

**`loadFromSecret`** (KubeSwift v0.16.0). The `runtime-config.toml` key of
the named Secret is delivered as the secret file
`/run/kubeswift-spin/runtime-config.toml` (root-owned, mode 0400), and
`KUBESWIFT_SPIN_RUNTIME_CONFIG_FILE` points to it. The entrypoint copies it
to `/var/lib/kubeswift-spin/runtime-config.toml` (mode 0600, UID 65532).
`loadFromSecret` replaces the whole runtime configuration, so combining it
with `keyValueStores`, `sqliteDatabases` or `llmCompute` is rejected; Spin
Operator silently ignores the other fields in that case. Secret files need
the sandbox kernel 6.6.14 or later.

**Literal options.** Options whose name denotes a credential (`token`,
`password`, `key`, `secret`, `credentials`, `connection_string`, and names
ending in `_token`, `_password`, `_secret`, `_key` or `_credentials`) must
be empty or use `valueFrom.secretKeyRef`. Values that look like URLs with
user information, or with a query parameter whose name contains `token`,
`password`, `passwd`, `secret`, `auth`, `key`, `sig` or `credential`, are
rejected. The check works on the raw text and rejects even when the URL
does not parse. `configMapKeyRef` is rejected because KubeSwift does not
resolve it for sandboxes. Spin requires an `auth_token` key for
`llm_compute` type `remote_http`; an empty value is accepted.

### Registry credentials

`spec.imagePullSecrets` (KubeSwift v0.16.0) lets Spin pull the application
from a private registry. The `.dockerconfigjson` key of the i-th Secret is
delivered as the secret file `/run/kubeswift-spin/registry-auth/<i>.json`,
and `KUBESWIFT_SPIN_REGISTRY_AUTH_FILES` lists the files. The entrypoint
merges their `auths` sections (a later Secret wins for the same registry)
into `/var/lib/kubeswift-spin/home/.docker/config.json` (mode 0600, UID
65532), where Spin's registry client looks. The Secrets must be of type
`kubernetes.io/dockerconfigjson`. The credentials are inside the guest and
readable by the Spin process, not by Wasm components. This path is tested in
the Docker runtime test only, not in a microVM.

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
  revision. A replica counts as available when it is ready (KubeSwift
  v0.16.0, readiness probe passing) or, on older KubeSwift, when it is
  `Running`. An outdated replica that is not available is replaced at once.
  An available outdated replica is replaced only while every desired
  replica is available, one at a time, highest ordinal first. A broken new
  revision therefore stops the rollout after one replica. There is no
  surge: a replica is deleted before its replacement exists. With two or
  more replicas the SpinApp Service keeps routing to the others, and the
  KVM e2e test sees no failed request during a rolling update of two
  replicas (see [compatibility.md](compatibility.md#tested-versions)).
  **A SpinApp with one replica is unavailable during every replacement**
  (any spec change, including a runtime image change on upgrade), for the
  sandbox deletion plus a boot (a cold boot to `Available` took about 20
  seconds on the lab cluster). Use
  at least two replicas for applications that must stay reachable.
- **Failure**: KubeSwift launcher pods never restart, so when Spin exits the
  sandbox becomes `Completed` or `Failed`. kubeswift-spin replaces it after a
  backoff of 10 seconds, doubling per consecutive failure up to 5 minutes.
  The count resets only after a replica has run for 10 minutes without
  failing, because a guest is `Running` before Spin has pulled the
  application. The backoff state is in memory and restarts from 10 seconds
  after a controller restart. When a liveness probe fails `failureThreshold`
  times, KubeSwift marks the sandbox `Failed` (`LivenessProbeFailed`) and
  restarts nothing; kubeswift-spin then replaces it with the same backoff.
  The KVM e2e test covers this, including a replacement that fails again
  and recovery.
- **Deletion**: sandboxes are deleted with foreground propagation, so a
  replacement with the same name is created only after KubeSwift's launcher
  pod, runtime-intent ConfigMap and NetworkPolicy are gone.
- **Stale sandboxes**: an owned sandbox whose name does not match its
  ordinal label is deleted.
- **Conflicts**: if a sandbox name is taken by an object the SpinApp does not
  control (checked with a direct API read, not the cache), that ordinal is
  not created and `Progressing` reports `SandboxConflict`. The SpinApp is
  rechecked every 30 seconds, so the replica is created once the name is
  free.

While a SpinApp has unsupported configuration, an invalid executor or an
incompatible warm pool, kubeswift-spin creates and deletes nothing for it;
existing sandboxes keep running.

## What triggers replacement

The revision label is a hash of the full sandbox spec. A change to any of
these replaces the replicas (one at a time, as above):

- `spec.image`, `spec.components`, `spec.variables`,
  `spec.runtimeConfig`, `spec.invocationLimits`, `spec.resources` (when the
  rounded vCPU count or MiB value changes)
- `spec.checks`, `spec.podLabels`, `spec.imagePullSecrets` (Secret names
  only)
- the executor profile: runtime image, pull secret, verify key, network
  mode, egress allowlist, ingress peers, rootfs mode, kernel profile, node
  selector, pool, default CPU or memory (when used), OpenTelemetry endpoints
- a KubeSwift upgrade that adds a detected feature (for example v0.15.1 to
  v0.16.0): ports, probes and launcher pod labels enter the spec, so every
  replica is replaced once. The controller re-reads the schema at most every
  `--capability-refresh` (default 5 minutes); a failed read keeps the last
  result, so a transient error never changes the spec
- a different executor that kubeswift-spin manages, when its profile renders
  a different spec (the `spin.kubeswift.io/executor` label of existing
  sandboxes is not updated when the spec is identical)

These do not replace replicas:

- `spec.replicas`, labels and annotations on the SpinApp,
  `spec.serviceAnnotations`
- changing the contents of a referenced Secret. The revision hashes Secret
  names and keys, not values, and values are delivered when a sandbox
  starts, so a rotated Secret takes effect only when a replica is replaced
  for another reason. To roll deliberately, change something in the spec,
  for example a variable.
- restarting or upgrading the kubeswift-spin controller, as long as the new
  version renders the same sandbox spec. The runtime image has its own
  version (`runtime/VERSION`, chart value `runtimeImage.tag`) and changes
  only when Spin, the entrypoint or the runtime contract changes, so a
  controller release normally keeps it. A release that changes the rendered
  spec (a new runtime image, a different Spin command line) rolls every
  replica and says so in [CHANGELOG.md](../CHANGELOG.md). A golden test
  (`internal/translate/golden_test.go`) fails when the rendered spec changes,
  so this cannot happen by accident.

## Executor changes

| Event | Behavior |
|---|---|
| `spec.executor` changes to an executor kubeswift-spin does not manage | All sandboxes of the SpinApp are deleted; status is left to the new executor. |
| The managed-by label is removed from the executor | Same as above, for every SpinApp using it. |
| The executor is deleted or missing | Sandboxes are kept; `Progressing=False`, reason `ExecutorNotFound`. Spin Operator normally blocks executor deletion while SpinApps use it. |
| The executor becomes invalid | Sandboxes are kept; `Progressing=False`, reason `ExecutorInvalid`. |
| The executor is labelled but sets `createDeployment: true` | Sandboxes are kept; an `ExecutorInvalid` Event is recorded, and status is left to Spin Operator, which realizes such executors. |

## Warm pools

When a profile sets `spin.kubeswift.io/sandbox-pool`, each sandbox gets
`poolRef` and KubeSwift checks out a pre-booted slot instead of booting
cold.

kubeswift-spin compares the pool with the sandbox spec before it sets
`poolRef`: image, CPU, memory, network mode, rootfs mode, kernel profile,
verify key, node selector, `network.ports`, `network.egress` and
`network.ingress.from` (in order, as KubeSwift compares it). It refuses
to create sandboxes for an incompatible or missing pool (`Progressing=False`,
reason `WarmPoolIncompatible`, with the mismatching fields in the message).
An unset kernel profile is compared as `sandbox`, KubeSwift's default, and an
egress Service without a namespace is compared in the namespace of the
object that declares it. KubeSwift v0.16.0 also compares the full slot shape
at checkout and boots cold on a mismatch; KubeSwift v0.15.1 compared only
image, network mode and verify key.

The image is compared as an exact string. The released chart passes the
runtime image to the controller by digest
(`ghcr.io/kubeswift-io/kubeswift-spin-runtime@sha256:...`, listed in the
release's `images.txt`), so a pool must use that same reference, not the
`spin-4.2.1-r1` tag. The controller's reference is in its arguments:

```bash
kubectl -n kubeswift-spin-system get deployment kubeswift-spin -o jsonpath='{.spec.template.spec.containers[0].args}'
```

On KubeSwift v0.16.0 every sandbox exposes `http-app`, so the pool must
declare the same port, and the same egress allowlist and ingress peers as
the executor. With an image built from source, by tag:

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata:
  name: spin-warm
spec:
  image: ghcr.io/kubeswift-io/kubeswift-spin-runtime:spin-4.2.1-r1
  cpu: 1
  memory: 512Mi
  network:
    mode: restricted
    ports:
      - name: http-app
        port: 3000
  minWarm: 2
  maxWarm: 4
```

Whether the SwiftSandboxPool API exists is discovered at controller startup; installing it later requires restarting
the controller. When the pool is compatible but has no
free slot, KubeSwift's own cold fallback applies and is recorded by KubeSwift
as a `PoolColdFallback` Event on the SwiftSandbox. The pool is a capacity
mechanism only: kubeswift-spin still creates one SwiftSandbox per replica.

## Status

kubeswift-spin writes `status.activeScheduler` (the executor name),
`status.readyReplicas`, and the `Available` and `Progressing` conditions,
using a JSON merge patch with the resource version it read. Conditions of
other types are preserved. When the SpinApp changed in between, the API
server rejects the patch with a conflict and the reconcile is retried on
fresh data. A conflict is logged only at debug level and counted as
`result="conflict"`, not as an error.

`readyReplicas` counts replicas whose application readiness was verified. A
replica counts only when its SwiftSandbox is `Running`, has a readiness
probe, and KubeSwift reports the `WorkloadReady` condition `True`. A running
microVM alone does not prove that Spin accepts requests. On KubeSwift
v0.15.1 sandboxes have no probe, so `readyReplicas` stays 0.

`Available` follows Deployment semantics with `maxUnavailable` 1: it is
`True` when at least `replicas - 1` replicas (and at least one) are ready.

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
| Available | False | `ApplicationNotReady` | sandboxes run, but too few pass the readiness probe yet ("N of M replicas ready; K running sandboxes do not pass the readiness check yet") |
| Available | False | `NetworkUnavailable` | KubeSwift before v0.16.0 only: sandboxes run, but readiness cannot be verified and the app cannot be exposed |
| Available | False | same as Progressing | nothing is running yet |

Messages name fields, variables and objects, never values. A condition
message lists at most five problems ("and N more problems"), echoes names
truncated to 64 characters, and is capped at 4096 bytes; Event notes are
capped at 1000 bytes.

## Events

Events are recorded on the SpinApp with the `events.k8s.io/v1` API:
`SandboxCreated`, `SandboxDeleted`, `SandboxFailed`, `UnsupportedConfiguration`,
`PartiallySupported`, `ExecutorInvalid`, `ExecutorNotFound`,
`WarmPoolIncompatible`, `SandboxConflict`, `ExecutorChanged`. Configuration
Events fire once per SpinApp generation or when the message changes.

## Namespace prerequisites

KubeSwift resolves the sandbox kernel and its Secrets in the sandbox's
namespace. Each namespace that runs SpinApps therefore needs:

- a Ready SwiftKernel named `sandbox` (or the profile's kernel profile);
  version 6.6.14 or later when SpinApps use secret files
  (`runtimeConfig.loadFromSecret`, `imagePullSecrets`),
- a kubeswift-spin executor,
- the runtime image pull secret and verify key Secrets, if the profile names
  them, and the Secrets SpinApps reference.

Until the kernel exists, sandboxes stay pending and `Progressing` reports
the KubeSwift reason (`KernelNotFound` or `KernelNotReady`) in its message.
