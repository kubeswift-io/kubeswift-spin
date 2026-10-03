# runtime

Dockerfile for `ghcr.io/kubeswift-io/kubeswift-spin-runtime`, the root
filesystem booted by every kubeswift-spin SwiftSandbox. Build it from the
repository root:

```bash
make runtime-image RUNTIME_IMAGE=kubeswift-spin-runtime:dev
```

```bash
make runtime-test RUNTIME_IMAGE=kubeswift-spin-runtime:dev
```

Contents, the entrypoint contract and the Spin update procedure are in
[docs/runtime-image.md](../docs/runtime-image.md).
