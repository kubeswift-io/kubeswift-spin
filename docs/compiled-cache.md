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
| Compilation in a SwiftSandbox and KubeSwift output export | not yet |
| Signing and verification policy | not yet |
| Placement on warm-pool nodes and mounting in replicas | not yet |

Until the remaining stages land, records report `Compiled=False/Pending`
and replicas start without a cache.

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
| False | `Compiling`, `CompileFailed` | Compilation pending, running, or failed (retried with backoff) |
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
