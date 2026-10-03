# KubeSwift requirement: read-only OCI artifact projection into SwiftSandbox

Status: proposal for KubeSwift. Not implemented in KubeSwift v0.15.1.

## User problem

Many sandbox workloads need content that is not part of their root
filesystem image: application bundles, plugins, datasets, policy bundles,
WebAssembly applications. Today the workload has to download that content
itself from inside the guest, which means:

- the guest needs network egress to the registry, so `network: none` is
  impossible;
- private registries need credentials inside the guest, which cannot be
  delivered securely (see
  [kubeswift-sandbox-secret-projection.md](kubeswift-sandbox-secret-projection.md));
- every replica downloads the same bytes, and the content is not verified
  before the workload sees it.

## Why this is generic KubeSwift functionality

KubeSwift already pulls, caches per node, cosign-verifies and mounts
read-only OCI content into sandboxes: `spec.model` does exactly this for
model weights, using the sandbox's `imagePullSecret` and
`verifyKeySecretRef`. Generalizing it to any OCI artifact (any media types,
not only filesystem layers) is useful to every workload that consumes
packaged content, and keeps credentials on the host side.

## Why it does not belong in kubeswift-spin

kubeswift-spin cannot put files into a guest, and pulling artifacts in the
controller would put a registry client, credentials and a cache in the
control plane for no benefit. Reusing `spec.model` for Spin applications
would be a misuse: a Spin artifact is a manifest plus Wasm layers
(`application/vnd.wasm.content.layer.v1+wasm`), not a filesystem image, and
coupling Spin to a model-specific field is the kind of hack this project
avoids.

## Current behavior (v0.15.1)

- `spec.model.imageRef` mounts the filesystem of one OCI image read-only
  over virtio-fs at `mountPath`; it is materialized once per node, keyed by
  digest, and verified with `verifyKeySecretRef` when set.
- There is no way to mount an arbitrary artifact or to preserve its layers
  and manifest as files.

## Proposed API

```yaml
spec:
  artifacts:
    - name: app
      ref: registry.example.com/team/hello-http@sha256:...
      mountPath: /run/artifacts/app
      layout: oci          # oci: OCI image layout (index.json, blobs/), unpacked: filesystem layers
      pullSecretRef: {name: team-registry}   # optional, defaults to spec.imagePullSecret
      verifyKeySecretRef: {name: team-cosign} # optional
```

With `layout: oci`, the mount contains an OCI image layout of the artifact
(manifest, config and blobs by digest), which any OCI-aware tool in the
guest can read offline.

## Controller behavior

- Resolve the reference to a digest and record it in status.
- Pull on the node with the referenced Secret, verify the signature when a
  key is given, cache by digest, and share read-only over virtio-fs, as
  `spec.model` does.
- Fail the sandbox before boot when the pull or verification fails, with a
  reason that names the artifact.
- Include artifacts in the warm-pool slot shape, or mount them at checkout.

## Security considerations

- Registry credentials stay on the host side; the guest never sees them.
- Verification happens before the workload starts.
- The mount is read-only, so a compromised workload cannot alter the cached
  artifact shared with other sandboxes on the node.
- Cache keys must be digests, never tags.

## Compatibility implications

Additive field. With it, kubeswift-spin could pass `spin up --from` a local
OCI layout path instead of a registry reference, which would enable private
Spin application registries (SpinApp `imagePullSecrets`), `network: none`
executors and digest pinning of the application at reconcile time. This
requires Spin to run an application from a local OCI layout; Spin 4.2.1
supports registry references, manifests and Wasm files for `--from`, so the
Spin side needs to be confirmed or contributed as well.

## Tests required

- A private artifact is mounted with the referenced pull Secret and its
  credentials are absent from the guest.
- A tampered or unsigned artifact fails the sandbox before boot when a key
  is configured.
- Two sandboxes on one node share one cached copy.
- The mount is read-only in the guest.
- `network: none` sandboxes can read the artifact.
