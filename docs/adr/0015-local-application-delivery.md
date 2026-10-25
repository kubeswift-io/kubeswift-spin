# 0015: Run warm replicas from a KubeSwift-mounted copy of the application artifact

Status: accepted for implementation in kubeswift-spin v0.3.0
([#56](https://github.com/kubeswift-io/kubeswift-spin/issues/56)) after two
rounds of architecture and security review. Not implemented. It amends [ADR 0004](0004-runtime-rootfs-and-application-artifact.md),
[executor-contract.md](../executor-contract.md) and
[kubeswift-sandbox-artifact-projection.md](../upstream/kubeswift-sandbox-artifact-projection.md),
which say the application is always pulled in the guest, and extends the
status contract of [ADR 0014](0014-compiled-wasmtime-cache.md).

## Context

Every replica runs `spin up --from=<SpinApp.spec.image>`. Spin contacts the
registry on every start, even with a full cache: it resolves the registry
host, authenticates and fetches the manifest before it runs anything
(`crates/oci/src/client.rs`, `Client::pull`, unchanged in v4.2.5 and on
`main`). A fresh sandbox has neither a resolver cache nor a Spin cache.

Measured for [#49](https://github.com/kubeswift-io/kubeswift-spin/issues/49)
(KVM lab, kubeswift-spin v0.2.1, KubeSwift v0.20.0, Spin 4.2.1,
hello-http, 30 warm starts per case, guest monotonic clock):

- Registry next to the cluster: acquiring the application (from `spin up`
  exec to Spin's `Pulled`) took 267 ms at p50 and 832 ms at p95. The p95
  tail is name resolution of the registry host in the fresh sandbox,
  bimodal: most lookups take 1 to 4 ms, the rest 120 to 820 ms.
- ghcr.io: the same step took 944 ms at p50.
- The compiled Wasmtime cache (ADR 0014) removes compilation after the
  fetch, not the fetch.

A proof of concept mounted the replica's own application artifact as a
read-only OCI image layout and started Spin from it. Dispatch to first
direct response, 30 warm starts each:

| | p50 | p95 |
|---|---:|---:|
| registry (today) | 774 ms | 1319 ms |
| local OCI layout | 521 ms | 565 ms |
| registry and compiled cache | 511 ms | 1027 ms |
| local OCI layout and compiled cache | 126 ms | 184 ms |

None of the local runs made a registry request from the guest. A second
variant, a signed bundle derived from the application, measured 515/531 ms
and 114/162 ms. These numbers are evidence for the design, not release
gates; the production design copies blobs into the guest (see Runtime), so
it will be somewhat slower than the proof of concept.

### KubeSwift facts (v0.20.0; the contract dates from v0.19.3)

- `spec.artifacts` with `layout: oci` mounts a digest-pinned artifact into
  the guest as an OCI image layout of the manifest as pushed (`oci-layout`,
  `index.json`, `blobs/sha256/*`): regular files only, directories 0555 and
  files 0444, read-only at the virtio-fs daemon, the launcher bind and the
  guest mount. A skipped optional artifact mounts nothing; the path shows
  the image's own content, if any.
- The node artifact cache is shared by all namespaces and keyed by digest.
  An entry fetched from repository A is used for repository B only after B
  served every blob of it, read with the credentials of the run that asks
  (an origin marker per repository). A warm checkout fetches nothing: it
  projects an entry only if it is sealed and has the requesting
  repository's marker.
- Each checkout resolves the reference with the sandbox's credentials: the
  artifact's `pullSecretRef`, else `spec.imagePullSecret`. Successful
  answers are cached for the authorization TTL (default 5 minutes,
  `--artifact-authorization-ttl`, minimum 30 seconds) and renewed in the
  background after half of it; a refusal (400, 401, 403, 404) replaces the
  answer at once. For a cold start the answer can be up to twice the TTL
  old. Blobs are not read again once a repository's marker exists. Running
  mounts and node entries are never revoked.
- `SwiftArtifactVerificationPolicy` changes apply at the next checkout,
  cold start or placement run.
- A `SwiftArtifactPlacement` prepares artifacts on the nodes that run its
  pool's slots, with its own `pullSecretRef` only. Prepare pods run in the
  placement's namespace and count against its quota. Matching at checkout
  ignores the pull Secret; the sandbox's own resolve is the gate. Deleting a
  placement evicts nothing. Node cache eviction is off by default; an
  artifact may be up to 10 GiB by default.
- An optional artifact with `fetchTimeout: 0s`: on a warm checkout, a hit is
  `Mounted/Cached`, a miss is `Skipped/NotOnNode` and costs a second
  dispatch round trip. On a cold start (including a pool replica that finds
  no free slot), a hit costs a resolve wait of up to 250 ms and one init
  container; a miss is `Skipped/FetchTimeout` and never fills the cache.
- KubeSwift fetches with go-containerregistry (v0.22.1), which uses plain
  HTTP for `localhost` with a port, names under `.localhost`, the loopback
  addresses `127.0.0.1` and `::1`, and RFC1918 IP literals
  (`pkg/name/registry.go`, `Registry.Scheme`). Spin always uses HTTPS unless
  `--insecure` is given, which kubeswift-spin never passes.
- The pull Secret is used only by KubeSwift's materializer pods. The
  launcher never sees it.
- `index.json` carries an `org.opencontainers.image.ref.name` annotation
  naming whichever repository first wrote the digest on that node, possibly
  another tenant's.
- SwiftSandbox specs are immutable except `spec.ttl`, enforced by
  KubeSwift's admission webhook (failure policy Fail). The schema does not
  show the KubeSwift patch version: v0.19.0 and v0.19.1 fail, or never make
  ready, a replica whose optional artifact is skipped (kubeswift#843, #844,
  fixed in v0.19.2), and v0.19.2 skips an optional artifact already on the
  node on a cold start (#851, fixed in v0.19.3).

### Spin facts (4.2.1, rechecked on v4.2.5 and `main`)

v0.3.0 moves the runtime image from Spin 4.2.1 to 4.2.5 (tag commit
`e09d7d5`, Wasmtime 50.0.0). Spin 4.2.4 fixed a crafted component id in an
OCI application writing outside the assets directory (upstream
`509dde64f`), which affects today's `spin up --from` path independently of
this ADR. The facts below were established on 4.2.1 and rechecked against
the 4.2.5 and `main` sources; the field comparison and the golden test are
rerun on 4.2.5 before the converter is bound to it.

- Spin turns an OCI application into a locked application, then starts one
  process per trigger type:
  - Pull: the config blob is the lock unless a layer of media type
    `application/vnd.fermyon.spin.application.v1+config` exists; with
    several such layers the result is undefined in 4.2.1. Wasm layers
    (`application/vnd.wasm.content.layer.v1+wasm`, `application/wasm`)
    provide component sources; other layers and the members of archive
    layers provide files.
  - Load (`crates/oci/src/loader.rs`): a document with `spin_lock_version`
    is a `LockedApp`, otherwise the artifact is a bare Wasm package.
    `metadata.origin` becomes `vnd.fermyon.origin-oci:<reference>`. Every
    component `source`, component dependency `source` and component trigger
    dependency `source` is replaced wholesale by `{content_type, source:
    file://<cached blob>}`; a source without a digest is an error. If a
    component has `files`, each is written to `<working
    dir>/assets/<component id>/<path>` (paths whose every component is a
    normal name), and the list is replaced by one directory mount at `/`.
    Unknown fields are dropped by the typed round trip.
  - `spin up`: `--component-id` selection (with service-chaining
    validation), `--env` on every component, `<working dir>/spin.lock`, then
    `spin trigger http`, `spin trigger redis` or the plugin `spin
    trigger-<type>` with `SPIN_LOCKED_URL` and `SPIN_WORKING_DIR`. With one
    trigger type the trigger options pass through verbatim; with several,
    `spin up` probes and supervises the children.
- `spin trigger` is a hidden subcommand (`src/lib.rs`, `hide = true`),
  without documentation or a test that runs it directly; its environment
  contract is used de facto by trigger plugins.
- The trigger turns `file://` sources into reads in four places: component
  sources, component dependencies, component trigger dependencies and files
  mounts (`crates/factor-wasi/src/spin.rs` joins an absolute path as is). It
  also reads `SPIN_LOCKED_URL`, `SPIN_WORKING_DIR`, `SPIN_LOCAL_APP_DIR`,
  `SPIN_LOG_DIR`, `RUNTIME_CONFIG_FILE`, `WASMTIME_CACHE_FILE`,
  `DISABLE_WASMTIME_CACHE`, `SPIN_HTTP_LISTEN_ADDR`, `SPIN_TLS_CERT` and
  `SPIN_TLS_KEY` from its environment.
- A field-level comparison of the lock written by the real `spin up
  --from` with the proof of concept's converter, for six example
  applications, differed only in the Wasm file paths (same digests), an
  added `metadata.oci_image_digest` and a missing `metadata.origin`.

## Decision

Replicas checked out of a warm pool may start Spin from a verified copy of
their own application artifact, which KubeSwift mounts read-only as an OCI
image layout after a placement prepared it on the pool's nodes. The
canonical application stays the Spin OCI artifact in `SpinApp.spec.image`:
no new application format, no derived artifact, no new signature. Anything
the entrypoint detects before Spin starts falls back to `spin up --from`,
unchanged; a failure after Spin starts is a replica failure and starts a
hold (see Failure hold). The feature is off unless the platform turns it on.

### Components and ownership

| Component | Role |
|---|---|
| `internal/localapp` (pure) | Opt-in, eligibility, the placement key, name and spec, the limit selection, the artifact decoration, reasons. Used by the controller and the cache manager, as `compiledcache` is. |
| Controller | Decides eligibility, attaches the artifact to new warm replicas after the fingerprint, holds after failures, reports the `LocalApplication` condition. Reads placements; writes none. |
| Cache manager | Creates, updates and deletes the application placements. |
| Runtime entrypoint | Verifies the mounted layout, copies what Spin needs into its tmpfs, builds the locked application, starts Spin locally or falls back. |
| KubeSwift | Fetches, authorizes, caches and mounts the artifact; never sees Spin semantics. |

No new CRD.

### Opt-in

Three layers, as for the compiled cache:

1. Platform: chart value `localApplication.enabled` (default `false`). The
   chart refuses it unless `localApplication.nodeCacheBounded=true`
   acknowledges that KubeSwift's node artifact cache is bounded
   (`artifactCache.maxBytes` or `maxPercent` in the KubeSwift chart); see
   Resource bounds.
2. Administrator: the namespace label `spin.kubeswift.io/local-application:
   enabled`. The built-in `admin` role cannot change namespace labels, so a
   tenant cannot turn the feature on for itself.
3. Executor: the annotation `spin.kubeswift.io/local-application:
   enabled|disabled` (default `disabled`).

A SpinApp can opt out with the same annotation set to `disabled`; `enabled`
on a SpinApp whose executor does not enable the feature is a non-blocking
note; any other value blocks with `UnsupportedConfiguration`, and an invalid
executor value is `ExecutorInvalid`, as in ADR 0014. Turning the feature on
or off never replaces running replicas.

### Eligibility

A new replica gets the artifact only when all of these hold; each rule has a
`LocalApplication` reason:

- the three opt-in layers enable it and the SpinApp did not opt out;
- KubeSwift's schema shows `spec.artifacts[].optional`, `fetchTimeout`,
  `layout` and `pullSecretRef`, and the `swiftartifactplacements` resource
  exists (`Unsupported`). KubeSwift v0.19.3 or later is required and
  documented; the schema cannot show it (see Version detection);
- the replica comes from a warm pool, `spec.poolRef` set (`NoWarmPool`);
- `SpinApp.spec.image` matches `^[^@\s]+@sha256:[a-f0-9]{64}$` and is at
  most 512 bytes (`ImageNotPinned`). Resolving tags in the controller would
  need registry access with the tenant's credentials; it can be revisited if
  KubeSwift resolves tags for artifacts;
- the image's registry host, normalized as `compiledcache.PinnedReference`
  normalizes it (`docker.io` becomes `index.docker.io`), is one KubeSwift
  reaches over HTTPS only: not `localhost`, not a name under `.localhost`
  or `.local` (deliberately stricter than go-containerregistry, which uses
  HTTPS for `.local`), not a loopback or private-range IP literal
  (`RegistryNotEligible`); and, when the chart value
  `localApplication.registries` lists hosts (compared after the same
  normalization), it is one of them. The KubeSwift bump checklist rechecks
  go-containerregistry's `Registry.Scheme` against this classifier, and a
  unit test lists the hosts it must treat as plaintext;
- the SpinApp has exactly one image pull Secret, or none while the executor
  sets no runtime image pull Secret: KubeSwift would otherwise authorize the
  artifact with the executor's runtime credential (`PullSecrets`);
- the SpinApp selects no components (`spec.components`): reproducing
  Spin's selection would mean porting its service-chaining validation
  (`ComponentsSelected`);
- the executor's runtime image is pinned by digest and on the platform's
  list (`runtimeImage.digest` plus `localApplication.extraRuntimes`), so
  only images whose Spin and converter were tested together receive the
  artifact (`RuntimeNotAllowed`);
- no failure hold is active for the SpinApp (`DeliveryFailed`);
- the pool's application placement is trusted and reports `Prepared` for
  its current generation (`Placing`, `PlacementFailed`, `PlacementLimit`,
  `PlacementBlocked`).

The runtime decides the rest, which needs the application itself: trigger
types exactly `http`; no component dependencies or trigger dependencies;
an unambiguous lock; a supported Spin version.

### Identity and repository binding

The identity of a local application is the canonical form of
`SpinApp.spec.image`: the same repository and digest, with any tag removed
(`compiledcache.PinnedReference`). The controller uses it as the artifact
`ref` and the placement key. It never changes the repository, never pairs a
digest with another repository, and takes the reference from nowhere but
`spec.image`. SpinApp authors cannot add artifacts, change the mount path or
set the expected digest: the controller sets them, user variables are always
`SPIN_VARIABLE_*`, and SwiftSandbox specs are immutable.

Repository binding is KubeSwift's: digest D cached from repository A is
mounted for B@D only after B served all of D's blobs with the replica's own
credentials. kubeswift-spin adds no rule of its own; the e2e tests the
binding.

The runtime binds the content to the identity independently: it starts
from the expected digest (from the controller and from `--from`, not from
the layout) and hashes everything it uses. `metadata.origin` comes only from
`--from`; no layout annotation is read, logged or copied.

Spin and converter versions are not part of the identity: the artifact is
the application. Architecture is not part of it either: Spin applications
are WebAssembly.

### Pull authorization and revocation

The artifact and the placement name the SpinApp's image pull Secret, if
any, by name; neither the controller nor the cache manager reads it. The
model is exactly KubeSwift's:

- Removing or invalidating the pull Secret, or revoking access at the
  registry: new replicas fall back to the registry path, which then fails to
  pull as it would today, within KubeSwift's authorization TTL (default 5
  minutes; up to twice that for a pool replica that boots cold), or at once
  for a refusal the resolver sees. Running replicas keep their mount.
- The artifact deleted at the registry after placement: the same, through a
  404 refusal.
- A verification policy that starts denying the repository: new replicas
  fall back at the next checkout. Such a policy decides only whether the
  local copy is used; the fallback pulls the same repository in the guest.
- A verified copy on a node is not revoked or evicted by any of these; it
  is used again only after a fresh successful resolve with the replica's
  credentials.

The local copy does not extend authorization beyond that TTL window, which
KubeSwift documents for all artifacts.

### Delivery

For an eligible new replica the controller adds, after the revision
fingerprint is computed:

- an artifact: name `application`, `ref` the canonical reference,
  `layout: oci`, `mountPath: /opt/kubeswift-spin-app` (a runtime contract
  constant), `optional: true`, `fetchTimeout: 0s`, `pullSecretRef` the
  SpinApp's pull Secret if any, no claims and no
  `requirePlatformVerification`;
- the variable `KUBESWIFT_SPIN_APP_DIGEST=<manifest digest>`.

The replica's arguments are unchanged (`spin up --from=<spec.image> ...`),
so a fallback is exactly today's start. A runtime image older than the
feature does not know the variable and passes it to Spin, which ignores it.

### Placement lifecycle

The cache manager keeps one `SwiftArtifactPlacement` per (namespace, warm
pool, canonical reference, pull Secret name) that at least one eligible
SpinApp with replicas wants. It is separate from the compiled cache's: the
application can be placed in seconds while a compiled cache takes tens of
seconds, it is useful without one, and the two fail independently. Pools
stay generic; nothing recreates VMs.

- Name: `kss-app-` plus 20 hex characters of a SHA-256 over the key.
  Labels `app.kubernetes.io/managed-by=kubeswift-spin` and
  `spin.kubeswift.io/component=application-placement`; annotation
  `spin.kubeswift.io/application-key` with the full key hash. No owner
  reference (shared by SpinApps).
- Ownership: a placement is the cache manager's if it carries both labels
  and the annotation, its name matches the key hash, and the key-bearing
  fields of its spec (`poolRef`, and the single artifact's `ref`, `layout`
  and `pullSecretRef`) equal the key; the cache manager brings its other
  fields to what it writes (so defaults added by the API or by later
  versions do not orphan it). Anything else at the name is a conflict and is
  never modified (`PlacementBlocked`). The controller trusts a placement on
  the same test, with `Prepared` current for its generation. The annotation
  is a hash a tenant can compute; trust rests on the key fields and a
  current `Prepared`, and a forged placement costs at most a dispatch round
  trip and a fallback, never a different mount.
- Retention: when no SpinApp wants a placement any more, the cache manager
  records the time in the annotation `spin.kubeswift.io/unwanted-since` and
  deletes it after `localApplication.retention` (default 24 hours), so a
  rollback finds its placement prepared.
- Limits: at most `localApplication.maxPlacementsPerNamespace` placements
  per namespace (default 8), counting retained ones; at the limit, retained
  placements are deleted first, and wanted placements are kept in a stable
  order (oldest SpinApp creation, then name). At most
  `localApplication.maxNewPerHour` (default 10) of the namespace's existing
  application placements may have a `creationTimestamp` within the last
  hour; a new one waits until that holds. SpinApps beyond either limit
  report `PlacementLimit`. Both limits depend only on current objects, so
  the controller computes them with the same `internal/localapp` function.
- A placement the controller expects but that is missing for longer than
  two minutes (a conflict, a quota or admission refusal) is
  `PlacementBlocked`; the cache manager logs the cause and emits an Event.
  No `LocalApplication` message copies KubeSwift or API message text.
- The controller attaches the artifact only when the placement is
  prepared. A miss after that (an eviction, a new node) costs KubeSwift one
  extra dispatch round trip and falls back.

### Resource bounds

Tenant-chosen artifacts land on every node of a pool, in a disk all tenants
share. Enabling the feature therefore requires a bounded KubeSwift node
artifact cache (chart acknowledgement above), and the per-namespace and
per-hour placement limits apply. A generic per-artifact size limit on
`SandboxArtifact` and `PlacementArtifact` is proposed to KubeSwift
(`docs/upstream/`); until it exists, the operator documentation states
KubeSwift's 10 GiB per-artifact default and the cache bound as the limits.

### Runtime: verifying the layout

The entrypoint uses the layout only when every check passes. Each failure is
one log line with a fixed reason and a fallback; the plan is then
byte-for-byte the plan with the feature off.

1. `KUBESWIFT_SPIN_APP_DIGEST` is `sha256:` plus 64 lowercase hex, and the
   `--from=` argument ends in `@` plus that digest.
2. `/opt/kubeswift-spin-app` is a mount point whose topmost mount is
   read-only (`/proc/self/mountinfo`). A plain directory means the artifact
   was skipped.
3. Files are opened relative to a directory descriptor of the mount point
   with `openat2` and `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS |
   RESOLVE_NO_MAGICLINKS | RESOLVE_NO_XDEV`, `O_NOFOLLOW | O_NONBLOCK`, must
   be regular files by `fstat`, and are read with a size bound.
4. JSON (`oci-layout`, `index.json`, the manifest, the lock) is decoded
   strictly: duplicate keys, keys that differ only in case from a known
   struct field (not keys of map-typed fields such as `env`, `config`,
   `metadata`, `variables` or `trigger_config`, which are case-sensitive in
   Spin) and invalid UTF-8 fall back; numbers are kept as written. `oci-layout` is
   `{"imageLayoutVersion":"1.0.0"}`; `index.json` has schema version 2 and
   lists a descriptor with the expected digest and the OCI image manifest
   media type; annotations are ignored.
5. The manifest blob hashes to the expected digest, is at most 4 MiB, and is
   a schema-2 image manifest.
6. The lock is the single layer of media type
   `application/vnd.fermyon.spin.application.v1+config`, else the config
   blob; more than one such layer falls back. It hashes to its descriptor,
   is at most 16 MiB, contains `spin_lock_version`, and matches the field
   types of Spin's `LockedApp`. A bare Wasm package falls back.
7. Component sources resolve only to manifest layers of a Wasm media type;
   files only to other non-lock layers, archive-layer members or inline
   content; never to the config or the manifest.
8. Everything is bounded: 512 MiB per blob, 2 GiB per application in total,
   10,000 files, a decompression ratio for archive layers, and a deadline
   for the whole step (default 5 seconds), after which it falls back.

The verification and copy run as root before the privilege drop, as the
compiled-cache check does, streaming (no blob is held in memory whole). Go
is memory-safe and no external decompressor runs, so a separate
unprivileged process was not chosen; instead every write goes through
`openat2`/`mkdirat` relative to a descriptor of a freshly created output
root, with `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS` and `O_EXCL |
O_NOFOLLOW`, so a path bug cannot write elsewhere and duplicate or
conflicting paths fail instead of overwriting. Before copying, the
entrypoint checks free tmpfs space (`statfs`) against the manifest's total
size and falls back if it does not fit. The deadline cannot interrupt a
read blocked in the kernel on a hung virtio-fs mount; that only happens on
a KubeSwift fault and would hang the start.

### Runtime: copying and building the locked application

Spin never reads application code or files from the mount. Every blob the
lock uses is copied into `<tmpfs>/app/blobs/` while it is hashed (one read;
on any fallback after copying started, the entrypoint removes
`<tmpfs>/app` before `spin up` starts, so the fallback runs with the same
memory and filesystem as with the feature off), in root-owned directories of
mode 0555 and files of mode 0444, so Spin's user cannot change them. Files
are materialized under `<tmpfs>/app/assets/<component id>/` the same way.
Content replaced on the node after the check therefore never reaches Spin.
The copy costs guest memory comparable to what `spin up` writes into its
in-guest cache today.

The conversion reproduces what Spin's loader and `spin up` do for an OCI
application before the trigger starts, and nothing else:

- `metadata.origin` = `vnd.fermyon.origin-oci:` plus the `--from` value,
  normalized as Spin's `Reference` does;
- each component `source` is replaced wholesale by `{content_type, source:
  file://<copy>}`; a source without a digest, or with `inline` content,
  falls back;
- `files`, when present, must be an array and are replaced wholesale by one
  directory mount at `/`; each file path must have only normal components
  (Spin's rule), and the component id must be a single normal name;
- a component with a `dependencies` or `trigger_dependencies` key holding
  anything but an empty object falls back (v0.3.0; Spin resolves them, the
  golden test needs fixtures first);
- no other field changes; in particular no `oci_image_digest`, which would
  change `/.well-known/spin/info`.

The trigger receives only the converter's own output. A golden test in CI
pushes each example and fixture application to a loopback registry, runs the
pinned `spin up --from <ref>@<digest> --temp <dir>`, and compares the lock
it writes, and its `assets/` tree, with the converter's output field by
field; only file paths may differ, compared by digest. Fixtures cover files
(inline, per-file layers, archive layers), dependencies, host requirements,
`static_response` triggers, a multi-trigger application, and forged locks
(`source: file:///...` beside a digest, in sources and files; dependency
keys as `[]`, `null` or non-empty; traversal and duplicate component ids, which the pinned Spin 4.2.5 refuses;
manifests with case-variant or duplicate keys); each must match Spin or fall
back. The Spin bump checklist diffs `crates/oci/src/{client,loader}.rs`,
`crates/locked-app/src/locked.rs`, `crates/app/src/lib.rs`,
`src/commands/up.rs` and `crates/trigger/src/cli.rs` between tags.

### Credentials in the guest

The replica still receives the SpinApp's pull Secret as secret files for the
fallback (mode 0400, owned by root). On the local path the entrypoint does
not write Spin's `HOME/.docker/config.json`, so Spin and its components
never hold registry credentials; on the fallback path nothing changes.

### Environment

On both paths the entrypoint drops these inherited variables before Spin
starts and logs that it ignored them: `SPIN_LOCKED_URL`, `SPIN_WORKING_DIR`,
`SPIN_LOCAL_APP_DIR`, `SPIN_LOG_DIR`, `RUNTIME_CONFIG_FILE`,
`SPIN_HTTP_LISTEN_ADDR`, `SPIN_TLS_CERT`, `SPIN_TLS_KEY`, in addition to the
reserved Wasmtime variables it drops today. It strips every
`KUBESWIFT_SPIN_*` variable, including the new one. Only the entrypoint
decides what Spin runs.

### Spin-version binding

The runtime contract declares the exact `spin --version` line the converter
supports, taken from the verified 4.2.5 release binary (expected
`spin 4.2.5 (e09d7d5 2026-10-23)`), and a converter version. If the runtime
ships a privately patched Spin, the constant is that build's line and
digest. Spin
and the entrypoint ship in the same runtime image, Spin pinned by digest and
signature. `hack/test-runtime-image.sh` fails unless the image's `spin
--version` equals the constant, the golden test passed for that Spin, and a
converted example starts through `spin trigger http`, answers a request and
exits 0 on SIGTERM. The entrypoint does not run `spin --version` on the start
path, because it costs a process start; images built from the runtime image
with another Spin are excluded by the runtime list (Eligibility). Setting
the supported version to none makes every replica use the registry path; a
unit test covers that.

### The `spin trigger` dependency

The local path starts `spin trigger http` directly with `SPIN_LOCKED_URL` and
`SPIN_WORKING_DIR`, the command and environment `spin up` uses for an
application with only HTTP triggers, with `--listen` and
`--runtime-config-file` passed as `spin up` passes them. For a supported
application there is one trigger process, so there is no supervision to
reproduce; SIGTERM reaches the trigger, which exits as it does under `spin
up`. Variables (`SPIN_VARIABLE_*`), runtime configuration and invocation
limits reach Spin the same way on both paths; the runtime-image test covers
each.

The subcommand is hidden and has no documented stability. This is technical
debt until Spin offers a supported way to run a pre-resolved locked
application; it is proposed upstream as
[spinframework/spin#3753](https://github.com/spinframework/spin/issues/3753).
When Spin offers one, the runtime switches to it in a runtime
image release, without a controller change.

### Failure hold

Fallbacks happen before Spin starts. A replica that carried the
`application` artifact and failed without ever having been observed with
KubeSwift's `WorkloadReady` condition `True`, or that
KubeSwift failed for an artifact reason (`Artifact*`, or `KernelUnsupported`
naming artifacts), starts a hold: for 30 minutes, new replicas of that
SpinApp get no `application` artifact (`LocalApplication=False/
DeliveryFailed`, a Warning Event). The hold is in memory, as the compiled
cache's is. An artifact failure of a replica that carried both artifacts
starts both holds, because KubeSwift's failure does not always say which
artifact failed; that is conservative.

### Fallback

Everything below starts `spin up --from=<spec.image>` exactly as today:
feature off at any layer; SpinApp opted out; any eligibility rule; artifact
skipped by KubeSwift; mount missing or writable; any verification check;
deadline exceeded; unsupported Spin version or application feature. The
controller reports the reasons it knows; the runtime logs its reason in the
replica's console.

### Revision and rollout

The artifact and the variable are added after the revision fingerprint, as
for the compiled cache. A placement becoming ready, failing or going away,
or a hold, never replaces a healthy replica; they affect new replicas only.
A rollout from A to B proceeds as today; each new B replica uses the local
copy if B's placement is prepared when the replica is created, otherwise the
registry.

### Status, Events and metrics

The `LocalApplication` condition is present only for SpinApps whose executor
enables the feature and that did not opt out, and removed otherwise. It
never affects `Available`, `Progressing` or `readyReplicas`. `True/Attached`
means new warm replicas carry the artifact; whether Spin used it is in the
replica's console. Reasons (API): `NotConfigured` (the executor asks, the
chart value is off), `NamespaceNotEnabled`, `Unsupported`, `ImageNotPinned`,
`RegistryNotEligible`, `PullSecrets`, `ComponentsSelected`,
`RuntimeNotAllowed`, `NoWarmPool`, `Placing`, `PlacementFailed`,
`PlacementLimit`, `PlacementBlocked`, `DeliveryFailed`, `Attached`. Messages
name fields and chart values, never values from Secrets, KubeSwift or the
API server. Events `LocalApplicationReady`, `LocalApplicationSkipped` and
`LocalApplicationFailed` fire once per reason change. This extends the
status contract of ADR 0014; executor-contract.md is updated.

Metrics with fixed label values only (no namespace, name, digest or
registry): `kubeswift_spin_local_application_lookups_total{result}`
(`attached`, `ineligible`, `unplaced`, `held`), counted per created replica;
`..._attachments_total{result,reason}` from the replica's artifact status
(`mounted`; `skipped` with KubeSwift's skip reasons mapped to a fixed set);
`..._placements{state}` gauge (`prepared`, `preparing`, `failed`,
`retained`). The runtime's decision is one console line; kubeswift-spin does
not read consoles.

### RBAC and processes

- The cache manager registers the compiled-cache controllers only with
  compiled caches enabled and the application placement controller only with
  local applications enabled. With only local applications it has read
  access to SpinApps, executors, pools and namespaces, full access to
  `swiftartifactplacements`, leases and Events, and nothing on
  `swiftsandboxes` or `spincompiledcaches`.
- The controller gains get, list and watch on `swiftartifactplacements`
  (its cache selects only `component=application-placement`) and on
  namespaces whenever either feature is on, and a watch that maps placements
  to SpinApps.
- `hack/check_chart_policy.py` gains one approved rule set per feature
  combination and asserts that the controller never writes placements.
  docs/security-model.md is updated (RBAC table, cache manager, application
  artifacts, residual risks).

### Cold replicas

Not a v0.3.0 goal. A pool replica that boots cold because no slot is free
keeps the artifact; on a node-cache hit it pays the resolve wait and an init
container, on a miss it falls back. The performance validation measures it.
Replicas of executors without a pool get no artifact.

### Version detection

KubeSwift v0.19.3 or later is required: optional artifacts are skipped
rather than failing the sandbox, and a warm re-dispatch after `NotOnNode`
works (kubeswift#843, #844). The schema shows the fields but not the
version, so the requirement is documented, the upgrade order puts KubeSwift
first, and the controller logs the assumption at startup, as in ADR 0014.

### Upgrade and rollback

- With the feature off, nothing changes for existing users; compiled caches
  are unaffected.
- A controller older than v0.3.0 rejects unknown `spin.kubeswift.io/`
  executor annotations. Before rolling the controller back below v0.3.0,
  remove `spin.kubeswift.io/local-application` from executors (on a SpinApp
  it is only a note).
- A runtime image older than the feature ignores the artifact and starts
  from the registry.
- Moving the default runtime image to Spin 4.2.5 changes the replica
  template, so replicas of executors that use the default runtime roll once
  on upgrade (release notes). Wasmtime moves from 49.0.1 to 50.0.0:
  `runtimecontract.WasmtimeCacheVersion` changes and every compiled cache is
  rebuilt for the new runtime digest. The Spin release signature is verified
  with `hack/verify-spin-release.sh` and the runtime Dockerfile digests are
  updated.

### Validation required before release

- Unit and fuzz tests for every runtime check and fallback: wrong digest,
  manifest and blob tampering, writable mount, symlinks, submounts and `..`
  escapes, oversized blobs and archives, unexpected layout files, strict
  JSON cases, environment redirection, unsupported Spin version, deadline;
  the fallback plan equals the feature-off plan byte for byte, and
  `KUBESWIFT_SPIN_*` is stripped on both paths; HOME has no
  `.docker/config.json` on the local path.
- The golden lock test with the fixtures above.
- envtest: eligibility and every reason, decoration after the fingerprint,
  the artifact ref byte-equal to the canonical `spec.image`, no SpinApp field
  or annotation able to add artifacts or set the variable, placement
  lifecycle, ownership, conflicts, limits and retention, a placement for
  another application never trusted, the failure hold.
- KVM e2e: the seven examples from local content; the registry made
  unresolvable after placement and a fresh warm replica serving, with
  console evidence of no OCI client request; repository binding (one digest
  under two repositories, one without access); a private registry behind a
  private CA, with credential removal and invalid credentials; an HTTP-only
  registry never used on the local path; the four combinations with the
  compiled cache; unsupported applications falling back.
- Performance: at least 30 warm starts each for registry and local, with and
  without the compiled cache, compared structurally with the proof of
  concept; registry request counts at dispatch and at placement.

## Alternatives considered

- **A signed bundle derived from the application.** 6 to 34 ms faster in
  the proof of concept, less once blobs are copied. It needs a trusted
  derivation workload that builds the lock itself (Spin's trigger trusts
  `file://` sources as given, so a lock from an untrusted step could read any
  guest file), another artifact per application, a signing identity and
  policy, an index and its lifecycle, and more registry objects and failure
  modes. The OCI layout uses the canonical artifact and keeps the digest as
  the only trust root.
- **Reading the mounted blobs in place.** Faster, but Spin would read code
  after the check from a host-backed cache that principals trusted with the
  node can write; the copy makes the check hold at the time of use.
- **Spin's registry cache, pre-seeded.** Every start still contacts the
  registry, and the cache must be writable.
- **A local registry or proxy.** Still round trips and a writable cache per
  start, plus a server to run.
- **Spin's Rust libraries in a custom binary.** Internal crates that change
  between releases, and the signed upstream Spin release would be lost.
- **Waiting for upstream Spin.** No supported interface exists; it is
  proposed in [spinframework/spin#3753](https://github.com/spinframework/spin/issues/3753),
  and the feature does not wait for it.

## Consequences

- For eligible applications, the guest makes no registry request after
  placement; KubeSwift's controller still authorizes each digest with the
  registry, host-side, at least once per authorization TTL.
- kubeswift-spin owns a converter that tracks Spin's OCI loading, bound to
  one Spin version and guarded by a golden test, and depends on the
  undocumented `spin trigger` subcommand until upstream offers an interface.
- The cache manager runs in more installations and manages a second kind of
  placement; node cache usage grows with distinct application digests per
  pool, within the required bounds.
- Registry fetches move from the guest to KubeSwift's control plane for
  eligible applications: they no longer go through the executor's network
  mode or egress allowlist, which is why plaintext and, optionally,
  unlisted registries are excluded.
- Revocation reaches new replicas within KubeSwift's authorization TTL;
  running replicas are unaffected.
- Residual risks for the security model: with a bounded node cache, one
  namespace's placements can evict other tenants' idle entries, compiled
  caches included (slower starts and fallbacks, nothing else); with
  `localApplication.registries` empty, KubeSwift's control plane makes
  HTTPS requests to registry hosts tenants choose.
- Upstream proposals: in KubeSwift, a per-artifact size limit, an HTTPS-only
  switch for registries, writing the requester's reference (or none) in
  `index.json`, and an explicit anonymous artifact pull
  ([kubeswift-artifact-fetch-hardening.md](../upstream/kubeswift-artifact-fetch-hardening.md));
  in Spin, a supported
  way to run a pre-resolved locked application
  ([spinframework/spin#3753](https://github.com/spinframework/spin/issues/3753)).
