# Runtime image

`ghcr.io/kubeswift-io/kubeswift-spin-runtime` is the OCI image every
kubeswift-spin SwiftSandbox boots as its root filesystem. One image serves
every SpinApp: the application is not baked in, Spin pulls it at start.

## Versioning

The runtime image is versioned independently of the controller. Its tag is
in `runtime/VERSION` (currently `spin-4.2.5-r1`: the Spin version and a
revision for entrypoint or base image changes), and the chart's
`runtimeImage.tag` must match it (`make helm-lint` checks this). The release
workflow builds and pushes the runtime image only when that tag does not
exist yet. Because the image is part of every sandbox spec, keeping its tag
and digest stable across controller releases is what lets a controller
upgrade leave running replicas alone.

## Contents

| Path | Origin |
|---|---|
| `/usr/local/bin/spin` | Spin v4.2.5, `spin-v4.2.5-static-linux-<arch>.tar.gz` from the GitHub release, SHA-256 verified |
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

1. Accept only `up ...` (the controller's arguments), `compile ...` (see
   [Compile mode](#compile-mode)) or `--version`.
2. Create `/var/lib/kubeswift-spin/{state,home,tmp}` with mode 0700.
3. Runtime configuration, at most 64 KiB, written to
   `/var/lib/kubeswift-spin/runtime-config.toml` with mode 0600. It comes
   from exactly one of:
   - `KUBESWIFT_SPIN_RUNTIME_CONFIG_B64` (base64). Option values of the form
     `kubeswift-spin-secret:KUBESWIFT_SPIN_SECRET_<n>` are replaced with the
     value of that environment variable, which KubeSwift filled from a
     Secret; the document is parsed and re-encoded so any value is quoted
     correctly. A missing variable is an error that names the variable,
     not a value.
   - `KUBESWIFT_SPIN_RUNTIME_CONFIG_FILE`, which must be
     `/run/kubeswift-spin/runtime-config.toml`, the secret file KubeSwift
     writes for `loadFromSecret` (root-owned, mode 0400). It is read before
     root is dropped.

   A runtime config and the `--runtime-config-file=<that path>` argument
   must either both be present or both be absent.
4. Registry credentials: when `KUBESWIFT_SPIN_REGISTRY_AUTH_FILES` is set,
   read each listed file (only `/run/kubeswift-spin/registry-auth/*.json`,
   at most 64 KiB each, each a Docker config with an `auths` section),
   merge the `auths` (a later file wins for the same registry), and write
   `/var/lib/kubeswift-spin/home/.docker/config.json` with mode 0600.
5. Compiled cache (ADR 0014): when `KUBESWIFT_SPIN_CACHE_SOURCE`,
   `KUBESWIFT_SPIN_CACHE_RUNTIME` and `KUBESWIFT_SPIN_CACHE_ARCHITECTURE`
   are all set (`up` only), use the cache at `/opt/kubeswift-spin-cache`
   if it is a read-only mount point whose `metadata.json` names that
   identity, this binary's architecture and this image's Wasmtime cache
   version (`runtimecontract.WasmtimeCacheVersion`). Then write
   `/var/lib/kubeswift-spin/wasmtime-cache.toml` and set
   `WASMTIME_CACHE_FILE` for Spin. Otherwise log why and continue.
   `WASMTIME_CACHE_FILE` and `DISABLE_WASMTIME_CACHE` from the environment
   are always dropped. See
   [compiled-cache.md](compiled-cache.md#runtime-mount-contract).
6. When running as root: chown those paths to 65532, clear supplementary
   groups, `setgid(65532)`, `setuid(65532)`, and verify the result.
7. Set `PR_SET_NO_NEW_PRIVS`.
8. Set `HOME`, `TMPDIR`, `XDG_*` under `/var/lib/kubeswift-spin` and
   `PATH=/usr/local/bin`, remove every `KUBESWIFT_SPIN_*` contract variable
   (including the Secret values), and `exec` `/usr/local/bin/spin` with the
   given arguments.

Error messages name variables and paths, never file contents or Secret
values.

Because the entrypoint execs Spin, Spin receives termination signals
directly. `spin up` handles SIGINT, SIGTERM and SIGHUP by stopping its
trigger processes; in the Docker test it exits with status 0 within about
100 ms of SIGTERM.

The paths and variable names are defined once in
`internal/runtimecontract` and shared by the controller and the entrypoint.

## Compile mode

Since `spin-4.2.1-r3` the entrypoint also compiles an application into a
Wasmtime cache for the compiled cache
([compiled-cache.md](compiled-cache.md)). The cache manager runs it in a
compiler SwiftSandbox:

```text
kubeswift-spin-entrypoint compile --source=<repository@sha256:...> \
  --runtime=<runtime repository@sha256:...> --architecture=<amd64|arm64>
```

1. Refuse anything but these three flags, a tag instead of a digest, an
   architecture other than the image's own, and an output directory
   (`/var/lib/kubeswift-spin-output`, the sandbox's KubeSwift output) that
   is missing or not empty.
2. Prepare the guest and registry credentials as in `up` mode; then, as UID
   65532 with `no_new_privs`, `spin registry pull` the application into a
   fresh cache.
3. Read the locked application. Select the components of HTTP triggers
   (other triggers are left out with `--component-id` when present; an
   application without an HTTP trigger fails). Give every required variable
   that has no value in the environment a placeholder value, and write a
   runtime configuration with a `spin` store, a file in the guest's memory
   backed scratch directory, for every key-value and SQLite label other than
   `default`. Neither is part of the compiled code. Component IDs must be
   Spin component IDs.
4. Run `spin up` on `127.0.0.1:3999` with a Wasmtime cache directory of its
   own. Spin compiles every component before it opens the listener, so the
   first TCP connection the listener accepts ends the run: the entrypoint
   sends SIGINT. No request is ever sent to the application, and nothing
   outside the guest can reach the listener. After the pull and after the
   run every process of UID 65532 is killed, so none runs while root reads
   its files.
5. Copy the cache entries (regular files named by Wasmtime's 43-character
   key, owned by UID 65532 with a single link and opened without following
   symbolic links; never statistics or temporary files) to
   `cache/modules/wasmtime-<version>/` in the output directory, and write
   `metadata.json`: source, runtime, architecture, Spin version, Wasmtime
   cache directory, entries and components, with no timestamps. Exit 0.

The directories UID 65532 writes are its own; the work directory that
holds them and every file root writes after Spin ran belong to root, so a
compromised Spin process cannot redirect root's writes. Any failure exits
non-zero, and KubeSwift then exports nothing. The
compiled entries are the ones a normal start of the same application writes
on the same CPU class (the Docker test compares the keys).

## Building

```bash
make runtime-image RUNTIME_IMAGE=kubeswift-spin-runtime:dev
```

Without `RUNTIME_IMAGE`, the image is tagged
`ghcr.io/kubeswift-io/kubeswift-spin-runtime:$(cat runtime/VERSION)`.

The Dockerfile builds the entrypoint for `TARGETARCH` and selects the
matching Spin archive and digest, so `docker buildx build --platform
linux/amd64,linux/arm64` builds both. The release workflow publishes both;
only linux/amd64 has been tested (in Docker and on KVM). The arm64 image
is built and published but not validated; it has never been run.

## Testing without Kubernetes

```bash
make runtime-test RUNTIME_IMAGE=kubeswift-spin-runtime:dev
```

`hack/test-runtime-image.sh` starts a local registry, pushes three example
applications with `spin registry push`, and runs the image as root with the
same entrypoint arguments and environment the controller generates. It
checks:

- the expected files exist and no shell, package manager, `curl` or `wget`
  is present; the image user is 65532; image metadata and history contain
  no credential-like values
- `spin --version` reports v4.2.5
- the entrypoint rejects other commands and mismatched runtime-config input
- hello-http is pulled from the registry and serves the expected response
- every process in the container runs as UID 65532
- SIGTERM stops Spin with exit status 0 in under 5 seconds
- a key-value store and an `llm_compute` endpoint configured through
  `KUBESWIFT_SPIN_RUNTIME_CONFIG_B64` work
- the default key-value store works without runtime configuration
- a Secret-backed runtime-config option: the placeholder is replaced with
  the value of `KUBESWIFT_SPIN_SECRET_0`, and an endpoint that requires a
  token accepts it and rejects a wrong one
- a runtime config delivered as a root-owned 0400 file through
  `KUBESWIFT_SPIN_RUNTIME_CONFIG_FILE` works
- an application pulled from an htpasswd-protected registry with
  credentials merged from Docker config files, and refused without them
- a runtime config without the `--runtime-config-file` flag is rejected

The test uses `--insecure` only because its local registries speak plain
HTTP; the controller never passes `--insecure`. It does not exercise
KubeSwift: there is no microVM, materialization, Secret delivery or sandbox
networking. The KVM path is covered by [test/e2e](../test/e2e/README.md).

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
