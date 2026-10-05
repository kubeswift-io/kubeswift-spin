# Example Spin applications

Each directory is a complete Spin application with a `spin.toml`, Rust
source, a `spinapp.yaml`, and a README. The SpinApps use the `kubeswift`
executor (`config/executor/kubeswift.yaml`), except serverless-ai, which
uses `kubeswift-egress` (`config/executor/kubeswift-egress.yaml`) to call
one in-cluster Service, and `outbound-http/spinapp-open.yaml`, which uses
`kubeswift-open`.

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
`ghcr.io/kubeswift-io/kubeswift-spin-examples/<name>:<version>`, where
`<name>` is the application name in `spin.toml` (`mcp-tools` for
experimental/mcp), and each `spinapp.yaml` references the v0.1.0-rc1
artifact. To deploy a published example, from the repository root:

```bash
kubectl -n <namespace> apply -f examples/hello-http/spinapp.yaml
```

To use your own registry, push the example and deploy it with the same
registry and tag (`make example-deploy` rewrites `spec.image`):

```bash
make example-push EXAMPLE=hello-http EXAMPLE_REGISTRY=ghcr.io/<you> EXAMPLE_TAG=v0.1.0-rc1
```

```bash
make example-deploy EXAMPLE=hello-http EXAMPLE_REGISTRY=ghcr.io/<you> EXAMPLE_TAG=v0.1.0-rc1 NAMESPACE=<namespace>
```

`EXAMPLE_REGISTRY` must start with a registry host. Spin pulls the artifact
inside the sandbox, so the registry must be reachable from the guest. A
private registry needs `spec.imagePullSecrets` (see
[docs/executor-contract.md](../docs/executor-contract.md#registry-credentials)).

## What works in a KubeSwift sandbox today

With KubeSwift v0.16.0 each SpinApp is reachable through the Service Spin
Operator creates (`http://<app>.<namespace>.svc`), and becomes `Available`
when its replicas pass the readiness check. The KVM e2e test
([test/e2e](../test/e2e/README.md)) ran on a lab cluster:

| Example | In a KubeSwift v0.16.0 sandbox |
|---|---|
| hello-http | tested: readiness, Service, scaling, rolling update without failed requests, warm-pool checkout, private registry with `imagePullSecrets`, `ingress-from` |
| request-info | tested: two replicas behind the Service, literal and Secret-backed variables |
| serverless-ai | tested against an in-cluster mock: egress allowlist, Secret-backed token, `loadFromSecret` |
| outbound-http | tested in the liveness phase: a restricted sandbox with an `egress-allow` entry fetched from an in-cluster Service, and the replica was replaced while its `/fetch` liveness check failed; `spinapp.yaml` and `spinapp-open.yaml` themselves were not run |
| key-value | not run in a sandbox |
| experimental/mcp | not run in a sandbox |

The HTTP behavior of every example is tested locally with `spin up`
(`make example-test`) and, for hello-http, key-value and serverless-ai, in
the runtime image with Docker (`make runtime-test`). On KubeSwift v0.15.1
the examples run but cannot be reached and report `Available=False` with
reason `NetworkUnavailable`. See [docs/networking.md](../docs/networking.md).
