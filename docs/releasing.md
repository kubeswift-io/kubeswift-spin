# Releasing

Releases are cut from `main` by pushing a semver tag. The release workflow
(`.github/workflows/release.yaml`) runs only for tags in
`kubeswift-io/kubeswift-spin` and waits for approval of the `release`
environment, which must be configured with required reviewers in the
repository settings.

## Checklist

1. `make verify-all` and `make kind-test WITH_SPIN_OPERATOR=1` pass.
2. The KVM end-to-end test passes on a KubeSwift cluster
   ([test/e2e](../test/e2e/README.md)); record the versions in
   [compatibility.md](compatibility.md).
3. `CHANGELOG.md` has an entry for the version, including upgrade notes if
   the rendered SwiftSandbox spec changed.
4. `charts/kubeswift-spin/Chart.yaml` `appVersion` and the example manifest
   tags match the version.
5. Tag and push:

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
| runtime image | `ghcr.io/kubeswift-io/kubeswift-spin-runtime:<tag>` (amd64, arm64) |
| Helm chart | `oci://ghcr.io/kubeswift-io/charts/kubeswift-spin`, version without the `v` |
| example artifacts | `ghcr.io/kubeswift-io/kubeswift-spin-examples/<app>:<tag>` |
| GitHub release | draft, with image digests |

Images are signed with cosign keyless signing and carry SBOM and provenance
attestations. The packaged chart pins both images by digest.

Verify an image signature:

```bash
cosign verify ghcr.io/kubeswift-io/kubeswift-spin-runtime:v0.1.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/kubeswift-io/kubeswift-spin/.github/workflows/release.yaml@refs/tags/v'
```
