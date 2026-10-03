# Example Spin applications

Each directory is a complete Spin application with a `spin.toml`, Rust
source, a `spinapp.yaml` for the `kubeswift` executor, and a README.

| Example | Demonstrates |
|---|---|
| [hello-http](hello-http/) | Minimal HTTP app; the canonical smoke test |
| [request-info](request-info/) | Routing, application variables, multiple replicas |
| [outbound-http](outbound-http/) | Outbound HTTP and sandbox network profiles |
| [key-value](key-value/) | Spin key-value API and runtime configuration |
| [serverless-ai](serverless-ai/) | Spin LLM API against an OpenAI-compatible endpoint |
| [experimental/mcp](experimental/mcp/) | Minimal MCP tool server (experimental) |

All examples share one Cargo workspace (this directory) and a pinned Rust
toolchain (`rust-toolchain.toml`, Rust 1.97.1 with the `wasm32-wasip2`
target; spin-sdk 7 needs Rust 1.94 or newer). They use spin-sdk 7.0.0, the
SDK of the Spin 4.x `http-rust` template.

## Prerequisites

- Rust via rustup (the toolchain file installs the right version and target)
- The pinned Spin CLI: `make spin` installs Spin v4.2.1 into `bin/spin`
- Go, for the local test fixture in [tools/upstream](tools/upstream/)

## Build and test locally

From the repository root:

```bash
make examples
```

```bash
make example-test
```

`make example-test` runs every example under `spin up`, passes variables as
`SPIN_VARIABLE_*` environment variables (as kubeswift-spin does), replaces
external services with `tools/upstream`, and checks each response.

## Publish and deploy

Spin applications are published as Spin OCI artifacts with
`spin registry push`. The release workflow publishes the examples to
`ghcr.io/kubeswift-io/kubeswift-spin-examples/<name>:<version>`. To use your
own registry:

```bash
make example-push EXAMPLE=hello-http EXAMPLE_REGISTRY=ghcr.io/<you> EXAMPLE_TAG=v0.1.0
```

```bash
make example-deploy EXAMPLE=hello-http EXAMPLE_REGISTRY=ghcr.io/<you> EXAMPLE_TAG=v0.1.0 NAMESPACE=<namespace>
```

The artifact must be pullable without credentials from inside the sandbox
(see [docs/executor-contract.md](../docs/executor-contract.md)).

## What works in a KubeSwift sandbox today

With KubeSwift v0.15.1 every example deploys, its sandboxes reach `Running`,
and Spin starts serving inside the guest. The HTTP listener cannot be reached
from outside the sandbox yet, because SwiftSandbox has no inbound port
exposure; each SpinApp therefore reports `Available=False` with reason
`NetworkUnavailable`. The HTTP behavior of each example is tested locally
with `spin up` and in the runtime image with Docker
(`make runtime-test`). See [docs/networking.md](../docs/networking.md).
