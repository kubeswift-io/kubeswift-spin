# 0014: Deliver a trusted compiled Wasmtime cache as a KubeSwift artifact

Status: proposed, for architecture review. Nothing here is implemented in a
release; the proof of concept lives on an unmerged branch.

## Context

On a warm start most of the time between workload dispatch and the first
response is Wasmtime compiling the application's components: about 400 ms
of the roughly 1.4 s for hello-http in the lab, more for larger
applications. Wasmtime can reuse compiled code through its cache directory,
and a pre-filled cache removed that time in earlier diagnostics. The cache
entries are native machine code: whoever writes them controls what the
guest executes, so they are trusted material and must never come from a
SpinApp author or from a tenant's own sandbox.

Facts this decision rests on (Spin 4.2.1, Wasmtime 49.0.1, KubeSwift
v0.16.4; sources and measurements in the PoC record below):

- Wasmtime's cache key is a SHA-256 over the exact component bytes, the
  full engine configuration (target, Cranelift flags, tunables, wasm
  features, Wasmtime version) and every CPU feature flag Cranelift detects
  at runtime. An entry made on a CPU with a different feature set is a
  clean miss, never wrong code. A loaded entry is checked again against
  the host (ISA features, engine metadata); a failed check is a silent miss
  and a recompile.
- Wasmtime keeps no checksum over the compiled code. Integrity of the
  entries has to come from how they are delivered.
- One entry per component, keyed by content: an application with N
  components has N entries, identical components share one, and changing
  one component adds exactly one entry.
- A read-only cache directory works: hits are served, Wasmtime's usage
  statistics and cleanup fail silently, and a miss compiles normally
  without storing. Spin points Wasmtime at a cache configuration file
  through `WASMTIME_CACHE_FILE`, whose `[cache] directory` can be any
  absolute path.
- Spin 4.2.1 cannot load precompiled components (`.cwasm`) with
  `spin up --from`; the cache directory is the only reuse mechanism.
- KubeSwift v0.16.2 and later mount `spec.artifacts` read-only in the
  guest at warm-pool checkout as well as at a cold start: digest-pinned,
  optionally cosign-verified, cached per node and bound to the repository
  that served it, mounted over virtio-fs before the workload is
  dispatched, without restarting the VM, and outside the slot shape, so
  pools stay generic.

## Decision

### Delivery

A compiled cache is an OCI artifact attached to a replica as a KubeSwift
artifact:

```yaml
artifacts:
  - name: wasmtime-cache
    ref: <cache repository>@sha256:<compiled artifact digest>
    layout: unpacked
    mountPath: /opt/kubeswift-spin-cache
    verifyKeySecretRef: {name: <platform verification key>}
env:
  - {name: WASMTIME_CACHE_FILE, value: /opt/kubeswift-spin-cache/wasmtime-cache.toml}
```

The artifact is mounted read-only and used in place, with no copy. The
runtime contract (`internal/runtimecontract`) gains the mount path and the
variable. No change to the runtime image or to KubeSwift is needed.

### Artifact format

One artifact per application digest, OCI image manifest with OCI media
types and one tar layer:

```
wasmtime-cache.toml   [cache] directory = "/opt/kubeswift-spin-cache/cache"
metadata.json         source digest, component digests, runtime image digest,
                      Spin and Wasmtime versions, target, CPU classes and their
                      feature sets, compiler image and node, compile time
cache/modules/wasmtime-49.0.1/<key>   one entry per component and CPU class
```

The usage statistics files are not shipped. Entries for several CPU
classes coexist in one directory because the CPU features are part of
Wasmtime's key; Wasmtime picks the matching one. The artifact records the
source as OCI annotations as well and, where the registry supports it, as
an OCI referrer (`subject`) of the application, so registry tools can show
the relation and garbage-collect with it.

### Lookup identity

kubeswift-spin finds an artifact by an external identity and leaves
validity to Wasmtime:

- the application digest (the SpinApp image resolved to a digest; the
  compiled entries are per component, so the digest stands for the set of
  component bytes);
- the runtime image digest (it fixes Spin, Wasmtime and the engine
  configuration, including the static build's unwind setting);
- the target architecture.

CPU classes are not part of the lookup: one artifact carries an entry per
class. Wasmtime's own key then decides, per component and per guest,
whether an entry is used. kubeswift-spin never computes Wasmtime's key.

### Compilation

A dedicated trusted compiler, run by the platform:

- One Kubernetes Job per application digest and CPU class, in the
  kubeswift-spin namespace, on a node of that class. It runs the runtime
  image's own `spin` binary on the application (pulled by digest), which
  compiles every component into an empty cache, and stops it before the
  application receives any request. A second container, which never
  handles the application, packages the entries, pushes the artifact and
  signs it. The compile container has no credentials, no service account
  token and no network access beyond the registry.
- Compilation is triggered when kubeswift-spin first sees a SpinApp
  revision with an application digest it has no artifact for.
- Before signing, the compiler checks the output: it starts `spin` once
  more on a writable copy of the packaged cache and fails if that start
  adds an entry (an added entry means a component missed the cache).
- A compiler in a container produces the same entries a guest on the same
  CPU class would (same key in the PoC), because the lab's Cloud Hypervisor
  guests see the host's CPU features. This must hold on each supported
  platform; where a hypervisor hides features, the compiler has to run in
  a guest of that class instead.

### Index

The compiler records each artifact in a namespaced `SpinCompiledCache`
object in the application's namespace (application digest, runtime image
digest, artifact reference by digest, CPU classes, state), owned by the
SpinApp so it is deleted with it. Only the compiler's service account may
write it; SpinApp authors have no RBAC on it. kubeswift-spin reads it when
it renders a replica. This is a platform index, not an application API: the
user-facing API stays the SpinApp, and nothing about it changes. It needs
its own CRD and RBAC review before implementation (CLAUDE.md requires an ADR
for any new CRD; this is that ADR).

### Trust chain

```
SpinApp image digest (tenant input, untrusted Wasm)
  -> trusted compiler Job (platform namespace, pinned runtime image digest)
  -> artifact digest, metadata binding source digest, runtime digest, CPU classes
  -> cosign signature by the platform key (optionally an attestation)
  -> SpinCompiledCache record written by the compiler only
  -> kubeswift-spin renders the artifact with the executor's verification key
  -> KubeSwift verifies the signature at the pinned digest before materializing
  -> node cache, sealed read-only, bound to the repository
  -> read-only virtio-fs mount in the guest
  -> Wasmtime key check (content, engine, CPU) on every load
```

- Signing: the platform's compiler, with a key held outside tenant reach
  (a KMS-backed or Kubernetes Secret key in the kubeswift-spin namespace).
  KubeSwift v0.16.4 verifies cosign public-key signatures without a
  transparency log; keyless verification needs a KubeSwift change.
- Verification: KubeSwift, before anything is mounted. kubeswift-spin only
  chooses which artifact to reference, and only from records it trusts.
- Cache poisoning: a SpinApp author cannot name an artifact (there is no
  user-facing setting), cannot write records, and cannot sign. A wrong but
  validly signed artifact is still only used where Wasmtime's key matches
  the exact component bytes, so another application's entries are never
  executed. Output of a tenant's own sandbox is never promoted to the
  cache.
- Tenant separation: compiled code reveals as much as the application it
  came from. Artifacts are stored per namespace (one repository per
  namespace, readable only with that namespace's pull credential), so one
  tenant cannot reference another tenant's compiled code even by digest:
  KubeSwift binds its node cache to the repository and resolves with the
  sandbox's own credentials.
- The guest cannot overwrite entries: the mount is read-only at several
  levels, and Wasmtime's own writes into it fail silently.

### Fallback

The cache is an optimization; without it a replica starts as today.

| Situation | Behavior |
|---|---|
| No record yet, compiler running or failed | No artifact; Spin compiles as today |
| Artifact cannot be resolved, fetched or verified | KubeSwift v0.16.4 fails the sandbox (`ArtifactResolveFailed`, `ArtifactMaterializeFailed`, `ArtifactMissing`). kubeswift-spin marks the record unusable and replaces the replica without the artifact. A KubeSwift `optional` artifact would remove the failed attempt (proposed upstream) |
| CPU class, Wasmtime version or engine mismatch | Silent miss, normal compile (measured) |
| Entry present but not used | Silent miss, normal compile |
| Corrupt entry | Prevented by digest pinning and signature. Wasmtime has no code checksum; a corrupt compressed entry is a miss, a corrupt metadata section is a start failure, which the fallback above then handles |

There is no strict mode: no operational need for refusing to start without
the cache has been shown.

### Rollout

For a change from application digest A to B:

1. Compilation of B starts when kubeswift-spin sees the new revision.
2. The rollout does not wait for it. The surge replica and the replacements
   start without the cache until the record for B is ready, then with it.
3. Old replicas keep serving under the existing surge rules (ADR 0013).
4. A failed compilation only means no cache.
5. B always starts without the cache.
6. The compiler deletes the record for A once no replica of the SpinApp
   runs digest A, after a grace period; every record is also deleted with
   its SpinApp (owner reference). A registry retention job removes
   artifacts that no record references.

The cache artifact is not part of the replica revision: its arrival does
not roll replicas, and removing it (fallback) does not either. Replicas
created later pick it up.

### Warm pools

Pools stay generic. A slot of any pool that runs the same runtime image can
take any application's cache at checkout. The first checkout of an artifact
on a node fetches it into the node cache (one pod, then a second dispatch);
later checkouts on that node mount it from the cache.

### Ownership

- KubeSwift: artifact resolution, verification, node cache, read-only
  mount at checkout. It knows nothing of Spin, Wasmtime or compiled code.
- kubeswift-spin: the compiler Jobs, the index, the lookup, the metadata,
  the fallback and the rollout behavior.
- Spin and Wasmtime: the cache format, the key and the compatibility
  checks.

## Alternatives considered

- **Lazy capture from the first replica.** Rejected: the cache would come
  from a tenant's own sandbox, where the application runs and could have
  altered it.
- **Per-node compilation on first use.** Every node pays the compile once
  per application, and the output still has to be trusted and shared;
  it gives up the main benefit.
- **CI-produced artifacts.** Possible only with the exact runtime image,
  engine configuration and CPU classes of the cluster, and with a signing
  identity the platform trusts; it moves platform concerns into every
  application's pipeline.
- **Admission-time precompilation, waiting before rollout.** Lower first-
  start latency for new revisions, at the cost of rollouts depending on
  the compiler. Kept as a later option (a bounded wait), not the default.
- **Baking the cache into the runtime image or a pool per application.**
  Rejected: one image or pool per application defeats generic warm pools.
- **Precompiled `.cwasm` loading.** Not supported by `spin up` in Spin
  4.2.1.
- **Copying the cache into a writable directory before Spin starts.** Not
  needed: the read-only mount works. A copy would cost 5 to 10 ms and would
  let the guest change the copy.
- **A portable baseline CPU build.** Code compiled for a baseline target
  never matches the key of an engine that detects the host's features, so
  it would never be a hit with Spin's engine.

## Proof of concept

Unmerged branch `poc/compiled-cache`: the KubeSwift `spec.artifacts` type
in `internal/sandboxapi`, and a SpinApp annotation that names an artifact
and its verification key. The annotation stands in for the index and is not
part of the design.

- Compiler: a pod on a lab node ran the runtime image's `spin` on hello-http
  v0.1.1 (by digest), packaged the single entry with the cache
  configuration and metadata (one tar layer, 213 KB), pushed it to the lab
  registry and signed it with a PoC key. The entry key matched the one a
  guest on the same node had produced earlier, so the guest and the
  container see the same CPU features.
- KubeSwift cosign-verified, materialized and mounted the artifact; the
  replica served normally on the cold and the warm path.
- 30 warm starts each on KubeSwift v0.16.4, one node, the shipped
  hello-http (application pulled from ghcr.io at every start), against the
  same build without the artifact:

| Seconds from SpinApp creation (p50 / p95) | v0.1.1 runtime | Compiled cache |
|---|---:|---:|
| checkout (claim) | 0.040 / 0.069 | 0.038 / 0.779 |
| claim to artifact projected | n/a | 0.001 |
| dispatch to first direct response | 1.444 / 1.652 | 1.039 / 1.226 |
| first direct response | 1.513 / 1.718 | 1.102 / 1.851 |
| `Available` | 3.140 / 3.184 | 3.132 / 3.543 |
| first Service response | 3.316 / 3.336 | 3.310 / 3.743 |

  The cache removed 405 ms (p50) between dispatch and the first response.
  Projecting a cached artifact at checkout cost about 1 ms. `Available` and
  the Service response did not move at p50 because the example's own
  readiness check (`initialDelaySeconds: 1`, `periodSeconds: 2`) gates
  them. Every fifth minute one checkout took about 0.75 s longer: KubeSwift
  caches its authorization check of a digest reference for 5 minutes and
  repeats it, serially, before claiming a slot.
- 10 warm starts each, same setup:

| Case | dispatch to first direct response (p50) | `Available` (p50) | Result |
|---|---:|---:|---|
| artifact with an entry compiled on another CPU class (AVX-512) | 1.480 s | 3.125 s | miss, normal compile, application served |
| artifact with entries for both CPU classes | 1.034 s | 2.151 s | hit on the lab's class |
| no readiness check, v0.1.1 runtime | 1.450 s | 1.571 s | |
| no readiness check, compiled cache | 1.035 s | 1.182 s | 389 ms earlier `Available` |

  The first checkout of each new artifact digest on the node took 5.4 to
  5.5 s instead of about 1.1 s: KubeSwift ran its fetch pod (pull and
  signature check) and dispatched again. Later checkouts on that node
  mounted it from the node cache. The first Service response stayed at
  about 2.2 s without a readiness check in both cases: kube-proxy's rule
  sync, not the start, sets it.

## Consequences

- No KubeSwift release is required for the core path (v0.16.2 or later
  with kernels/sandbox 6.6.15 or later on warm slots). KubeSwift changes
  make it cheaper and more robust: optional artifacts, placement on pool
  nodes, node cache garbage collection, keyless verification, and a
  concurrent or longer-lived authorization check for digest references
  ([kubeswift-sandbox-optional-artifacts.md](../upstream/kubeswift-sandbox-optional-artifacts.md)).
- kubeswift-spin gains a compiler component, a CRD and a signing key to
  operate, and a registry location per namespace for artifacts.
- The cache must be rebuilt for every runtime image (Spin version) and for
  every new CPU class in the cluster.

## Open questions

- Applications in private registries: the compiler must pull them, which
  needs a credential the platform may not have (kubeswift-spin reads no
  Secret). Options: a platform pull credential per executor, or compiling
  in the application's namespace with the SpinApp's own `imagePullSecrets`
  and a separate signer.
- A compile-only command in Spin (load every component, populate the
  cache, exit without serving) would replace starting `spin up` and
  stopping it, and would rule out executing application code during
  compilation for triggers other than HTTP.
- CPU class discovery on clusters with mixed hardware: node labels (for
  example from Node Feature Discovery) to place one compiler Job per class.
