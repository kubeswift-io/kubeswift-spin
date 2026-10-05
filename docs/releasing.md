# Releasing

Releases are cut from `main` by pushing a signed semver tag. The release
workflow (`.github/workflows/release.yaml`) runs only for tags in
`kubeswift-io/kubeswift-spin`. Every job that publishes or holds a write
token uses the `release` environment and waits for its approval.

The workflow had not run before v0.1.0-rc1; that tag is its first run.
Check every artifact (see [Verifying a release](#verifying-a-release))
before publishing the draft release.

## Repository settings

Configure these before the first release:

- Environment `release`: required reviewers (the maintainers), and a
  deployment tag rule `v*` so that only version tags can deploy to it.
- A tag ruleset for `refs/tags/v*` that lets only maintainers create,
  update or delete version tags. The environment approval protects only
  jobs that declare the environment, and a tag pushed on a commit whose
  workflow omits it would publish and sign without approval.
- Workflows from fork pull requests need approval for all outside
  collaborators.
- No self-hosted runner outside a runner group restricted to
  `.github/workflows/kvm-e2e.yaml` on `main` (see
  [test/e2e](../test/e2e/README.md#ci)).
- Private vulnerability reporting enabled (Settings, Code security), which
  [SECURITY.md](../SECURITY.md) relies on.
- Artifact attestations need a public repository (or GitHub Enterprise
  Cloud for a private one).
- Packages under `ghcr.io/kubeswift-io` that already exist when the
  workflow first pushes to them must grant this repository write access
  (package settings, Manage Actions access). A package that the workflow
  creates is linked to the repository; set it to public once after the
  first release.

## Checklist

1. `make verify-all` and `make kind-test WITH_SPIN_OPERATOR=1` pass, and CI
   is green on the commit to tag.
2. The KVM end-to-end test passes on a KubeSwift cluster
   ([test/e2e](../test/e2e/README.md)); record the versions in
   [compatibility.md](compatibility.md).
3. `CHANGELOG.md` has an entry for the version, including upgrade notes if
   the rendered SwiftSandbox spec changed.
4. `charts/kubeswift-spin/Chart.yaml` `version` and `appVersion`, the
   example manifests and the install commands in the README use the
   version.
5. If Spin, the entrypoint, `internal/runtimecontract` or the runtime base
   image changed since the last release, bump `runtime/VERSION` (for example
   `spin-4.2.1-r2`) and `runtimeImage.tag` in the chart values, and add an
   upgrade note: every replica using the default runtime image is replaced.
   Otherwise leave both unchanged, so the release does not replace running
   replicas.
6. Tag and push. A tag with a pre-release suffix (`v0.1.0-rc1`) produces a
   pre-release:

   ```bash
   git tag -s v0.1.0-rc1 -m v0.1.0-rc1
   ```

   ```bash
   git push origin v0.1.0-rc1
   ```

7. Approve the `release` deployments, wait for every job, check the
   artifacts (below), then publish the draft GitHub release.

## What the workflow publishes

| Artifact | Location |
|---|---|
| controller image | `ghcr.io/kubeswift-io/kubeswift-spin:<tag>` (linux/amd64, linux/arm64) |
| runtime image | `ghcr.io/kubeswift-io/kubeswift-spin-runtime:<runtime/VERSION>` (linux/amd64, linux/arm64), built only when that tag does not exist yet; an existing tag is reused only if this workflow signed it |
| Helm chart | `oci://ghcr.io/kubeswift-io/charts/kubeswift-spin`, version without the `v`; it pins both images by digest |
| example artifacts | `ghcr.io/kubeswift-io/kubeswift-spin-examples/<app>:<tag>` |
| GitHub release | draft, marked as a pre-release for tags with a `-` suffix, with generated notes and these files: the chart package, SPDX SBOMs of both images, `images.txt` and `examples.txt` (references with digests) and `SHA256SUMS` |

No `latest` tag is published. Only linux/amd64 has been tested end to end;
the arm64 images are built but not validated.

Images, the chart and the example artifacts are signed with cosign keyless
signing by the workflow. Images and the chart also have GitHub artifact
attestations (SLSA provenance), and the images carry BuildKit SBOM and
provenance attestations.

## Verifying a release

Signatures. Check the exact workflow identity of the release tag, not a
pattern:

```bash
cosign verify ghcr.io/kubeswift-io/kubeswift-spin:v0.1.0-rc1 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/kubeswift-io/kubeswift-spin/.github/workflows/release.yaml@refs/tags/v0.1.0-rc1
```

The same command verifies
`ghcr.io/kubeswift-io/kubeswift-spin-runtime:spin-4.2.1-r1` (signed by
the release that first built it; v0.1.0-rc1 for `spin-4.2.1-r1`),
`ghcr.io/kubeswift-io/charts/kubeswift-spin:0.1.0-rc1` and
`ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http:v0.1.0-rc1`.

Provenance attestations:

```bash
gh attestation verify oci://ghcr.io/kubeswift-io/kubeswift-spin:v0.1.0-rc1 --repo kubeswift-io/kubeswift-spin
```

The same command verifies the runtime image and the chart
(`oci://ghcr.io/kubeswift-io/charts/kubeswift-spin:0.1.0-rc1`). The
example artifacts are signed but have no attestation.

Release files, after downloading them from the GitHub release:

```bash
sha256sum -c SHA256SUMS
```
