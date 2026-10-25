# KubeSwift requirement: bounds and transport guarantees for artifacts that tenants choose

Status: proposal, not filed. Written for kubeswift-spin v0.3.0
([ADR 0015](../adr/0015-local-application-delivery.md)), which lets
platform-enabled namespaces mount an application artifact that the tenant
names. None of it is Spin-specific; kubeswift-spin works around each gap
as described below.

## User problem

When a platform component creates `spec.artifacts` entries or
`SwiftArtifactPlacement`s for references that tenants choose, KubeSwift's
control plane and its privileged materializer pods fetch content from
hosts and in sizes the tenant decides, into a node cache all tenants share.
Four properties of KubeSwift v0.20.0 matter then:

1. **Size.** An artifact may be up to the materializer's default limit (10
   GiB) unless the node cache is bounded as a whole. A platform cannot ask
   for a smaller limit for one artifact or placement.
2. **Transport.** The materializer and the resolver use go-containerregistry,
   which talks plain HTTP to `localhost` with a port, `*.localhost`,
   loopback addresses and RFC1918 IP literals, without the reference asking
   for it. A pull Secret for such a registry is sent in clear text to
   whatever answers.
3. **Layout metadata.** `layout: oci` writes an `index.json` whose
   `org.opencontainers.image.ref.name` names the reference of whichever run
   first wrote the digest on the node. A workload in another namespace that
   mounts the same digest from its own repository sees the first writer's
   repository name.
4. **Credentials.** An artifact without `pullSecretRef` is resolved and
   fetched with `spec.imagePullSecret`, the rootfs image credential. A
   platform that wants an artifact fetched anonymously cannot say so.

## Proposal

1. An optional `maxBytes` (quantity) on `SandboxArtifact` and
   `PlacementArtifact`, enforced like the global limit (declared sizes
   before download, actual sizes during it), and never larger than the
   global limit.
2. A controller-manager option (and chart value) that refuses plaintext
   registries for artifacts unless the artifact (or an administrator list of
   registries) explicitly allows insecure transport; a refused artifact is
   `Skipped` (optional) or fails (required) with a dedicated reason.
3. `index.json` written with the requesting run's reference, or without
   `ref.name`, so that a mount never names another repository.
4. An explicit way to request an anonymous fetch for one artifact (for
   example `pullSecretRef: {}` or `anonymous: true`), so that a platform need
   not reason about `spec.imagePullSecret` falling back.

## What kubeswift-spin does meanwhile

- Requires the node artifact cache to be bounded before the feature can be
  enabled, and limits placements per namespace and per hour.
- Treats registry hosts that go-containerregistry would contact over plain
  HTTP as ineligible, so those applications keep the in-guest pull, which
  always uses HTTPS.
- Never reads `index.json` annotations.
- Makes a SpinApp without image pull Secrets ineligible when its executor
  sets a runtime image pull Secret.
