# 0014: Trusted compiled Wasmtime cache delivered as a KubeSwift artifact

Status: accepted; implemented in kubeswift-spin v0.2.0-rc1. See
[compiled-cache.md](../compiled-cache.md) for the operator documentation.

## Context

On a warm start most of the time between workload dispatch and the first
response is Wasmtime compiling the application's components: about 400 ms
of roughly 1.4 s for hello-http in the lab, more for larger applications.
Wasmtime can reuse compiled code from its cache directory. Cache entries
are native machine code: whoever writes them controls what the guest
executes, so they are trusted material and must never come from a SpinApp
author or from a sandbox that served traffic.

Facts this decision rests on (Spin 4.2.1, Wasmtime 49.0.1, KubeSwift
v0.19.1):

- Wasmtime's cache key is a SHA-256 over the component bytes, the engine
  configuration and every CPU feature Cranelift detects at run time. A
  mismatching entry is a silent miss and a normal compile. The key is only
  a file name chosen by whoever writes the cache; Wasmtime keeps no checksum
  over the code. **Integrity comes only from who may produce, sign and
  reference an artifact.**
- One entry per component, keyed by content. `spin up` compiles every
  component of the selected triggers before it opens its HTTP listener, so
  a listener that accepts a TCP connection proves the entries exist. No
  request is needed.
- A read-only cache directory works: hits are served, statistics and
  cleanup writes fail silently, misses compile normally.
- Spin reads `WASMTIME_CACHE_FILE`; a named file that does not exist stops
  `spin up`. `spin up` refuses an application with a required variable that
  has no value, or a key-value or SQLite label without runtime
  configuration.
- KubeSwift v0.19.1 provides, without knowing anything about Spin:
  - optional artifacts (`optional`, `fetchTimeout`): a sandbox starts
    without an artifact it cannot get in time;
  - `SwiftArtifactVerificationPolicy`: cluster-scoped, tenant-proof cosign
    verification with signed claims (`value`, `values`, `fromNamespace`,
    `fromArtifact`), and per-artifact `claims` and
    `requirePlatformVerification` that only add requirements;
  - `SwiftArtifactPlacement`: prepares artifacts on the nodes of a warm pool
    before checkout, verified under the same policies;
  - background authorization renewal and a bounded node artifact cache;
  - `spec.outputs` with `SwiftArtifactExportPolicy`: a sandbox exports a
    directory as an OCI artifact, pushed with platform credentials the
    guest never sees;
  - warm-slot checkout artifact attachment and guest-agent-aware pools.
- A SwiftSandbox's spec is immutable except `spec.ttl` (admission webhook)
  until the sandbox is being deleted, when the webhook stops validating.
  KubeSwift tags an exported artifact `<sandbox UID>.<output name>` in the
  destination repository; only its export Job pushes it.
- KubeSwift's node artifact cache (`/var/lib/kubeswift/sandbox-artifacts`)
  is written by fetch and prepare pods that run in the sandbox's namespace
  with a read-write hostPath, and the launcher is privileged. A warm
  checkout trusts a cache entry once its verification record exists,
  without hashing the content again. KubeSwift documents principals who can
  exec into or modify these pods as trusted with the node.

## Decision

### Components and ownership

| Component | Identity | Writes |
|---|---|---|
| SpinApp controller (existing) | its own ServiceAccount | SpinApp status, replica SwiftSandboxes (adds the cache artifact to new replicas) |
| Cache manager (`manager --mode=cache-manager`, same image) | its own ServiceAccount | `SpinCompiledCache` records (spec, compile and placement status, conditions), compiler SwiftSandboxes, SwiftArtifactPlacements |
| Cache signer (own image with cosign) | its own ServiceAccount, the signing key | `status.signature` of records, signatures in the cache registry |
| KubeSwift | | export, verification, placement, mount |

The controller only reads records. Nothing writes SpinApps or executors.
KubeSwift knows nothing of Spin or Wasmtime.

### Opt-in

- Off by default, with three layers:
  1. The platform enables the feature (chart `compiledCache.enabled`, which
     installs the CRD, the cache manager and the signer and requires a
     digest-pinned runtime image).
  2. An administrator enables each namespace with the label
     `spin.kubeswift.io/compiled-cache=enabled`. The platform's export and
     verification policies select the same label, and setting it asserts the
     namespace meets the prerequisites below.
  3. An executor opts in with `spin.kubeswift.io/compiled-cache: enabled`; a
     SpinApp can opt out with the same annotation set to `disabled`. A
     SpinApp cannot opt in on its own.
- An invalid value of either annotation is reported like any other invalid
  kubeswift-spin annotation (`ExecutorInvalid`, `UnsupportedConfiguration`).
  This is the one way the feature can stop a SpinApp, and it happens only on
  a configuration error.
- Two modes only: disabled, and enabled (opportunistic). There is no strict
  mode: the cache never gates a replica.
- A cluster whose KubeSwift lacks optional platform-verified artifacts and
  outputs (detected from the published schema, ADR 0012) gets no cache;
  the SpinApp reports `CompiledCache=False/Unsupported`.

### Identity

`(namespace, SpinApp image digest reference, runtime image digest
reference, architecture)`, plus the export repository derived from the
platform setting, so changing the platform repository makes new records.

- Both images must be pinned by digest (`repository@sha256:...`, any tag
  dropped). The controller has no registry credentials and never resolves
  tags; a tagged SpinApp gets `ImageNotPinned`. Both references must be valid
  KubeSwift claim values (at most 256 characters).
- The runtime image must be on the platform's list (the chart's
  `runtimeImage.digest` plus `compiledCache.extraRuntimes`). An executor can
  name any runtime image, so without the list the signer would vouch for
  code from an image the executor author chose (`RuntimeNotAllowed`).
- The architecture is the executor's `kubernetes.io/arch` node selector,
  else the platform default (`amd64`). CPU feature classes are not part of
  the identity: Wasmtime's own key chooses among the entries of one
  artifact.
- The record name is a hash of the identity, so every SpinApp, replica,
  reconcile and restart converges on one record and one compilation.

### Index: SpinCompiledCache

A namespaced CRD (`spin.kubeswift.io/v1alpha1`) in the kubeswift-spin
namespace, never in a tenant namespace. `spec` is the identity and the
export repository, required and immutable. `status` has `compile` (attempt,
token, compiler sandbox name, UID and spec hash, exported artifact,
failures, backoff), `signature` (signed artifact, key ID, rejection,
failures), `placements`, `consumers` (SpinApps using it, and the time each
moved away), `pools` and the conditions `Compiled`, `Trusted`, `Placed` and
`Ready`.

Status has three writers, each owning whole fields: the cache manager's
namespace reconciler (`consumers`, `pools`, `lastReferencedAt`), its record
reconciler (`compile`, `placements`, conditions) and the signer
(`signature`). Each sends its fields in a JSON merge patch that carries the
resourceVersion it read, and sends the cleared fields of an object as
`null`, so no writer overwrites another and a reset (a cleared rejection, a
zeroed failure count) takes effect.

A namespace holds at most `compiledCache.maxRecordsPerNamespace` records
(default 32): records live outside the tenant's quotas. SpinApps scaled to
zero get none.

Why a CRD (ADR 0008 requires a reason): the index must be writable only by
platform components and readable by the controller. SpinApp and executor
annotations are tenant- or executor-author-writable, ConfigMaps and
Secrets are outside the controller's RBAC by design, and Helm values are
static. The user API stays SpinApp; no user creates or edits a record.

Even so, a record is not the trust anchor. A replica mounts an artifact
only through KubeSwift, which verifies its signature and claims under the
platform policy. A forged record can at worst point at an artifact the
platform signed for the same identity.

### Compilation

- When a record has no artifact and is not in backoff, the cache manager
  creates a compiler SwiftSandbox (generated name) in the record's
  namespace. It runs the
  record's runtime image (by digest) in compile mode, with the SpinApp's
  `imagePullSecrets` (so it can pull exactly what the SpinApp can, and
  nothing more), the executor's network mode, egress rules, node selector
  (plus `kubernetes.io/arch`), kernel and rootfs settings, bounded CPU,
  memory and `spec.timeout`, no exposed ports and no launcher pod labels
  (no Service selects it), and one output.
- Compile mode (runtime entrypoint) pulls the application, reads its
  locked application, supplies placeholder values for required variables
  without one and a scratch runtime configuration for every key-value
  and SQLite label, runs `spin up` for the components of HTTP triggers on a
  loopback listener as the unprivileged user, waits for the listener to
  accept a TCP connection, stops Spin, copies only well-formed cache
  entries (regular files named by Wasmtime's key) and writes
  `metadata.json`. It never sends a request. Literal SpinApp variables are
  passed as for a replica; Secret-backed ones are not, so the compiler
  holds the SpinApp's pull credentials, as a replica does, and no other
  tenant Secret.
- KubeSwift exports the output directory when the workload exits 0. The
  push credential is in the KubeSwift namespace; the compiler guest has no
  registry push credential and no signing key.
- Deduplication: the record name is deterministic; before creating a
  compiler the cache manager claims the attempt (a fresh token and attempt
  number, written with the record's resourceVersion), creates the sandbox
  with labels naming the record and the token, and records the sandbox's
  name, UID and spec hash from the create response. A compiler it finds
  without a recorded UID (a restart in between) is never adopted: it is
  deleted, by UID precondition, and a new attempt starts. The cache manager
  deletes only sandboxes that carry its labels for that record; a tenant who
  copies the labels onto a sandbox of their own can at most have it deleted
  in their own namespace, and it deletes only sandboxes that also run compile
  mode with an allowed runtime, so labels alone never make it delete a
  replica. Compilers also carry a one-hour TTL, so a compiler orphaned by a
  deleted record goes away by itself; the record reconciler also deletes the
  compilers of a record it finds gone. A failure counter is reset only once a
  cache is signed, so an artifact the signer keeps refusing backs off.
- Concurrent compilers are bounded cluster-wide and per namespace, so one
  namespace cannot hold KubeSwift's export capacity.
- A failed attempt (non-zero exit, timeout, export failure) records the
  reason, a Warning Event and a metric, deletes the compiler, and waits 1
  minute, doubling to 6 hours, before the next attempt. The SpinApp is
  never affected.

### Signing

- The signer is a separate Deployment with its own ServiceAccount, the only
  holder of the signing key (a KMS reference where available, otherwise a
  Secret in the kubeswift-spin namespace) and of the registry credential
  that writes signatures. It never runs tenant code.
- For a record whose compile attempt exported an artifact, it checks,
  independently of the cache manager: the compiler sandbox exists with the
  recorded UID and is not being deleted (its spec is then frozen); its whole
  stored spec, every field included, hashes to the hash the cache manager
  recorded at creation; it sets no field a compiler never has (artifacts,
  model, scratch disk, GPU, probes, pool, pod metadata), no environment
  variable other than literal Spin variables and the registry credential
  list, no secret file other than registry credentials and no port; it runs
  an allowed runtime image in compile mode for the record's source with the
  record's output repository and architecture; and it completed. It does not trust the sandbox's labels or its
  `status.outputs` digest: it resolves the tag `<sandbox UID>.wasmtime-cache`
  in the repository, which only KubeSwift's export Job pushes, and requires
  that digest to be the record's. It
  then pulls the artifact by that digest itself and checks the manifest
  (one gzip tar layer), every entry (only `metadata.json` and
  `cache/modules/wasmtime-<version>/<key>`, regular files and directories,
  bounded sizes and counts) and the metadata (it must name the record's
  source, runtime and architecture). OCI annotations are never read.
- It signs that digest with cosign, with the claims
  `spin.kubeswift.io/kind=wasmtime-cache`, `namespace`, `source`,
  `runtime`, `architecture` and `compiler` (the signer's version) inside the
  signed payload, then records the signed digest and key ID. An artifact
  that fails a check is marked rejected and never signed; the cache manager
  compiles again after its backoff. A decision names the compiler it judged,
  so a later compiler that exports the same bytes is judged again.

### Verification policy

The administrator creates a `SwiftArtifactVerificationPolicy` that selects
the opted-in namespaces (labels only administrators set), governs the
compiled-cache repository, requires digest pinning, trusts the signer's
public key, and requires `spin.kubeswift.io/kind=wasmtime-cache` and
`spin.kubeswift.io/namespace` from the namespace. An example ships with
the chart documentation. Independently of what the policy lists, every
replica's artifact sets `requirePlatformVerification: true` and the claims
`kind`, `namespace`, `source`, `runtime` and `architecture`, so KubeSwift
refuses an artifact that no policy governs, or whose signature was made for
another namespace, application, runtime or architecture.

### Delivery

- Once a record is Trusted, the cache manager keeps one
  `SwiftArtifactPlacement` per warm pool its consumers use, in the
  consumer namespace, with the same reference, layout and claims a replica
  uses. `Placed` is True when every pool reports every node prepared;
  `Ready` is Trusted and Placed.
- New replicas of a SpinApp whose record is Trusted get one artifact:
  `name: wasmtime-cache`, the signed `repository@sha256` reference,
  `layout: unpacked`, `mountPath: /opt/kubeswift-spin-cache`,
  `optional: true`, the platform's pull Secret name if configured, the
  claims above and `requirePlatformVerification`. `fetchTimeout` is `0s`
  for replicas checked out of a warm pool (the placement prepares the
  nodes, so a replica uses the cache only if its node already holds it and
  never waits). A warm replica gets the artifact only once its pool's
  placement reports every node prepared: KubeSwift v0.19.1 never accepts
  the readiness of a warm replica whose optional artifact it skipped after
  the checkout (kubeswift-io/kubeswift#843). Cold replicas get the cache
  only when the platform sets a cold fetch timeout (default none):
  KubeSwift verifies the signature with the registry on every cold start
  and fetches the cache on a new node within that timeout, which on the
  lab took longer than 3 seconds and, for small applications, costs more
  than the compilation it saves.
  The controller also sets the runtime contract variables that name the
  expected source, runtime and architecture. SpinApps on an executor
  without a sandbox pool, while the cold fetch timeout is none, get no
  record (`NoWarmPool`).
- Two KubeSwift v0.19.1 defects can still cost a replica: a warm replica
  whose node lost its entry while the placement reports it prepared
  (kubeswift-io/kubeswift#843), and a cache pull Secret KubeSwift cannot
  read, which fails the sandbox although the artifact is optional
  (kubeswift-io/kubeswift#844). The controller replaces a replica that
  matches the first defect exactly (checked out, cache skipped after the
  claim, no readiness verdict accepted for a minute, or for its readiness
  check's initial delay, two periods and 15 seconds if longer), one per
  reconcile, and after either defect starts the SpinApp's new replicas
  without the cache for 30 minutes (`DeliveryFailed`). The hold is kept
  in memory; a controller restart ends it early.
- The decoration is applied per replica outside the revision fingerprint:
  a cache becoming available, or going away, never rolls replicas.
- The runtime entrypoint owns activation. It uses the cache only when the
  contract path is a read-only mount point, its `metadata.json` names the
  expected source, runtime and architecture, and its entries are in the
  Wasmtime version directory of its own Spin. It then writes the Wasmtime
  cache configuration in its own writable directory and exports
  `WASMTIME_CACHE_FILE`. In every other case Spin starts with its normal
  cache. `WASMTIME_CACHE_FILE` and `DISABLE_WASMTIME_CACHE` are reserved:
  the entrypoint ignores values it did not set.

### Fallback

| Situation | Behavior |
|---|---|
| Feature off, executor not opted in, SpinApp opted out | Unchanged replicas, no condition |
| No record yet, compiling, failed, signing, rejected | No artifact; Spin compiles; condition says why |
| Tagged image or runtime, unsupported architecture or KubeSwift | No record; condition says why |
| Node not prepared and the fetch does not finish within `fetchTimeout`; registry refuses; verification or claims fail | KubeSwift skips the optional artifact; Spin compiles; `status.artifacts` and the controller's metric say why |
| Mount missing, writable or with other metadata | The entrypoint starts Spin without it |
| CPU class or Wasmtime version not in the artifact | Wasmtime misses silently and compiles |

`Available` never depends on the cache.

### Rollout

For a change from application digest A to B, the existing replicas of A
keep serving under ADR 0013; B's rollout starts at once; B's record is
created and compiled in the background; replicas of B created after B's
record is Trusted mount it. A failed compile of B only leaves B without a
cache.

### Garbage collection

The cache manager keeps a record while a SpinApp uses it as its current
cache, and the most recently replaced record of each existing SpinApp (its
rollback target). Other records are deleted after a retention period
(default 24 hours) from their last use, with their placements. Registry
artifacts are not deleted by kubeswift-spin in v0.2.0; a registry retention
job removes unreferenced digests. KubeSwift's node cache is bounded by
KubeSwift.

### CPU classes and architectures

A compiler runs on a node selected like the SpinApp's replicas, so its
entries match that CPU class. Mixed fleets get entries only for the
classes a compiler ran on; other classes miss and compile. arm64 follows
the same path but is not validated until it runs on arm64 hardware.

### Status contract

This ADR extends the SpinApp status contract: besides `activeScheduler`,
`readyReplicas`, `Available` and `Progressing`, kubeswift-spin writes the
condition `CompiledCache` for SpinApps that ask for a cache, and removes it
when they no longer do. It never affects `Available` or `Progressing`.

### Prerequisites

Setting the namespace label asserts, for that namespace:

- No principal other than the platform can exec, attach, port-forward or
  add ephemeral containers to pods, or create, update or patch pods. The
  built-in `edit` and `admin` roles grant these. Such a principal can reach
  KubeSwift's fetch pods and through them the node's shared artifact cache,
  and replace verified cache entries of any namespace on that node with
  native code.
- No principal other than the platform can write `swiftsandboxes/status`.
- No principal other than the platform can create, update, patch or delete
  ConfigMaps or SwiftKernels. The launcher runs what the sandbox's intent
  ConfigMap and kernel say, not what its spec says, and the signer checks
  only the spec.
- Cache artifacts are readable only with that namespace's own pull
  credential, scoped to `<repository>/<namespace>/**`, and the registry
  denies anonymous reads. Compiled code reveals as much as the application.

kubeswift-spin cannot verify these; the documentation states them, and the
KubeSwift requirements in `docs/upstream/` ask for a node cache that does
not depend on them.

### Version detection

The schema check finds the v0.19.0 fields; v0.19.1 is the tested and
required baseline.

## Alternatives considered

- **Compiling in a container next to the push and signing credentials**
  (the earlier proof of concept). Rejected: a Wasm runtime escape during
  compilation would reach credentials valid for every tenant, outside a
  microVM.
- **Compiling in the kubeswift-spin namespace.** Rejected: the compiler
  would need pull credentials beyond the tenant's, and a tenant could have
  the platform compile, sign and place another tenant's private
  application in its own namespace.
- **Lazy capture from a serving replica.** Rejected: the cache would come
  from a sandbox the application ran in and could have altered.
- **Per-node compilation on first use**, **CI-produced caches**, **baking
  caches into the runtime image or a pool per application**: as in the
  first revision of this ADR; each loses either trust or generic pools.
- **Waiting for the cache before a rollout, or a strict mode.** Not in
  v0.2.0: the cache is an optimization.
- **A writable copy of the cache.** Not needed: read-only works.

## Consequences

- kubeswift-spin v0.2.0 gains a CRD, a cache manager mode, a signer image, a
  compile mode in the runtime image, new chart values and RBAC, and
  administrator setup (export policy and credential, verification policy,
  signer key, cache repository, namespace labels).
- The controller's RBAC grows only by read access to records.
- Correctness depends on KubeSwift v0.19.1 behavior: optional artifacts,
  policy verification and claims, placement, output export and spec
  immutability. kubeswift-spin uses them only when the schema shows them.
- Residual risks, documented in docs/security-model.md: a tenant's own
  application can influence the native code compiled for that tenant's
  namespace (as a Cranelift bug would let any Wasm module do); principals
  who can exec into KubeSwift pods or modify its node cache are trusted
  with the node, as KubeSwift documents, which the prerequisites exclude
  for enabled namespaces; claims bind a namespace by name, so a namespace
  deleted and re-created by another principal inherits its predecessor's
  signed caches until they are deleted from the registry.

The first revision of this ADR, with the proof-of-concept measurements on
KubeSwift v0.16.4 (405 ms saved between dispatch and the first response
at p50 for hello-http), was reviewed in kubeswift-io/kubeswift-spin#27.
