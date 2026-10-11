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
| `spin.kubeswift.io/readiness-startup` | `auto`, `always`, `never`; see [Readiness startup](#readiness-startup); `always` needs KubeSwift v0.16.2 | `auto` |
| `spin.kubeswift.io/drain-period` | a duration from `0s` to `10m` (for example `30s`): how long a deleted replica keeps serving after it has left the Service before KubeSwift stops it (`spec.termination.drainPeriod`); see [Rolling updates](#rolling-updates); needs KubeSwift v0.16.3 | unset (KubeSwift's 5 seconds) |

Any other annotation with the `spin.kubeswift.io/` prefix makes the executor
invalid, so a misspelled key is reported rather than ignored. Network mode
`none` is rejected because Spin downloads the application over the network
when it starts. `egress-allow`, `ingress-from` and `readiness-startup: always`
on a cluster whose KubeSwift lacks the feature also make the executor
invalid (`ExecutorInvalid`).

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
  | `spin.kubeswift.io/surge` | `true` on the extra replica of a rollout (see [Rolling updates](#rolling-updates)); absent otherwise |

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
| `readinessStartup` | `{}` (KubeSwift defaults) when [readiness startup](#readiness-startup) applies; KubeSwift v0.16.2 |
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

When a check omits `initialDelaySeconds`, the Spin Operator CRD defaults it
to 10, so
the first probe runs 10 seconds after KubeSwift starts probing the guest,
even when the application answers well before that. The project's examples
set `initialDelaySeconds: 1`; see
[performance.md](performance.md#readiness-configuration).

Without `spec.checks.readiness`, the readiness probe is a TCP check on
`http-app` with `periodSeconds: 2`, `timeoutSeconds: 1` and
`failureThreshold: 3`: the replica is ready once Spin accepts connections.
Spin Operator v0.6.1 rejects a check whose `httpGet` has no `httpHeaders`;
set `httpHeaders: []` (see
[compatibility.md](compatibility.md#known-upstream-issues)).

### Readiness startup

On KubeSwift v0.16.2 or later, a replica's readiness check can run quickly
from the replica's start until it first passes (SwiftSandbox
`spec.readinessStartup`), so a replica that answers a few hundred
milliseconds after it starts is reported ready then, not at the next
whole-second check time. kubeswift-spin sets `readinessStartup: {}`, which
uses KubeSwift's defaults:

- the check is retried every 100 ms, for at most 30 s from the replica's
  start (the window);
- failures before the first pass do not count toward `failureThreshold`;
- after the first pass, or once the window has passed, the check runs every
  `periodSeconds` with the usual thresholds, so a replica that never
  becomes ready is not checked every 100 ms forever;
- `initialDelaySeconds` does not apply before the first pass.

The executor annotation `spin.kubeswift.io/readiness-startup` decides when
it is used:

| Value | SpinApp without `spec.checks.readiness` | SpinApp with `spec.checks.readiness` |
|---|---|---|
| `auto` (default) | used, for the TCP check kubeswift-spin adds | not used: the check keeps its `initialDelaySeconds` |
| `always` | used | used: the check's `initialDelaySeconds` does not delay the first checks |
| `never` | not used | not used |

`auto` never changes how a check written in the SpinApp behaves. Choose
`always` for an executor whose applications declare HTTP checks and should be
ready as soon as those checks pass. On a KubeSwift without
`spec.readinessStartup`, `auto` falls back to whole-second checks and
`always` makes the executor invalid.

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
readable by the Spin process, not by Wasm components. The Docker runtime
test and the KVM e2e test (phase 6, an htpasswd registry) cover this path.

To run an application from a private registry, create the Secret in the
SpinApp's namespace:

```bash
kubectl -n <namespace> create secret docker-registry app-registry --docker-server=registry.example.com --docker-username=<user> --docker-password=<password>
```

and reference it from the SpinApp:

```yaml
spec:
  image: registry.example.com/team/app:v1
  imagePullSecrets:
    - name: app-registry
```

Requirements:

- `spec.image` starts with the registry host, the same host as
  `--docker-server`.
- The registry serves HTTPS with a certificate that the runtime image's CA
  bundle trusts. kubeswift-spin has no option for a custom CA
  (`deploymentConfig.caCertSecret` is rejected) and never passes
  `--insecure`.
- The registry is reachable from the guest: under network mode
  `restricted`, a public address or an `egress-allow` entry.
- KubeSwift v0.16.0 and a sandbox SwiftKernel 6.6.14 or later (secret
  files).

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
- **Scale down**: ordinals at or above `spec.replicas`, other than the
  surge replica of a rollout, are deleted. While
  fewer than `spec.replicas` of the remaining replicas are available (for
  example right after a scale-up, or at the end of a rolling update), the
  lowest of them that are available are kept until enough of the remaining
  ones are; they serve traffic and count toward `readyReplicas` meanwhile.
  Replicas that are not available go at once.
- **Replacement**: SwiftSandbox specs are immutable except `ttl`, so a
  change is applied by deleting a replica and recreating it at the new
  revision. A replica counts as available when it is ready (KubeSwift
  v0.16.0, readiness probe passing) or, on older KubeSwift, when it is
  `Running`. An outdated replica that is not available is replaced at once.
  For available outdated replicas, see [Rolling updates](#rolling-updates).
- **Failure**: KubeSwift launcher pods never restart, so when Spin exits the
  sandbox becomes `Completed` or `Failed`. kubeswift-spin replaces it after
  a backoff of 10 seconds, doubling per consecutive failure up to 5 minutes.
  The count resets when a replica has run for 10 minutes without failing (a
  guest is `Running` before Spin has pulled the application, so an earlier
  reset would keep the delay at its base), and when the failing replica runs
  a different revision than the previous failure. A failed replica from an
  earlier revision than the current spec is replaced 10 seconds after its
  failure, so a fixed spec is not held back by the broken one's backoff. The
  backoff state is in memory and restarts from 10 seconds after a controller
  restart. When a liveness probe fails `failureThreshold` times, KubeSwift
  marks the sandbox `Failed` (`LivenessProbeFailed`) and restarts nothing;
  kubeswift-spin then replaces it with the same backoff. The KVM e2e test
  covers this, including a replacement that fails again and recovery.
- **Deletion**: sandboxes are deleted with foreground propagation, so a
  replacement with the same name is created only after KubeSwift's launcher
  pod, runtime-intent ConfigMap and NetworkPolicy are gone.
- **Stale sandboxes**: an owned sandbox whose name does not match its
  ordinal label is deleted.
- **Conflicts**: if a sandbox name is taken by an object the SpinApp does
  not control (checked with a direct API read, not the cache), that ordinal
  is not created and `Progressing` reports `SandboxConflict`. This includes
  the surge name `<spinapp>-<replicas>`: a foreign object with that name
  stalls rollouts, while the existing replicas keep serving. The SpinApp is
  rechecked every 30 seconds, so the replica is created once the name is
  free. One case is not a conflict: a sandbox that is being deleted and is
  controlled by an earlier SpinApp with the same name (another UID), as left
  when a SpinApp is deleted and created again while KubeSwift still
  terminates its sandbox. The SpinApp then waits with `Progressing=True`
  (`SandboxCreating`), rechecking at least every 5 seconds, and creates its
  replica once the name is free. If the old sandbox is still there 2 minutes
  after its deletion started, `Progressing` reports `SandboxConflict` with
  the sandbox's finalizers, which usually means the KubeSwift controller is
  not running. Any other terminating object is a conflict at once.

While a SpinApp has unsupported configuration, an invalid executor or an
incompatible warm pool, kubeswift-spin creates and deletes nothing for it;
existing sandboxes keep running.

### Rolling updates

A rollout keeps `spec.replicas` replicas available
([ADR 0013](adr/0013-surge-replacement.md)):

1. It creates one surge replica, `<spinapp>-<replicas>` (for example
   `hello-1` for a single-replica SpinApp), at the new revision and with the
   label `spin.kubeswift.io/surge=true`. It serves Service traffic once it is
   ready.
2. It deletes an available outdated replica, highest ordinal first, only
   while the other available replicas, the surge replica included, still
   number `spec.replicas`. The deleted replica is recreated under its name
   once the old sandbox is gone, at the new revision.
3. It removes the surge replica once every ordinal below `spec.replicas`
   runs the new revision and is available.

If the spec changes again during a rollout, the surge replica of the
earlier revision is replaced too. While it is available and the replicas
below `spec.replicas` are not enough on their own, it is kept until they
are, so a second change never removes the last available replica. If
`spec.replicas` is raised during a rollout, the former surge replica
becomes an ordinary ordinal and is replaced once like an outdated replica,
so the `surge` label only ever marks the extra replica of a rollout.

A ready surge replica counts toward `readyReplicas`, capped at
`spec.replicas`, so `Available` stays `True` during a rollout. If the surge
replica cannot start or never becomes ready, the rollout stalls with every
old replica still serving, `Progressing` names the failing sandbox, and the
surge replica is replaced with the failure backoff above. A rollout needs
room for one extra sandbox: CPU and memory on a node, and room under any
ResourceQuota on SwiftSandboxes and on launcher pods. A SwiftSandbox create
that a quota or admission policy refuses (HTTP 403 or 422) is reported as
`SandboxRejected`; the SpinApp is requeued after 10 seconds, doubling per
consecutive refusal up to 5 minutes, and an earlier change to the SpinApp or
its sandboxes also retries; a launcher pod that a pod quota refuses shows up
as a KubeSwift waiting or failure reason on the sandbox instead. With a warm
pool, the surge replica takes a free slot or falls back to a cold boot like
any replica. To remove an outdated revision sooner, delete its
SwiftSandboxes with `--cascade=foreground`, including the surge replica
`<spinapp>-<replicas>` if its `spin.kubeswift.io/revision` label is not the
current one (`spec.replicas: 0` is not supported; see
[security-model.md](security-model.md#denial-of-service-and-capacity)).

KubeSwift owns what happens to a deleted replica; kubeswift-spin only
deletes it. On KubeSwift v0.16.3 or later, deleting a replica's
SwiftSandbox:

1. sets its readiness to false and takes it out of the SpinApp Service's
   ready endpoints, while the workload keeps running;
2. keeps the workload serving for the drain period
   (`spec.termination.drainPeriod`), meant to cover kube-proxy's next rule
   sync on every node (`minSyncPeriod` is 1 second by default) and the
   requests still running. kubeswift-spin leaves the field unset, so
   KubeSwift's default of 5 seconds for sandboxes with exposed ports
   applies, unless the executor sets `spin.kubeswift.io/drain-period`.
   Requests still running when the drain ends are cut: with the default, a
   request that takes longer than about 5 seconds can fail when its replica
   is replaced, scaled down or deleted (on KubeSwift v0.16.2 and earlier
   such a request had up to about 30 seconds). Set the annotation to the
   longest request the application must be able to finish, plus a few
   seconds; a longer drain also keeps each old replica, and the capacity it
   uses, for that long;
3. stops the microVM; the launcher pod exits and ends `Succeeded`;
4. removes the NetworkPolicy and the SwiftSandbox only after the launcher
   has stopped (KubeSwift lets the SwiftSandbox go anyway 30 seconds after
   the launcher's grace period, for example when its node is unreachable;
   the NetworkPolicy stays until the pod is gone).

In the KubeSwift v0.16.3 rolling-update runs
([compatibility.md](compatibility.md#zero-loss-rolling-updates)) the old
replica was marked not ready within 91 ms of its deletion, left the ready
endpoints within 17 ms of its launcher pod's deletion, and its launcher
exited 5.4 to 6.8 seconds after the deletion (the 5-second drain, then the
stop); its NetworkPolicy was removed 5 to 70 ms after the launcher exited,
and the SwiftSandbox was gone 5.6 to 7.1 seconds after the deletion. The
SwiftSandbox `Terminating` condition reports `Draining`, then `Stopping` (or
`LauncherGone` if the pod is already gone); KubeSwift computes the time of
`Stopping` from the pod's deletion timestamp, which has one-second
resolution, so the condition can change up to a second before the drain in
the launcher ends.

On KubeSwift v0.16.2 and earlier there is no explicit drain: the guest keeps
serving until the launcher's 30-second termination grace period ends, the
launcher pod ends `Failed`, and the NetworkPolicy is removed while the
replica can still serve (see
[compatibility.md](compatibility.md#known-upstream-issues)).

Measured on the lab cluster with continuous traffic through the SpinApp
Service (new connection per request): see
[compatibility.md](compatibility.md#zero-loss-rolling-updates).

## What triggers replacement

The revision label is a hash of the full sandbox spec. A change to any of
these replaces the replicas (one at a time, as in
[Rolling updates](#rolling-updates)):

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
- the first reconcile on KubeSwift v0.16.2 or later after upgrading
  KubeSwift from an earlier version: replicas whose readiness check
  gets `readinessStartup` (the TCP check kubeswift-spin adds, or every check
  with `readiness-startup: always`) are replaced once. The same holds for
  changing the `spin.kubeswift.io/readiness-startup` annotation
- setting, changing or removing the `spin.kubeswift.io/drain-period`
  annotation (only the executor's replicas are replaced)
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
release's `images.txt` and in the chart's `runtimeImage.digest` value), so
a pool must use that same reference, not the `spin-4.2.1-r2` tag. The
controller's reference is in its arguments:

```bash
kubectl -n kubeswift-spin-system get deployment kubeswift-spin -o jsonpath='{.spec.template.spec.containers[0].args}'
```

On KubeSwift v0.16.0 every sandbox exposes `http-app`, so the pool must
declare the same port, and the same egress allowlist and ingress peers as
the executor. For the released chart, with the digest from the
`--runtime-image` argument:

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata:
  name: spin-warm
spec:
  image: ghcr.io/kubeswift-io/kubeswift-spin-runtime@sha256:<digest>
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

With a chart installed from source without a digest, the reference is
`<runtimeImage.repository>:<runtimeImage.tag>` instead.
`config/samples/swiftsandboxpool.yaml` is a complete pool for the
`kubeswift-warm` executor; set its `image` the same way.

Whether the SwiftSandboxPool API exists is discovered at controller
startup; installing it later requires restarting the controller. When the
pool is compatible but has no free slot, KubeSwift's own cold fallback
applies and is recorded by KubeSwift as a `PoolColdFallback` Event on the
SwiftSandbox. The pool is a capacity
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
| Progressing | False | `SandboxConflict` | a sandbox name is taken by a foreign object, or by a deleted same-named SpinApp's sandbox still terminating after 2 minutes |
| Progressing | False | `SandboxRejected` | the API server refused to create a sandbox (Forbidden or Invalid), for example a ResourceQuota |
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
`SandboxCreated`, `SandboxDeleted`, `SandboxFailed`,
`UnsupportedConfiguration`, `PartiallySupported`, `ExecutorInvalid`,
`ExecutorNotFound`, `WarmPoolIncompatible`, `SandboxConflict`,
`SandboxRejected`, `ExecutorChanged`. Configuration Events fire once per
SpinApp generation or when the message changes.

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
