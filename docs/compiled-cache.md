# Compiled Wasmtime cache

kubeswift-spin can give new replicas of a SpinApp a precompiled Wasmtime
cache, so Spin starts serving without compiling the application's
components first. The cache is native code, so it is compiled in an
isolated SwiftSandbox, signed by a separate signer, and mounted read-only
only after KubeSwift verified the signature under a platform policy. The
design is [ADR 0014](adr/0014-compiled-wasmtime-cache.md).

The cache is an optimization. A SpinApp that cannot get one starts and
becomes `Available` exactly as without the feature.

## Status of this revision

Compiled caches are new in kubeswift-spin v0.2.0. From v0.2.1 they need
KubeSwift v0.19.3 or later (see [Requirements](#requirements)).

| Stage | State |
|---|---|
| Opt-in (chart, executor and SpinApp annotations) | implemented |
| Lookup identity and `SpinCompiledCache` records, consumers, garbage collection | implemented |
| SpinApp `CompiledCache` condition and Events | implemented |
| Compilation in a SwiftSandbox and KubeSwift output export | implemented |
| Signing and verification policy | implemented |
| Placement on warm-pool nodes and mounting in replicas | implemented |

The kubeswift-spin v0.3.0 release candidate was validated on the amd64 KVM
lab with KubeSwift v0.20.0, and the v0.3.0 signer (cosign v3.1.4) before
the release ([compatibility](compatibility.md#validation-of-v030-rc1)).
The v0.2.1 release candidate was validated with KubeSwift v0.19.3, with the cache repository and the
applications in a registry behind a private CA ([compatibility](compatibility.md#validation-of-v021-rc1),
[test/e2e](../test/e2e/README.md#compiled-caches),
[performance](performance.md#compiled-wasmtime-cache-kubeswift-v0193-2026-10-24));
v0.2.0 on KubeSwift v0.19.1.
arm64 follows the same path but has not been run on arm64 hardware: do
not rely on compiled caches there until it has.

## Requirements

- KubeSwift v0.19.3 or later. kubeswift-spin relies on its
  optional-artifact contract: a cache KubeSwift cannot deliver is skipped
  (`Skipped`), and the replica starts and becomes ready without it. v0.19.2
  fixed a warm replica that never became ready after its cache was skipped
  at the checkout (kubeswift-io/kubeswift#843) and a sandbox that failed
  when its cache pull Secret could not be used (#844); v0.19.3 uses a cache
  the node already holds for a replica that boots cold (#851). kubeswift-spin
  v0.2.0 worked around the first two; v0.2.1 does not.
  kubeswift-spin detects optional, platform-verified artifacts and sandbox
  outputs from the published schema (present from v0.19.0); without them
  SpinApps report `CompiledCache=False/Unsupported`. KubeSwift publishes no
  version kubeswift-spin can read, and v0.19.0 to v0.19.3 have the same
  schema, so it cannot tell v0.19.3 from an older release: upgrade KubeSwift
  first (see [Upgrading](#upgrading)). On v0.19.1 a warm replica whose
  cache is skipped after the checkout never becomes ready.
  Running SpinApps without compiled caches needs no particular KubeSwift
  v0.19 release: see [compatibility](compatibility.md#kubeswift-features).
- The SwiftKernel `sandbox` in each enabled namespace on
  `kernels/sandbox:6.6.17` or later. Compilers export their cache as a
  KubeSwift output, which needs that kernel; on an older one KubeSwift
  fails the compiler sandbox with `KernelUnsupported` and no cache is
  built.
- A runtime image pinned by digest (`runtimeImage.digest`). An executor
  that names another runtime image with `spin.kubeswift.io/runtime-image`
  gets a cache only if that digest is listed in
  `compiledCache.extraRuntimes` (`RuntimeNotAllowed` otherwise): the
  platform signs only what its own runtime images compiled.
- SpinApps whose `spec.image` is pinned by digest. A tagged image gets no
  cache (`ImageNotPinned`): the controller has no registry credentials to
  resolve a tag, and a tag can move.

## Prerequisites

Each namespace that uses compiled caches must meet these, because
kubeswift-spin cannot check them:

- No principal other than the platform can exec, attach, port-forward or
  add ephemeral containers to pods in the namespace, or create, update or
  patch pods there. The built-in `edit` and `admin` roles grant these. Such
  a principal can reach KubeSwift's fetch pods and, through them, the node's
  shared artifact cache, and replace verified cache entries of any namespace
  on that node (see
  [kubeswift-artifact-cache-tenant-isolation.md](upstream/kubeswift-artifact-cache-tenant-isolation.md)).
- No principal other than the platform can write `swiftsandboxes/status`
  in the namespace.
- No principal other than the platform can create, update, patch or delete
  ConfigMaps or SwiftKernels in the namespace. A compiler's launcher reads
  its command, arguments and environment from a ConfigMap KubeSwift writes
  next to it (immutable, but one that is deleted and recreated before the
  launcher starts is not detected), and runs the kernel its
  `kernelProfileRef` names. Either would change what the compiler runs
  while its spec, which the signer checks, stays the same.
- The namespace's cache repository (`<repository>/<namespace>/**`) is
  readable only with that namespace's pull credential, and the registry
  denies anonymous reads: compiled code reveals as much as the application.
- The Secret named by `compiledCache.pullSecretName` exists in the
  namespace as a `kubernetes.io/dockerconfigjson` Secret before the label
  is set. Without it placements fail and replicas start without the cache
  (see [Fallback](#fallback)).

An administrator records that a namespace meets them with the label
`spin.kubeswift.io/compiled-cache=enabled`. Only labelled namespaces get
caches (`NamespaceNotEnabled` otherwise), and the export and verification
policies select the same label. Use a label that tenants cannot set: the
built-in namespace `admin` role cannot change Namespace labels.

## Turning it on

The platform enables the feature in the chart:

```yaml
runtimeImage:
  digest: sha256:...          # required: caches are keyed by the runtime digest
compiledCache:
  enabled: true
  repository: registry.example.com/kubeswift-spin-cache
  architecture: amd64         # for executors without a kubernetes.io/arch node selector
  extraRuntimes: []           # other runtime images caches may be built for, by digest
  retention: 24h
  maxRecordsPerNamespace: 32
  pullSecretName: cache-read  # in every enabled namespace; reads only its cache repository
  fetchTimeout: 0s            # cold replicas get no cache (the default); see Replicas
```

This installs the `SpinCompiledCache` CRD and the cache manager, a second
Deployment from the controller image (`--mode=cache-manager`) with its own
ServiceAccount. The controller gains read access to records in the release
namespace, and to Namespaces for the label, and nothing else.

An administrator enables each namespace:

```bash
kubectl label namespace team-a spin.kubeswift.io/compiled-cache=enabled
```

An executor opts in:

```yaml
metadata:
  annotations:
    spin.kubeswift.io/compiled-cache: enabled
```

A SpinApp on that executor can opt out with
`spin.kubeswift.io/compiled-cache: disabled`. A SpinApp cannot opt in on an
executor that does not enable caches.

## Compilation

For each record the cache manager compiles the application in a compiler
SwiftSandbox in the record's namespace:

- The runtime image is the record's (by digest), started in compile mode
  ([runtime-image.md](runtime-image.md#compile-mode)). It pulls the
  application with the SpinApp's `imagePullSecrets`, so it can pull exactly
  what the SpinApp can. It gets the SpinApp's literal variables and no
  Secret-backed value.
- It runs where the SpinApp's replicas run (the executor's node selector,
  kernel profile, rootfs mode, network mode and egress rules, plus
  `kubernetes.io/arch`), so the entries match their CPU class. It exposes no
  port and carries no launcher labels.
- It is bounded: `compiledCache.compiler.cpu`, `memory`, `timeout`,
  `maxSize` and `maxEntries`, and at most `maxConcurrent` compilers run at
  once, `maxConcurrentPerNamespace` per namespace. KubeSwift boots it cold:
  sandboxes with outputs do not use warm slots.
- When compile mode exits 0, KubeSwift exports its output directory as an
  OCI artifact to `<repository>/<namespace>/wasmtime-cache`, pushed by a
  KubeSwift Job with the platform's credential; the guest never sees it. The
  record's `status.compile.artifact` names the exported digest.
- A failure (compile mode exits non-zero, the export is refused, the
  sandbox is deleted or exceeds its time) is recorded with its reason, a
  Warning Event on the record and the `CompiledCache=False/CompileFailed`
  condition on the SpinApp, and retried after 1 minute, doubling to at most
  6 hours. The SpinApp itself is never affected.

The compiler sandbox stays until the signer has checked it, then the cache
manager deletes it; it also carries a one-hour TTL. Compilers count against
the namespace's ResourceQuota (one SwiftSandbox, `compiler.cpu` and
`compiler.memory` each, `maxConcurrentPerNamespace` at a time): leave that
much headroom, or a replica created during a rollout can be refused while a
compiler runs.

### Export policy

KubeSwift exports only to repositories an administrator allows, with a
credential in the KubeSwift namespace:

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftArtifactExportPolicy
metadata:
  name: kubeswift-spin-compiled-cache
spec:
  namespaceSelector:
    matchLabels:
      spin.kubeswift.io/compiled-cache: enabled
  repositories:
    - registry.example.com/kubeswift-spin-cache/{namespace}/wasmtime-cache
  defaultRepository: registry.example.com/kubeswift-spin-cache/{namespace}/wasmtime-cache
  credentialsSecretRef:
    name: kubeswift-spin-cache-push
  maxSize: 512Mi
```

The Secret `kubeswift-spin-cache-push` is a `kubernetes.io/dockerconfigjson`
Secret in the KubeSwift namespace with the label
`kubeswift.io/export-credentials=true`; give it push access to the cache
repositories only. `{namespace}` gives each namespace its own repository,
so no namespace can export into another's. Without a policy the compiler's
output is refused (`OutputDestinationDenied`).

## Signing

The cache signer is a third Deployment (`kubeswift-spin-cache-signer`
image) with its own ServiceAccount. It is the only holder of the signing
key and of the credential that writes signatures. It never runs tenant
code and has no Secret RBAC: the key and the registry credential are
volumes of its pod.

For each record whose compiler exported an artifact, it signs only after
these checks, made independently of the cache manager:

1. The compiler sandbox the record names exists, has the recorded UID and
   is not being deleted.
2. Its whole stored spec hashes to the hash recorded at creation, and every
   field is one a compiler has: no artifacts, model, scratch disk, GPU,
   probes, pool or pod metadata, only literal Spin variables and the
   registry credential list in its environment, only registry credentials
   as secret files, no port, the record's architecture, an allowed runtime
   in compile mode for the record's source and repository.
3. It completed, and the export tag `<sandbox UID>.wasmtime-cache`, which
   only KubeSwift's export Job pushes, names the record's digest.
4. The artifact is the manifest KubeSwift's export produces, with one gzip
   tar layer of only `metadata.json` and cache entries (regular files and
   directories, within `compiler.maxSize` and `maxEntries`), and its
   metadata names the record's source, runtime and architecture.

It then signs the digest with cosign (v3.1.4, built from source in the
image; KubeSwift v0.20.0 verifies with v3.1.3, which accepts its signatures
in the compiled-cache e2e) and the claims
`spin.kubeswift.io/kind=wasmtime-cache`, `namespace`, `source`, `runtime`,
`architecture` and `compiler`, inside the signed payload, and records the
signed digest and key ID in `status.signature`. An artifact that fails a
check is marked rejected and never signed; the cache manager compiles again
after its backoff. A decision names the compiler it judged.

### Signing key and registry credential

The key is a cosign key in a Secret in the release namespace, or a KMS key
(preferred where available):

```bash
cosign generate-key-pair k8s://kubeswift-spin-system/kubeswift-spin-cache-signing-key
```

This creates the Secret with `cosign.key`, `cosign.pub` and
`cosign.password`. Nothing but the signer mounts it. For a KMS key, set
`compiledCache.signer.key.kmsRef` (for example `gcpkms://...`), give the
signer's ServiceAccount access with `signer.serviceAccountAnnotations` or
`signer.extraEnv`, and leave `secretName` empty. The chart refuses
`azurekms://` and `AZURE_*` variables: cosign v3.1.4 can send Azure
credentials to registries that are not Azure (GO-2026-6225), and no fixed
release exists yet.

The signer needs a `kubernetes.io/dockerconfigjson` Secret in the release
namespace that can read the cache repositories and push signatures to them:

```yaml
compiledCache:
  signer:
    key:
      secretName: kubeswift-spin-cache-signing-key
    registryCredentialsSecret: kubeswift-spin-cache-signer-registry
```

#### A cache registry behind a private CA

KubeSwift trusts a private CA for sandbox registries through its chart
value `sandboxMaterialize.registryCABundle` (KubeSwift v0.19.1 and later):
digest resolution, artifact and placement pulls, output exports and
cosign verification. Give the signer the same PEM bundle:

```yaml
compiledCache:
  signer:
    registryCABundle: |
      -----BEGIN CERTIFICATE-----
      ...
      -----END CERTIFICATE-----
```

The chart puts it in an immutable ConfigMap named after its content and
mounts it into the signer as a file in `SSL_CERT_DIR`, so the signer and
cosign trust it on top of the system roots for all their TLS connections,
a KMS included; a new bundle rolls the signer. Whoever can create
ConfigMaps in the release namespace and change the signer's Deployment
controls its trust anchors, like its key: keep both to the chart's
operators. Without it, signing fails for such a registry
(`SignFailed`). TLS verification is never turned off. The controller and
the cache manager do not contact registries. Replicas and compilers run
the runtime image, whose Spin pulls the application itself: an application
in a registry behind a private CA needs a runtime image that trusts that
CA, as without compiled caches.

### Verification policy

Replicas mount a cache only through KubeSwift, which refuses it unless a
`SwiftArtifactVerificationPolicy` governs its repository and the signature
by a policy key carries every claim the replica asks for. kubeswift-spin
requires the namespace, source, runtime and architecture claims on each
replica itself; the policy must name the signer's key and bind the
namespace:

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftArtifactVerificationPolicy
metadata:
  name: kubeswift-spin-compiled-cache
spec:
  namespaceSelector:
    matchLabels:
      spin.kubeswift.io/compiled-cache: enabled
  repositories:
    - registry.example.com/kubeswift-spin-cache/**
  otherRepositories: Allow
  digestPinning: Required
  authorities:
    - name: kubeswift-spin-cache-signer
      key:
        publicKey: |
          -----BEGIN PUBLIC KEY-----
          ...the signer's cosign.pub...
          -----END PUBLIC KEY-----
  claims:
    - key: spin.kubeswift.io/kind
      value: wasmtime-cache
    - key: spin.kubeswift.io/namespace
      fromNamespace: true
```

Get the public key with `cosign public-key --key k8s://kubeswift-spin-system/kubeswift-spin-cache-signing-key`
(or `--key <kmsRef>`). `kubectl get savp` shows the policy `Ready` with the
key's ID, which matches the record's `status.signature.keyID`. The
`fromNamespace` claim keeps a namespace from mounting another namespace's
cache even with read access to its repository.

To rotate the key, add the new key to the policy as a second authority,
switch the signer to it, and remove the old key once the caches it signed
are no longer used.

## Delivery

### Placement on warm pools

Once a record is signed (`Trusted=True`), the cache manager creates one
`SwiftArtifactPlacement` per warm pool its current consumers check out
from, in the consumer namespace, named `<record>-<hash of the pool>`.
KubeSwift fetches and verifies the artifact on every node of the pool
ahead of time. The placement names exactly what a replica mounts: the
same reference, `layout: unpacked`, pull Secret, claims and
`requirePlatformVerification`, so a checkout finds the prepared entry.

The record's `status.placements` copies each placement's `Prepared`
condition and node counts. `Placed` is True once every pool reports every
node prepared, and `Ready` is `Trusted` and `Placed`. A placement whose
pool no consumer uses any more, or whose record is no longer trusted or
is deleted, is deleted. A placement is the cache manager's when it has the
record's labels and the record's UID in the annotation
`spin.kubeswift.io/compiled-cache-uid`; any other object that holds a
placement's name is never changed, and the record reports
`PlacementConflict`. A placement the API server refuses (a quota, an
admission policy) is reported as `PlacementRejected` and does not hold back
the other pools. Both are looked at again every 2 minutes.

### Replicas

Replicas created while the record is trusted get one more entry in
`spec.artifacts`:

```yaml
- name: wasmtime-cache
  ref: registry.example.com/kubeswift-spin-cache/team-a/wasmtime-cache@sha256:...
  mountPath: /opt/kubeswift-spin-cache
  layout: unpacked
  optional: true
  fetchTimeout: 0s            # with a warm pool; compiledCache.fetchTimeout without one
  pullSecretRef: {name: cache-read}
  requirePlatformVerification: true
  claims:
    spin.kubeswift.io/kind: wasmtime-cache
    spin.kubeswift.io/namespace: team-a
    spin.kubeswift.io/source: ghcr.io/org/app@sha256:...
    spin.kubeswift.io/runtime: ghcr.io/kubeswift-io/kubeswift-spin-runtime@sha256:...
    spin.kubeswift.io/architecture: amd64
```

They also get `KUBESWIFT_SPIN_CACHE_SOURCE`, `KUBESWIFT_SPIN_CACHE_RUNTIME`
and `KUBESWIFT_SPIN_CACHE_ARCHITECTURE`, the identity the cache must have.
A replica of an executor with a warm pool gets the cache once the pool's
placement reports every node prepared (lookup result `unplaced` until
then), with `fetchTimeout: 0s`: it uses the cache only if its node already
holds it, and never waits. The placement gate is an optimization, not a
correctness rule. A replica that gets the cache on a node without it, or
whose cache KubeSwift cannot use for any other reason, starts without it:
KubeSwift skips the optional artifact (`status.artifacts[].state:
Skipped`, with the reason), Spin compiles as usual, and the replica becomes
ready. The gate only avoids handing replicas a cache their node cannot
serve yet. A SpinApp can be `Available` while its cache is `Placing`,
`PlacementFailed` or skipped.

The same holds for a replica of such an executor that boots cold because
no warm slot is free. With KubeSwift v0.19.3 it mounts the cache when its
node already holds a verified entry and KubeSwift's authorization of the
digest is fresh (`Mounted`, reason `Cached`), without asking the registry;
otherwise it starts without the cache at once.

A SpinApp whose executor has no sandbox pool reports `NoWarmPool` while
`compiledCache.fetchTimeout` is `0s`, and no cache is built for it: no
placement prepares its nodes, so its replicas would almost never find the
cache there.

With `compiledCache.fetchTimeout` above `0s`, replicas of an executor
without a warm pool get the cache and wait at most that long for it.
KubeSwift verifies the signature with the registry and, on a node without
the entry, fetches it, all within `fetchTimeout` counted from the sandbox's
creation. On the KVM lab a first fetch did not finish within 3 seconds,
and with 20 seconds the cache was mounted; for small applications the
verification costs more than the compilation it saves.

The artifact and the variables are not part of the replica's revision: a
cache that becomes available, or goes away, never replaces running
replicas.

### Runtime mount contract

The runtime entrypoint decides, before it starts Spin, and logs the
decision. It uses the cache only when:

- `/opt/kubeswift-spin-cache` is a mount point and the mount is read-only
  (`/proc/self/mountinfo`);
- `metadata.json` there is compiled cache metadata naming exactly the
  expected source, runtime and architecture, the architecture the
  entrypoint was built for, and the Wasmtime cache version of the image's
  own Spin;
- the entry directory `cache/modules/<version>` exists and is not a link.

It then writes `[cache] directory = "/opt/kubeswift-spin-cache/cache"` to
`/var/lib/kubeswift-spin/wasmtime-cache.toml` and sets
`WASMTIME_CACHE_FILE` for Spin. `WASMTIME_CACHE_FILE` and
`DISABLE_WASMTIME_CACHE` are reserved: values from anywhere else are
dropped, and the log names the dropped variable. In every other case Spin
starts with its normal cache and compiles as before.

### Fallback

The cache never stops a replica:

| Situation | Result |
|---|---|
| No record, compiling, failed, signing, rejected | No artifact; Spin compiles |
| The node does not hold the cache (`NotOnNode`, `FetchTimeout`); the pull Secret cannot be used (`PullSecretInvalid`); the registry refuses or is unavailable; verification, claims or a policy fail | KubeSwift skips the optional artifact (`status.artifacts` says why); Spin compiles; the replica becomes ready as without the feature |
| Mount missing, writable, or with other metadata | The entrypoint starts Spin without it |
| The pool's placement is not prepared on every node | Warm replicas get no cache (lookup `unplaced`) |
| KubeSwift fails a replica because of the cache: its artifact init container failed (for example a sandbox-materialize image of another KubeSwift version, which refuses `--authorized-until`), or its kernel cannot mount artifacts (`KernelUnsupported`) | The replica is replaced as any failed replica; the SpinApp's new replicas start without the cache for up to 30 minutes (`DeliveryFailed`), since each would fail the same way. The hold is kept in memory: a controller restart ends it early |
| CPU features or Wasmtime version not in the cache | Wasmtime misses and compiles |

### Metrics

| Metric | Labels |
|---|---|
| `kubeswift_spin_compiled_cache_lookups_total` | `result`: `hit` (a signed cache was attached), `miss` (none yet), `unplaced` (a warm replica whose pool is not prepared yet), `cold` (a replica that boots cold, without a cold fetch timeout), `skipped` (the SpinApp cannot have one) |
| `kubeswift_spin_compiled_cache_attachments_total` | `result`: `mounted`, `skipped`; `reason`: KubeSwift's (`Cached`, `Fetched`, `FetchTimeout`, `PolicyDenied`, ...) |
| `kubeswift_spin_compiled_cache_compilations_total` | `result`, `reason` |
| `kubeswift_spin_compiled_cache_signatures_total` | `result` |

Lookups are counted when a replica is created. Attachments are counted
once per replica, by the controller that is leader when KubeSwift reports
the outcome; replicas created before it started reconciling are not
counted.

### Settings that must agree

The controller and the cache manager build the replica artifact and the
placement from the same settings (`compiledCache.pullSecretName`,
`compiledCache.fetchTimeout`, rendered for both by the chart). A warm
checkout uses a prepared entry only when both name the same pull Secret,
layout and claims: do not override these flags in only one of them with
`extraArgs`.

### Upgrading

From kubeswift-spin v0.2.0 with compiled caches enabled:

1. Upgrade KubeSwift to v0.19.3 or later. Its controller and its
   sandbox-materialize image must come from the same release: the v0.19.3
   controller passes sandbox-materialize a flag (`--authorized-until`)
   that an older one refuses, which fails every replica that carries the
   cache. The KubeSwift chart sets both; check only if you pin
   `sandboxMaterialize.image.tag` separately. kubeswift-spin v0.2.0 keeps
   working on v0.19.3.
2. A warm replica that KubeSwift v0.19.1 left not ready before the
   upgrade stays not ready (its readiness verdict was lost; see KubeSwift's
   v0.19.2 upgrade notes). kubeswift-spin v0.2.0 replaces such replicas
   within a minute or two; v0.2.1 does not. Before upgrading
   kubeswift-spin, check that none is left: a SwiftSandbox of a SpinApp
   with `spec.poolRef`, `status.artifacts` showing `wasmtime-cache`
   `Skipped`, and `WorkloadReady=False` with reason `Probing`. Delete any
   you find; the SpinApp replaces it.

   ```bash
   kubectl get swiftsandboxes -A -o jsonpath='{range .items[?(@.spec.poolRef)]}{.metadata.namespace}{"/"}{.metadata.name}{" "}{.status.artifacts[?(@.name=="wasmtime-cache")].state}{" "}{.status.conditions[?(@.type=="WorkloadReady")].reason}{"\n"}{end}'
   ```

3. Then upgrade kubeswift-spin to v0.2.1 with `--reset-then-reuse-values`,
   as the [README](../README.md#upgrade) describes.

kubeswift-spin cannot tell which KubeSwift release runs (v0.19.0 to
v0.19.3 publish the same schema, and KubeSwift publishes no version), so it
logs at startup that compiled caches assume v0.19.3.

Do not run kubeswift-spin v0.2.1 with compiled caches on KubeSwift v0.19.1
or v0.19.2: v0.2.1 no longer replaces a warm replica that KubeSwift v0.19.1
never reports ready after skipping its cache, and v0.19.2 skips a cache
that a replica booting cold could have used. With compiled caches
disabled, v0.2.1 has the same KubeSwift requirements as v0.2.0.

### Turning it off or revoking a cache

The cache is outside the revision, so neither removing a record nor
disabling the feature replaces running replicas that mount a cache. To
stop using one:

1. Delete the records (the cache manager deletes their placements), or
   remove the namespace label to stop new records there.
2. Restart the affected SpinApps' replicas, for example by scaling to zero
   and back, so new replicas start without the cache.
3. To disable the feature, set `compiledCache.enabled=false` only after
   step 1: without the cache manager, nothing deletes placements, and
   `kubectl delete swiftartifactplacements -A -l spin.kubeswift.io/component=placement`
   removes what is left.

## What a SpinApp reports

The `CompiledCache` condition is separate from `Available` and
`Progressing`, which follow the replicas alone: a SpinApp can be
`Available` with any `CompiledCache` reason.

| Status | Reason | Meaning |
|---|---|---|
| False | `NotConfigured` | The executor enables caches, the platform does not |
| False | `NoWarmPool` | The executor has no sandbox pool and the platform gives replicas that boot cold no cache (`compiledCache.fetchTimeout` is `0s`) |
| False | `NamespaceNotEnabled` | The namespace does not have the administrator's label |
| False | `Unsupported` | KubeSwift is too old, the executor selects an architecture other than amd64 or arm64, or an image reference is longer than 256 characters |
| False | `ImageNotPinned`, `RuntimeNotPinned` | The application or runtime image has no digest |
| False | `RuntimeNotAllowed` | The executor's runtime image is not one the platform builds caches for |
| False | `Requested` | Waiting for the cache manager to record the request |
| False | `Compiling`, `CompileFailed` | Compilation pending (including while waiting for a compiler slot), running, or failed (retried with backoff) |
| False | `Signing`, `SignFailed` | Waiting for, or refused by, the signer |
| True | `Placing`, `PlacementFailed` | The cache is trusted; new warm-pool replicas mount it once their pool's nodes are prepared, which is still in progress or failed |
| False | `DeliveryFailed` | KubeSwift failed a replica because of the cache (see [Fallback](#fallback)); new replicas start without it for up to 30 minutes. A replica whose cache was skipped is not a failure and never causes it |
| True | `Ready` | New warm-pool replicas get the cache (and replicas of an executor without a pool, when `compiledCache.fetchTimeout` allows) |

Events: `CompiledCacheRequested`, `CompiledCacheReady`,
`CompiledCacheFailed` (also for `DeliveryFailed`), `CompiledCacheSkipped`
and `CompiledCachePlacementFailed`, once per change of reason.

## Records

```console
$ kubectl -n kubeswift-spin-system get spincompiledcaches
NAME                                     FOR      ARCH    READY   REASON    AGE
spincc-0b6f0f1c2d3e4f5a6b7c8d9e0f1a2b3c   team-a   amd64   False   Pending   2m
```

One record exists per namespace, application digest, runtime digest,
architecture and platform repository; SpinApps that share them share the
record. The record lists its consumers and their warm pools. It is deleted
once no SpinApp has used it for the retention period, except the most
recently replaced record of each SpinApp, which is kept so a rollback finds
its cache. A namespace has at most `maxRecordsPerNamespace` records, and
SpinApps scaled to zero get none. Users never create or edit records.

An invalid `spin.kubeswift.io/compiled-cache` value is a configuration
error like any other kubeswift-spin annotation: `ExecutorInvalid` on an
executor, `UnsupportedConfiguration` on a SpinApp.
