# 0004: One runtime rootfs image plus the unchanged Spin application artifact

Status: accepted

## Context

A SwiftSandbox boots an OCI image as its root filesystem. A SpinApp names a
Spin OCI artifact, which is a manifest plus Wasm layers, not a filesystem
image. Options were to convert every application into a bootable image, or
to boot a generic Spin image that pulls the application.

## Decision

Build one runtime image (`kubeswift-spin-runtime`) containing the pinned
static Spin binary and an entrypoint. Every SpinApp boots it; Spin pulls
`SpinApp.spec.image` at start with `spin up --from`. The entrypoint drops
root to UID 65532 because KubeSwift runs the workload as root and ignores
the image user.

## Consequences

- No per-application image builds or registries; nodes materialize one
  rootfs, and warm pools can hold it booted.
- The guest needs egress to the application registry, which excludes
  network mode `none`, and private registries would need credentials in the
  guest, which are not supported. Host-side artifact projection is proposed
  upstream.
- Spin version upgrades are runtime image upgrades and roll replicas.

## Update (KubeSwift v0.16.0, 2026-10-05)

The decision stands. Private application registries are now supported:
KubeSwift v0.16.0 delivers `imagePullSecrets` to the guest as secret files
and the entrypoint writes them to Spin's Docker configuration
([ADR 0006](0006-secrets.md),
[executor-contract.md](../executor-contract.md#registry-credentials)). The
credentials are therefore inside the guest. KubeSwift v0.16.0 also added
read-only artifact projection (`spec.artifacts`), but Spin 4.2.1 cannot run
an application from a local OCI layout, so Spin still pulls the application
inside the guest
([kubeswift-sandbox-artifact-projection.md](../upstream/kubeswift-sandbox-artifact-projection.md)).
