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

kubeswift-spin v0.2.0 is in development; this page describes what the
current revision does.

| Stage | State |
|---|---|
| Opt-in (chart, executor and SpinApp annotations) | implemented |
| Lookup identity and `SpinCompiledCache` records, consumers, garbage collection | implemented |
| SpinApp `CompiledCache` condition and Events | implemented |
| Compilation in a SwiftSandbox and KubeSwift output export | implemented |
| Signing and verification policy | implemented |
| Placement on warm-pool nodes and mounting in replicas | not yet |

Until placement and mounting land, records stop at `Trusted=True` with
`Placed` waiting, and replicas start without a cache.

## Requirements

- KubeSwift v0.19.1 or later. kubeswift-spin detects optional,
  platform-verified artifacts and sandbox outputs from the published schema
  (present from v0.19.0; v0.19.1 is the tested baseline); without them
  SpinApps report `CompiledCache=False/Unsupported`.
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

It then signs the digest with cosign (v3.1.3, the version KubeSwift
verifies with, built from source in the image) and the claims
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
`azurekms://` and `AZURE_*` variables: cosign v3.1.3 can send Azure
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

## What a SpinApp reports

The `CompiledCache` condition is separate from `Available` and
`Progressing`:

| Status | Reason | Meaning |
|---|---|---|
| False | `NotConfigured` | The executor enables caches, the platform does not |
| False | `NamespaceNotEnabled` | The namespace does not have the administrator's label |
| False | `Unsupported` | KubeSwift is too old, the executor selects an architecture other than amd64 or arm64, or an image reference is longer than 256 characters |
| False | `ImageNotPinned`, `RuntimeNotPinned` | The application or runtime image has no digest |
| False | `RuntimeNotAllowed` | The executor's runtime image is not one the platform builds caches for |
| False | `Requested` | Waiting for the cache manager to record the request |
| False | `Compiling`, `CompileFailed` | Compilation pending (including while waiting for a compiler slot), running, or failed (retried with backoff) |
| False | `Signing`, `SignFailed` | Waiting for, or refused by, the signer |
| True | `Placing`, `PlacementFailed` | New replicas mount the cache; warm-pool nodes are still being prepared, or preparing failed |
| True | `Ready` | New replicas mount the cache |

Events: `CompiledCacheRequested`, `CompiledCacheReady`,
`CompiledCacheFailed`, `CompiledCacheSkipped` and
`CompiledCachePlacementFailed`, once per change of reason.

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
