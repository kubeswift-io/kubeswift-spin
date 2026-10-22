# KubeSwift requirement: a node artifact cache tenants cannot write

Status: proposal, not filed. Found while designing the compiled Wasmtime
cache ([ADR 0014](../adr/0014-compiled-wasmtime-cache.md)) on KubeSwift
v0.19.1.

## User problem

A platform that mounts artifacts it vouches for (compiled code, signed
indexes) into tenants' sandboxes relies on KubeSwift's node artifact cache:
a warm checkout mounts an entry once a verification record for the
artifact's digest, repository, policy, key and claims exists on the node,
without re-reading the content.

In KubeSwift v0.19.1 that cache, `/var/lib/kubeswift/sandbox-artifacts`, is
written by fetch pods (checkouts) and prepare pods (placements) that run in
the sandbox's or placement's namespace with a read-write hostPath, and the
cold-path launcher, also in the sandbox's namespace, is privileged. A
principal with `pods/exec`, `pods/attach`, `pods/ephemeralcontainers` or pod
update in any namespace that has sandboxes, which the built-in `edit` and
`admin` roles grant, can therefore replace verified content on that node,
including content verified for other namespaces. The next checkout on the
node mounts the replaced content as verified.

KubeSwift documents such principals as trusted with the node. Platforms
that grant namespace `edit` to tenants cannot rely on artifact verification
across namespaces as a result.

## Proposed behavior

Any of these closes the gap; they are listed from least to most change:

1. Run fetch and prepare materializers in the KubeSwift namespace, not the
   tenant's, with the tenant's pull credentials passed in by reference,
   as output export already does for push credentials.
2. Verify content at mount: the launcher, or swiftletd for a warm
   checkout, checks the unpacked tree against a content digest recorded at
   verification time, out of reach of tenant pods, and refuses a mismatch.
3. Separate the cache per namespace (or per policy binding), so content
   written from one namespace is never mounted in another.

## Why this belongs in KubeSwift

The cache, its records and the pods that write it are KubeSwift's; no
client can protect them. The need is generic: any platform-verified
artifact, not only compiled code.

## kubeswift-spin until then

kubeswift-spin enables compiled caches only in namespaces an administrator
labels, and documents that such namespaces must not grant non-platform
principals exec, attach, ephemeral containers or pod writes
([compiled-cache.md](../compiled-cache.md#prerequisites)).
