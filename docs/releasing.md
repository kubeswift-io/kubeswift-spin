# Releasing

Releases are cut from `main` by pushing a semver tag. The release workflow
(`.github/workflows/release.yaml`) runs only for tags in
`kubeswift-io/kubeswift-spin` and waits for approval of the `release`
environment, which must be configured with required reviewers in the
repository settings. The workflow has not been run on GitHub yet; the
artifacts below describe what it is written to publish.

## Checklist

1. `make verify-all` and `make kind-test WITH_SPIN_OPERATOR=1` pass.
2. The KVM end-to-end test passes on a KubeSwift cluster
   ([test/e2e](../test/e2e/README.md)); record the versions in
   [compatibility.md](compatibility.md).
3. `CHANGELOG.md` has an entry for the version, including upgrade notes if
   the rendered SwiftSandbox spec changed.
4. `charts/kubeswift-spin/Chart.yaml` `appVersion` and the example manifest
   tags match the version.
5. If Spin, the entrypoint, `internal/runtimecontract` or the runtime base
   image changed since the last release, bump `runtime/VERSION` (for example
   `spin-4.2.1-r2`) and `runtimeImage.tag` in the chart values, and add an
   upgrade note: every replica using the default runtime image is replaced.
   Otherwise leave both unchanged, so the release does not replace running
   replicas.
6. Tag and push:

   ```bash
   git tag -s v0.1.0 -m v0.1.0
   ```

   ```bash
   git push origin v0.1.0
   ```

## What the workflow publishes

| Artifact | Location |
|---|---|
| controller image | `ghcr.io/kubeswift-io/kubeswift-spin:<tag>` (amd64, arm64) |
| runtime image | `ghcr.io/kubeswift-io/kubeswift-spin-runtime:<runtime/VERSION>` (amd64, arm64), built only when that tag does not exist yet |
| Helm chart | `oci://ghcr.io/kubeswift-io/charts/kubeswift-spin`, version without the `v` |
| example artifacts | `ghcr.io/kubeswift-io/kubeswift-spin-examples/<app>:<tag>` |
| GitHub release | draft, with image digests |

Images are signed with cosign keyless signing and carry SBOM and provenance
attestations. The packaged chart pins both images by digest.

Verify an image signature:

```bash
cosign verify ghcr.io/kubeswift-io/kubeswift-spin-runtime:spin-4.2.1-r1 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/kubeswift-io/kubeswift-spin/.github/workflows/release.yaml@refs/tags/v'
```
