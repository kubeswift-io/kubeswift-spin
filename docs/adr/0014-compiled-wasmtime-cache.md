# 0014: Deliver a trusted compiled Wasmtime cache as a KubeSwift artifact

Status: proposed, revised after security and architecture review. Nothing
here is implemented in a release; the proof of concept lives on an
unmerged branch and exercises only the delivery path.

## Context

On a warm start most of the time between workload dispatch and the first
response is Wasmtime compiling the application's components: about 400 ms
of the roughly 1.4 s for hello-http in the lab, more for larger
applications. Wasmtime can reuse compiled code through its cache directory.
The cache entries are native machine code: whoever writes them controls
what the guest executes, so they are trusted material and must never come
from a SpinApp author or from a tenant's sandbox that served traffic.

Facts this decision rests on (Spin 4.2.1, Wasmtime 49.0.1, KubeSwift
v0.16.4; measurements in the proof-of-concept section):

- Wasmtime's cache key is a SHA-256 over the component bytes, the engine
  configuration (target, Cranelift flags, tunables, wasm features, Wasmtime
  version) and every CPU feature flag Cranelift detects at runtime. A
  mismatching entry is a clean miss, and a loaded entry is checked again
  against the host; a failed check is a silent miss and a recompile.
- The key is only a file name chosen by whoever writes the cache. Wasmtime
  does not check an entry's content against its key and keeps no checksum
  over the code. All inputs to the key are public or derivable, so anyone
  who can place an entry can target any component. **Integrity comes only
  from who may produce, sign and reference an artifact**, never from the
  key.
- One entry per component, keyed by content; identical components share an
  entry, and changing one component adds one entry.
- A read-only cache directory works: hits are served, statistics and
  cleanup writes fail silently, misses compile normally without storing.
- Spin reads `WASMTIME_CACHE_FILE`. If it names a file that does not exist,
  `spin up` exits ("failed to read config file"). Spin 4.2.1 cannot load
  precompiled components with `spin up --from`; the cache directory is the
  only reuse mechanism.
- `spin up` refuses to start an application with a required variable that
  has no value, or with a key-value store label that has no runtime
  configuration.
- KubeSwift v0.16.2 and later mount `spec.artifacts` read-only at warm-pool
  checkout as well as at a cold start: digest-pinned, optionally
  cosign-verified (public key only, no transparency log), cached per node
  and bound to the repository that served it, mounted over virtio-fs before
  dispatch, without restarting the VM, outside the slot shape. A warm slot
  can take artifacts only with kernels/sandbox 6.6.15 or newer; otherwise
  KubeSwift boots the sandbox cold. A registry that refuses the reference
  fails the sandbox; an unreachable registry keeps it `Pending` and retries
  without limit.

## Decision

### Enablement and gating

- Off by default. An executor opts in (annotation
  `spin.kubeswift.io/compiled-cache: enabled`, chart value
  `executors[].compiledCache`), together with the repository prefix, the
  per-namespace pull credential and the verification settings below.
- Used only when KubeSwift serves warm-checkout artifacts (detected through
  the v0.16.2 schema, as in ADR 0012) and only on executors whose operator
  guarantees warm-mount-capable slots; KubeSwift does not yet advertise that
  per pool (requested upstream). Otherwise the executor is
  `ExecutorInvalid`, never a silent cold boot.
- Used only when both the SpinApp image and the runtime image are pinned by
  digest. The controller has no registry access and cannot resolve tags;
  tagged images get no cache, and the executor documentation says so.

### Delivery

The replica gets a KubeSwift artifact and a runtime contract variable:

```yaml
artifacts:
  - name: wasmtime-cache
    ref: <namespace's cache repository>@sha256:<artifact digest>
    layout: unpacked
    mountPath: /opt/kubeswift-spin-cache
    pullSecretRef: {name: <platform pull credential for this namespace's repository>}
    verifyKeySecretRef: {name: <verification key>}
env:
  - {name: KUBESWIFT_SPIN_COMPILED_CACHE, value: /opt/kubeswift-spin-cache}
```

- The runtime entrypoint owns the Wasmtime configuration. It writes
  `[cache] directory = "<mount>/cache"` under its own base directory and
  exports `WASMTIME_CACHE_FILE` only if the contract path is a read-only
  mount containing `cache/`. Otherwise it starts Spin without it.
  `WASMTIME_CACHE_FILE` and `DISABLE_WASMTIME_CACHE` become reserved
  names: the entrypoint drops any other value, and translation never emits
  them. This needs a runtime image release, which also makes the mount
  path a runtime contract rather than something stored in signed
  artifacts.
- The cache decoration is applied per replica outside the revision
  fingerprint: the Template keeps the revision-bearing spec, and
  `NewSandbox` adds the artifact and the variable when a record applies.
  The arrival or removal of a cache never rolls replicas. Golden tests
  prove the revision is identical with and without it, and the documented
  meaning of the revision label is updated.

### Artifact format

One OCI image manifest per application digest (OCI media types), one tar
layer:

```
metadata.json                          source and runtime digests, versions,
                                       CPU classes and feature sets, compiler identity
cache/modules/wasmtime-49.0.1/<key>    one entry per component, CPU class and architecture
```

Only regular files whose names match the key pattern, mode 0444, no links;
the packager fails otherwise. No statistics files. Entries for several CPU
classes and architectures coexist because both are part of Wasmtime's key;
neither is part of the lookup. Metadata is written by a JSON encoder, never
by templating values into a shell.

### Lookup identity

`(namespace, SpinApp image digest reference, runtime image digest
reference)`. kubeswift-spin never computes Wasmtime's key; Wasmtime decides
per component and per guest whether an entry is used.

### Compilation

A separate component, the compiler, with its own Deployment, ServiceAccount
and namespace. The main controller gains no Job, Pod or Secret access.

1. The compiler watches SpinApps (read-only) on opted-in executors and
   deduplicates by lookup identity, with per-namespace concurrency limits,
   resource limits, a deadline and a maximum component and artifact size.
2. It compiles inside a SwiftSandbox of the target CPU class: the same
   isolation as a production replica, no credentials, no service account
   token, egress only to the application's registry host. The runtime image
   gains a compile mode: it pulls the application by digest, supplies
   placeholder values for required variables and an in-memory runtime
   configuration for every store label (none of which is part of Wasmtime's
   key), loads every component of HTTP triggers, never sends a request, and
   exits when the cache is populated. Other trigger types wait for a
   compile-only command in Spin.
3. The output leaves the sandbox through a generic KubeSwift capability
   that does not exist yet: exporting a directory as an OCI artifact when
   the workload exits, pushed by KubeSwift with platform credentials outside
   the guest (requested upstream).
4. A separate signer, which never handles tenant input, checks the
   exported artifact (file names, modes, sizes, metadata), pushes the final
   artifact to the namespace's repository, and signs the digest it computed
   itself, never a re-resolved tag. The signature carries the claim
   (namespace, application digest, runtime digest, compiler identity) as an
   attestation. The key is KMS-backed with audit logging; rotation re-signs
   or recompiles.
5. The signer writes the record (below). It is the only writer.

Rationale: a compiler exploited by its input runs in a microVM, holds no
credentials, and can only influence the cache of that tenant's own
application, which the tenant could already exploit in its own guest. The
damage stays inside one namespace as long as artifacts, repositories,
claims and records are namespace-scoped. A second, independent compile
whose output must be byte-identical is an optional hardening against a
compromised compiler sandbox that is not driven by the input.

### Index

`SpinCompiledCache` objects in the compiler's namespace, keyed by namespace
and lookup identity (deterministic names within 63 characters), with
`spec` (identity, artifact reference by digest, CPU classes) written by the
signer and `status` (usable, last failure) that the main controller may
patch. The main controller gets get, list and watch on them and patch on
their status only; it discovers the CRD at startup as an optional API.

Why a CRD: the index must be writable only by the platform and readable by
the controller. SpinApp and executor annotations are writable by tenants
and executor authors, Helm values are static, and ConfigMaps and Secrets
are outside the controller's RBAC by design. The user-facing API stays the
SpinApp. Two SpinApps with the same image share one record; garbage
collection belongs to the compiler (it keeps the record for each SpinApp's
current image and one previous image), not to owner references.

### Trust chain

```
SpinApp image digest (tenant input)
  -> compiler sandbox (microVM, no credentials, pinned runtime digest)
  -> exported artifact (KubeSwift, platform credentials outside the guest)
  -> signer: checks, pushes to the namespace's repository, signs the computed
     digest with a claim (namespace, application, runtime)
  -> record in the platform namespace (signer only)
  -> controller renders the artifact by digest with the namespace's pull
     credential and the verification key
  -> KubeSwift verifies before materializing; node cache bound to the repository
  -> read-only virtio-fs mount; entrypoint checks the mount is read-only
  -> Wasmtime uses an entry only where its key matches
```

- A SpinApp author has no setting that names an artifact and cannot write
  records, sign, or push to cache repositories.
- Confidentiality: compiled code reveals as much as its application.
  Each namespace's artifacts live in their own repository, readable only
  with that namespace's platform-provisioned, read-only pull credential;
  the controller always renders `pullSecretRef` and never falls back to
  the runtime image credential.
- Verification today: KubeSwift reads `verifyKeySecretRef` from the
  sandbox's namespace, where whoever can write Secrets can replace it. Until
  KubeSwift supports a verification policy tenants cannot modify, with
  claim checks (requested upstream), integrity rests on the record's RBAC,
  the digest pin and the per-namespace repository; the signature serves as
  provenance and protects against registry tampering.
- A failed verification never leads to a retry without verification; it
  produces a Warning Event and a metric.
- The guest cannot change entries: the mount is read-only on the host and
  in the guest, and the entrypoint refuses a writable cache path.

### Fallback

The cache is an optimization; without it a replica starts as without the
feature.

| Situation | Behavior |
|---|---|
| No record, compiler running or failed, tagged image | No artifact; Spin compiles |
| Registry refuses the reference, fetch or verification fails (`ArtifactResolveFailed`, `ArtifactMaterializeFailed`, `ArtifactMissing`, `InvalidArtifacts`) on a replica that carries the cache | Classified as a cache failure: no backoff, no `SandboxFailed` condition, a Warning Event and a metric. The controller marks the record unusable in its status (one retry after a controller restart) and replaces the replica without the cache |
| Registry unreachable (`RegistryUnavailable`, `Resolving`) longer than a bounded wait | The replica is replaced without the cache, with the same classification |
| Mount absent or not read-only | Entrypoint starts Spin without the cache |
| CPU class, Wasmtime version or engine mismatch, entry unused | Silent miss, normal compile (measured) |
| Corrupt entry | Prevented by the digest pin; a corrupt metadata section would be a start failure, handled as a cache failure |

There is no strict mode.

### Rollout

For a change from application digest A to B:

1. The compiler sees the new image and compiles B.
2. The rollout does not wait. The first replicas of B start without the
   cache; replicas created after the record is ready get it.
3. Old replicas keep serving under ADR 0013; nothing about the surge
   changes, because the cache is outside the revision.
4. A failed compilation only means no cache.
5. The compiler keeps the records for each SpinApp's current and previous
   image and deletes the rest; a registry retention job removes
   unreferenced artifacts.

### Warm pools

Pools stay generic. Any slot running the same runtime image can take any
application's cache at checkout. The first checkout of an artifact on a
node fetches it (one pod, then a second dispatch), which costs more than
the cache saves. The feature therefore stays off by default until
KubeSwift can place artifacts on pool nodes before first use and checks
authorization off the checkout path (requested upstream).

### Observability

Events on the SpinApp when a replica gets a cache and when it falls back
(with the reason), metrics for hits attached, fallbacks and compiler
outcomes, and the record's status. No new SpinApp condition.

### Ownership

- KubeSwift: artifact resolution, verification, node cache, read-only
  mount at checkout, and the requested output export. It knows nothing of
  Spin, Wasmtime or compiled code.
- kubeswift-spin: the compiler and signer components, the index, the
  lookup, the metadata, the fallback, the rollout behavior, the runtime
  entrypoint's cache handling.
- Spin and Wasmtime: the cache format, the key and the compatibility checks.

## Alternatives considered

- **Compiling in a container next to the push and signing credentials** (the
  proof of concept's compiler). Rejected: a Wasm runtime escape during
  compilation would reach credentials valid for every tenant, outside a
  microVM.
- **Lazy capture from a serving replica.** Rejected: the cache would come
  from a sandbox the application ran in and could have altered.
- **Per-node compilation on first use.** Every node pays the compile per
  application and the output still has to be trusted.
- **CI-produced artifacts.** Only with the cluster's exact runtime image,
  engine configuration and CPU classes and a signing identity the platform
  trusts.
- **Waiting for compilation before a rollout.** A later option with a
  bounded wait, not the default.
- **Baking the cache into the runtime image, or a pool per application.**
  Defeats generic warm pools.
- **Precompiled `.cwasm` loading.** Not supported by `spin up` in Spin
  4.2.1.
- **A writable copy of the cache.** Not needed (read-only works), costs 5 to
  10 ms, and lets the guest change the copy.
- **A portable baseline CPU build.** Never matches the key of an engine that
  detects the host's features.

## Proof of concept

Unmerged branch `poc/compiled-cache`: the KubeSwift `spec.artifacts` type
in `internal/sandboxapi` and a SpinApp annotation naming an artifact and a
key. The annotation is a stand-in for the index and must not ship. The
proof of concept puts the artifact into the revision and lets the artifact
supply the cache configuration; both differ from the decision above. Its
compiler was a container with credentials in the same pod, which the
security review rejected; it measured the delivery path only.

- A pod on a lab node ran the runtime image's `spin` on hello-http v0.1.1
  (by digest), packaged the single entry (one tar layer, 213 KB), pushed it
  and signed it with a throwaway key. The entry key matched the one a guest
  on the same node had produced earlier: the lab's guests see the host's
  CPU features. This must be checked per platform; the decision compiles
  inside a sandbox anyway.
- KubeSwift cosign-verified, materialized and mounted the artifact; the
  replica served on the cold and the warm path.
- 30 warm starts each on KubeSwift v0.16.4, one node, the shipped
  hello-http (pulled from ghcr.io at every start):

| Seconds from SpinApp creation (p50 / p95) | v0.1.1 runtime | Compiled cache |
|---|---:|---:|
| checkout (claim) | 0.040 / 0.069 | 0.038 / 0.779 |
| claim to artifact projected | n/a | 0.001 |
| dispatch to first direct response | 1.444 / 1.652 | 1.039 / 1.226 |
| first direct response | 1.513 / 1.718 | 1.102 / 1.851 |
| `Available` | 3.140 / 3.184 | 3.132 / 3.543 |
| first Service response | 3.316 / 3.336 | 3.310 / 3.743 |

  The cache removed 405 ms (p50) between dispatch and the first response;
  projecting a cached artifact cost about 1 ms. `Available` did not move at
  p50 because the example's readiness check (`initialDelaySeconds: 1`,
  `periodSeconds: 2`) gates it. Every fifth minute one checkout took about
  0.75 s longer: KubeSwift repeats its authorization check of a digest
  reference every 5 minutes, serially, before claiming a slot.
- 10 warm starts each:

| Case | dispatch to first direct response (p50) | `Available` (p50) | Result |
|---|---:|---:|---|
| entry compiled on another CPU class (AVX-512) | 1.480 s | 3.125 s | miss, normal compile, served |
| entries for both CPU classes | 1.034 s | 2.151 s | hit on the lab's class |
| no readiness check, v0.1.1 runtime | 1.450 s | 1.571 s | |
| no readiness check, compiled cache | 1.035 s | 1.182 s | 389 ms earlier `Available` |

  The first checkout of each new artifact digest on the node took 5.4 to
  5.5 s instead of about 1.1 s (fetch pod, pull, signature check, second
  dispatch). The first Service response stayed at about 2.2 s without a
  readiness check: kube-proxy's rule sync, not the start, sets it.

## Consequences

- kubeswift-spin v0.2.0 at the earliest: a compiler and a signer
  component, a CRD, a runtime image with a compile mode and an entrypoint
  that owns the cache configuration, new executor settings, per-namespace
  repositories and pull credentials to provision.
- KubeSwift work is required before this is production-worthy: output
  export (for an isolated compiler), a verification policy tenants cannot
  modify, optional artifacts, placement on pool nodes, authorization off the
  checkout path, node cache garbage collection, and per-pool advertisement
  of warm-mount support
  ([kubeswift-sandbox-optional-artifacts.md](../upstream/kubeswift-sandbox-optional-artifacts.md)).
- Acceptance criteria before the feature can default to on: the
  first-checkout fetch no longer on the request path, no periodic
  authorization delay at checkout, and a measured saving at p95 as well as
  p50.
- The cache must be rebuilt for every runtime image and every new CPU
  class.

## Open questions

- Applications in private registries: the compiler sandbox must pull them.
  A platform pull credential per executor is the simplest; compiling with
  the SpinApp's own `imagePullSecrets` inside the tenant's namespace keeps
  credentials where they are but must never let a key valid beyond that
  namespace sign the output.
- A compile-only command in Spin would replace the runtime's compile mode
  and cover non-HTTP triggers.
- CPU class discovery on mixed hardware: node labels (for example from Node
  Feature Discovery) to run one compile per class.
