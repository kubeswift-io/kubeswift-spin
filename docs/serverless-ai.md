# Serverless AI architecture

This document describes how Spin applications on KubeSwift are meant to use
GPU inference. The application side is implemented and tested (the
[serverless-ai](../examples/serverless-ai/) example), including on a KVM lab
cluster against a mock inference server; the GPU side is a design that this
project has not validated.

## Separation of planes

```
Spin WebAssembly application          application plane
        |  Spin LLM API (spin_sdk::llm)
        v
Spin runtime, llm_compute remote_http
        |  POST /v1/chat/completions
        v
OpenAI-compatible inference Service   inference plane
        |
        v
KubeSwift GPU workload: vLLM, LocalAI or llama.cpp
        |
        v
GPU passthrough
```

| Application plane | Inference plane |
|---|---|
| Spin, Wasm components | KubeSwift GPU lifecycle |
| small artifacts, fast start | model preload, warm GPU pools |
| per-request scaling | expensive, scarce hardware |
| developer-facing API | hardware topology, GPU isolation |

No Spin sandbox needs a GPU, and Spin does not manage GPU passthrough. The
two planes scale and fail independently and are owned by different teams.

## Application side (implemented)

A component calls `spin_sdk::llm::infer_with_options` with a model name.
The SpinApp's runtime configuration tells Spin where to send it, and the
token comes from a Secret:

```yaml
spec:
  executor: kubeswift-egress
  runtimeConfig:
    llmCompute:
      type: remote_http
      options:
        - name: url
          value: http://llm.inference.svc.cluster.local:8000
        - name: api_type
          value: open_ai
        - name: auth_token
          valueFrom:
            secretKeyRef:
              name: llm-token
              key: token
```

kubeswift-spin renders this into the sandbox runtime-config file. The token
is not in the SwiftSandbox: the rendered file holds a placeholder, KubeSwift
v0.16.0 delivers the Secret value to the guest as an environment variable,
and the entrypoint substitutes it before Spin starts (see
[executor-contract.md](executor-contract.md#runtime-configuration)). With
`api_type = "open_ai"`, Spin 4.2.1 posts an OpenAI chat-completion request
(`model`, `messages`, `max_completion_tokens`) to `<url>/v1/chat/completions`
with the token as a bearer token, and reads `choices[0].message.content`
and `usage`. The model name must be listed in the component's `ai_models`
and served under that name by the inference server.

The inference Service is a cluster address, which the `restricted` network
mode blocks. The `kubeswift-egress` executor
(`config/executor/kubeswift-egress.yaml`) keeps `restricted` and allows that
one Service and port:

```yaml
metadata:
  annotations:
    spin.kubeswift.io/egress-allow: '[{"service":{"name":"llm","namespace":"inference"},"ports":[{"port":8000}]}]'
```

An `open` executor also works but removes every egress restriction.

Constraints in this release:

- Secret references and the egress allowlist need KubeSwift v0.16.0.
  Secret files (`loadFromSecret`, `imagePullSecrets`) also need the sandbox
  kernel 6.6.14 or later; this example uses neither.
- The token is readable by the Spin process in the guest, not by the Wasm
  component.
- `llmCompute.type: spin` (local inference inside the Spin process) is
  rejected; inference belongs in the inference plane.

Tests: `make example-test` and `make runtime-test` run the example against
the mock OpenAI-compatible server in `examples/tools/upstream`; the runtime
test also checks that a Secret-backed token is substituted and that a wrong
token is refused by the server. On the KVM lab cluster the e2e test ran the
mock as an in-cluster Service, reached it through the egress allowlist with a
Secret-backed token, and confirmed that the same application on a restricted
executor without the allowlist could not reach it.

## Inference side (design)

The inference server must be reachable through a Kubernetes Service and
serve an OpenAI-compatible API, for example:

- vLLM with `--served-model-name default-model`,
- llama.cpp server with `--alias default-model`,
- LocalAI with a model configuration named `default-model`.

With KubeSwift v0.16.0, a SwiftSandbox can run the inference server and
expose its port (`spec.network.ports`) behind a Service of your own that
selects the launcher pod through `spec.podMetadata`, with a readiness probe
gating its endpoints. KubeSwift also supports GPU sandboxes (`gpuProfileRef`
or `gpuResourceClaim`), read-only model artifacts shared per node
(`spec.model`), and warm GPU pools that hold the model resident
(`SwiftSandboxPool` with `gpuProfileRef` and `model`). A SwiftGuest or
SwiftGuestPool with GPU passthrough and `spec.network.ports` is an
alternative.

This is a design. This project ships no inference manifests and has not run
an inference server in a GPU sandbox; refer to the KubeSwift documentation
for GPU passthrough.

## Why this split

- GPU allocation, model loading and warm capacity are expensive and
  stateful; they belong to the component that owns hardware.
- Spin applications stay small and can be replaced and scaled without
  touching GPUs.
- A compromised application sandbox holds no GPU and no model weights; it
  can only call the inference endpoint its network mode, egress allowlist
  and Spin manifest allow, with the token of its own SpinApp.
