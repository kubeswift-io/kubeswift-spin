# Runtime image

`ghcr.io/kubeswift-io/kubeswift-spin-runtime` is the OCI image every
kubeswift-spin SwiftSandbox boots as its root filesystem. One image serves
every SpinApp: the application is not baked in, Spin pulls it at start.

## Versioning

The runtime image is versioned independently of the controller. Its tag is
in `runtime/VERSION` (currently `spin-4.2.1-r1`: the Spin version and a
revision for entrypoint or base image changes), and the chart's
`runtimeImage.tag` must match it (`make helm-lint` checks this). The release
workflow builds and pushes the runtime image only when that tag does not
exist yet. Because the image is part of every sandbox spec, keeping its tag
and digest stable across controller releases is what lets a controller
upgrade leave running replicas alone.

## Contents

| Path | Origin |
|---|---|
| `/usr/local/bin/spin` | Spin v4.2.1, `spin-v4.2.1-static-linux-<arch>.tar.gz` from the GitHub release, SHA-256 verified |
| `/usr/local/bin/kubeswift-spin-entrypoint` | `cmd/spin-entrypoint`, static Go binary |
| `/usr/share/doc/spin/LICENSE` | Spin license from the release archive |
| `/var/lib/kubeswift-spin/` | empty, owned by 65532 |
| everything else | `gcr.io/distroless/static-debian12:nonroot`, pinned by digest: CA certificates, `/etc/passwd` and `/etc/group` with the `nonroot` user, tzdata |

There is no shell, package manager, `curl` or `wget`. The image is about
150 MB, almost all of it the Spin binary. The static Spin build is used so
the image needs no C library.

## Entrypoint contract

KubeSwift runs the sandbox command as root and ignores the image `USER`.
The entrypoint (`cmd/spin-entrypoint`) is therefore what makes Spin
unprivileged:

1. Accept only `up ...` (the controller's arguments) or `--version`.
2. Create `/var/lib/kubeswift-spin/{state,home,tmp}` with mode 0700.
3. If `KUBESWIFT_SPIN_RUNTIME_CONFIG_B64` is set, decode it (at most 64 KiB)
   and write `/var/lib/kubeswift-spin/runtime-config.toml` with mode 0600.
   The variable and the `--runtime-config-file=<that path>` argument must
   either both be present or both be absent.
4. When running as root: chown those paths to 65532, clear supplementary
   groups, `setgid(65532)`, `setuid(65532)`, and verify the result.
5. Set `PR_SET_NO_NEW_PRIVS`.
6. Set `HOME`, `TMPDIR`, `XDG_*` under `/var/lib/kubeswift-spin` and
   `PATH=/usr/local/bin`, remove the runtime-config variable, and `exec`
   `/usr/local/bin/spin` with the given arguments.

Because the entrypoint execs Spin, Spin receives termination signals
directly. `spin up` handles SIGINT, SIGTERM and SIGHUP by stopping its
trigger processes; in the Docker test it exits with status 0 within about
100 ms of SIGTERM.

The paths and variable names are defined once in
`internal/runtimecontract` and shared by the controller and the entrypoint.

## Building

```bash
make runtime-image RUNTIME_IMAGE=kubeswift-spin-runtime:dev
```

Without `RUNTIME_IMAGE`, the image is tagged
`ghcr.io/kubeswift-io/kubeswift-spin-runtime:$(cat runtime/VERSION)`.

The Dockerfile builds the entrypoint for `TARGETARCH` and selects the
matching Spin archive and digest, so `docker buildx build --platform
linux/amd64,linux/arm64` works; only amd64 has been built and tested so far.

## Testing without Kubernetes

```bash
make runtime-test RUNTIME_IMAGE=kubeswift-spin-runtime:dev
```

`hack/test-runtime-image.sh` starts a local registry, pushes three example
applications with `spin registry push`, and runs the image as root with the
same entrypoint arguments the controller generates. It checks:

- the expected files exist and no shell, package manager, `curl` or `wget`
  is present; the image user is 65532; image metadata and history contain
  no credential-like values
- `spin --version` reports v4.2.1
- the entrypoint rejects other commands and mismatched runtime-config input
- hello-http is pulled from the registry and serves the expected response
- every process in the container runs as UID 65532
- SIGTERM stops Spin with exit status 0 in under 5 seconds
- a key-value store and an `llm_compute` endpoint configured through
  `KUBESWIFT_SPIN_RUNTIME_CONFIG_B64` work
- the default key-value store works without runtime configuration

The test uses `--insecure` only because its local registry speaks plain
HTTP; the controller never passes `--insecure`. It does not exercise
KubeSwift: there is no microVM, materialization or sandbox networking.

## Updating Spin

1. Read the SHA-256 digests of `spin-<version>-static-linux-amd64.tar.gz`
   and `spin-<version>-static-linux-aarch64.tar.gz` from
   `checksums-<version>.txt` on
   `https://github.com/spinframework/spin/releases/tag/<version>`.
2. Update `SPIN_VERSION`, `SPIN_SHA256_AMD64` and `SPIN_SHA256_ARM64` in
   `runtime/Dockerfile` and the version and digests in
   `hack/install-spin.sh`.
3. Bump `runtime/VERSION` and `runtimeImage.tag` in
   `charts/kubeswift-spin/values.yaml`.
4. Verify the pins and signatures:

   ```bash
   hack/verify-spin-release.sh
   ```

   The script checks that the pinned digests match the release's checksums
   file and verifies the Sigstore signature (`spin.sig`, `crt.pem`) of the
   binary in each archive against the identity of Spin's release workflow
   for that tag. It needs `cosign`.
5. Run `make runtime-test example-test` and update
   [compatibility.md](compatibility.md), and add an upgrade note to
   `CHANGELOG.md`.

Changing the runtime image changes the sandbox spec of every SpinApp that
uses the default, so all replicas are replaced (one at a time) after the
controller is upgraded.
