# KubeSwift requirements: accelerating artifacts and producing them in a sandbox

Status: proposal, not filed. Builds on `spec.artifacts`, which KubeSwift
v0.16.2 mounts read-only at warm-pool checkout as well as at a cold start.

## User problem

KubeSwift v0.16.2 to v0.16.4 can attach immutable OCI artifacts to a
sandbox, including one checked out of a warm pool: the artifact is pinned
by digest, optionally cosign-verified, cached on the node per repository,
and mounted read-only over virtio-fs before the workload starts, with no
VM restart. Pools stay generic because artifacts are not part of the slot
shape.

That is enough for an artifact the workload needs. It is not enough for an
artifact that only speeds the workload up and that the workload can do
without, for example:

- a precompiled or pre-indexed form of content the workload can also
  produce itself (compiled code caches, search indexes, decompressed
  datasets);
- an optional configuration or data bundle with a built-in default.

For such an artifact:

1. **A failure is fatal.** If the artifact cannot be resolved, fetched,
   verified or materialized, the sandbox goes `Failed`
   (`ArtifactResolveFailed`, `ArtifactMaterializeFailed`,
   `ArtifactMissing`). The workload, which would have run correctly without
   it, never starts. The client must notice the failure, drop the artifact
   and create the sandbox again, which costs a failed attempt and a cold or
   warm start.
2. **The first checkout on each node is slow.** A warm slot on a node that
   does not yet hold the artifact starts a fetch pod and dispatches again
   afterwards. Nothing places an artifact on the nodes of a pool before
   the first checkout that needs it, and nothing prefers a slot on a node
   that already holds it.
3. **The node cache only grows.** `/var/lib/kubeswift/sandbox-artifacts` has
   no garbage collection and no size limit. Content that is replaced
   regularly (a new artifact per application version) accumulates on every
   node.
4. **Verification is key-only.** An artifact can be verified only against
   one cosign public key, without a transparency log. Keyless signatures
   and a policy (several keys, an identity per repository) are not
   available.
5. **Authorization is re-checked in the checkout path.** The controller
   caches its resolve of a digest reference (the check that the sandbox's
   credentials may read the repository) for 5 minutes and, on expiry,
   repeats it before claiming a slot. In a lab measurement every checkout
   after an expiry took about 0.75 s longer. Digest references are
   immutable, so the check could run concurrently with the claim, or be
   cached longer, without weakening the binding.
6. **Nothing can produce an artifact in isolation.** A platform that must
   run an untrusted transformation (compiling, indexing) to produce such an
   artifact can run it in a sandbox, but cannot get the result out without
   giving the guest registry credentials.
7. **Projection is not visible.** The sandbox is `Running` with
   `GuestRunning=True` (`CheckedOut`) before the artifacts are projected,
   and stays so while a fetch pod runs. A client cannot tell when the
   artifact is in place or whether the workload started without it.

## Proposed API

Each item is independent and can be filed and shipped separately.

1. **Optional artifacts.** `spec.artifacts[].optional` (bool, default
   `false`). When `true`, a failure to resolve, fetch, verify or
   materialize the artifact, or a registry that stays unreachable past a
   per-artifact `fetchTimeout`, does not fail the sandbox: nothing is
   mounted at `mountPath` (the path does not exist in the guest), the
   workload starts without it, and the sandbox records why (item 7).
   Verification stays strict: an optional artifact that fails verification
   is never mounted.
2. **Placement on pool nodes.** KubeSwift itself keeps the artifacts
   recently checked out from a pool materialized on every node that holds a
   slot of that pool, and a checkout prefers a slot on a node whose cache
   already holds the sandbox's artifacts. Clients do not write pool specs
   for this (pools are usually owned by administrators, and per-version
   digests would make pool edits frequent). Placement uses only the
   repositories and credentials of the pool's own namespace and never
   changes the slot shape.
3. **Node cache lifecycle.** A size limit per node (absolute and as a
   fraction of the filesystem) and least-recently-used eviction of entries
   that no running sandbox references. Eviction never removes the
   per-repository authorization or verification records of an entry that
   stays.
4. **Verification policy tenants cannot modify.** A verification policy
   referenced by the sandbox but held outside the sandbox's namespace (a
   cluster-scoped object or a platform namespace), with more than one
   accepted key, keyless verification (`certificateIdentity`,
   `certificateOIDCIssuer`, an optional Rekor URL), and optional checks of
   signed claims (annotations or attestation predicates that must match
   values the sandbox supplies, for example a namespace). Today
   `verifyKeySecretRef` is a Secret in the sandbox's namespace, which
   anyone who can write Secrets there can replace.
5. **Authorization off the critical path.** The resolve of a digest
   reference (the check that the sandbox's credentials may read the
   repository) runs concurrently with the slot claim. It still always
   completes, and succeeds, before anything is mounted; the claim is
   released if it fails. The cache of authorization results stays bounded,
   so revoked credentials still take effect.
6. **Output export.** A sandbox can declare a guest directory that
   KubeSwift packages and pushes as an OCI artifact when the workload
   exits successfully, with credentials that never enter the guest (for
   example `spec.outputs[]` with `path`, `repository`, `pushSecretRef`),
   and records the pushed digest in status. This lets a platform run an
   untrusted build or transformation step in a microVM without giving the
   guest registry credentials.
7. **Pool and sandbox status.**
   - `SwiftSandboxPool.status` advertises what its slots can take, at least
     whether they can mount artifacts at checkout (kernel bridge feature
     `warm-mounts`), so a client can avoid sandboxes that would fall back to
     a cold boot without reading pods.
   - `status.artifacts[]` gains `state` (`Mounted`, `Skipped`, `Failed`),
     `reason` and `mountedAt`, set when swiftletd projects the artifact
     (warm path) or when the launcher starts (cold path), before the
     workload is dispatched.

## Semantics

- An optional artifact never delays the workload beyond its own resolve
  and fetch timeouts.
- A required artifact (the default) keeps today's behavior.
- Concurrent checkouts that need the same uncached artifact on one node
  share one fetch.
- Read-only guarantees, digest pinning, repository binding of the node
  cache, per-sandbox authorization before mount and the mount ordering
  before dispatch are unchanged.

## Why this belongs in KubeSwift

Every client that attaches accelerating content has the same needs:
starting without the content when it is unavailable, placing it before the
first use, bounding the node cache, verifying with a policy the workload's
namespace cannot change, and producing such content in an isolated
sandbox. Doing it in each client means
creating a sandbox, watching it fail, and creating it again, and leaves the
node cache unbounded regardless.
