# Serverless AI architecture

This document describes how Spin applications on KubeSwift are meant to use
GPU inference. The application side is implemented and tested (the
[serverless-ai](../examples/serverless-ai/) example); the GPU side is a
design that this project has not validated.

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
The SpinApp's runtime configuration tells Spin where to send it:

```yaml
spec:
  runtimeConfig:
    llmCompute:
      type: remote_http
      options:
        - name: url
          value: http://llm.inference.svc.cluster.local:8000
        - name: api_type
          value: open_ai
        - name: auth_token
          value: ""
```

kubeswift-spin renders this into the sandbox runtime-config file. With
`api_type = "open_ai"`, Spin 4.2.1 posts an OpenAI chat-completion request
(`model`, `messages`, `max_completion_tokens`) to `<url>/v1/chat/completions`
and reads `choices[0].message.content` and `usage`. The model name must be
listed in the component's `ai_models` and served under that name by the
inference server.

Constraints in this release:

- The inference Service is a cluster address, so the SpinApp needs an
  `open` executor; `restricted` blocks cluster egress
  ([upstream/kubeswift-sandbox-egress-allowlist.md](upstream/kubeswift-sandbox-egress-allowlist.md)
  proposes a narrower option).
- `auth_token` must be empty: tokens cannot be delivered to a sandbox
  securely yet
  ([upstream/kubeswift-sandbox-secret-projection.md](upstream/kubeswift-sandbox-secret-projection.md)).
  Protect the endpoint with network policy instead.
- `llmCompute.type: spin` (local inference inside the Spin process) is
  rejected; inference belongs in the inference plane.

The example is tested against a mock OpenAI-compatible server
(`examples/tools/upstream`) both under `spin up` and in the runtime image.

## Inference side (design)

The inference server must be reachable through a Kubernetes Service.

**With KubeSwift v0.15.1**, that rules out SwiftSandbox for the server
itself (sandboxes cannot expose ports). A KubeSwift SwiftGuest or
SwiftGuestPool with GPU passthrough can expose ports through a Service
(`SwiftGuest.spec.network.ports`, `SwiftGuestPool` service ports), so the
inference server would run there, for example:

- vLLM with `--served-model-name default-model`,
- llama.cpp server with `--alias default-model`,
- LocalAI with a model configuration named `default-model`.

Refer to the KubeSwift documentation for GPU passthrough and guest port
exposure; this project does not ship manifests for them.

**Once sandbox port exposure exists**
([upstream/kubeswift-sandbox-service-exposure.md](upstream/kubeswift-sandbox-service-exposure.md)),
SwiftSandbox becomes the better fit for the inference plane as well:
KubeSwift already supports GPU sandboxes (`gpuProfileRef` or
`gpuResourceClaim`), read-only model artifacts shared per node
(`spec.model`), and warm GPU pools that hold the model resident
(`SwiftSandboxPool` with `gpuProfileRef` and `model`).

## Why this split

- GPU allocation, model loading and warm capacity are expensive and
  stateful; they belong to the component that owns hardware.
- Spin applications stay small and can be replaced and scaled without
  touching GPUs.
- A compromised application sandbox holds no GPU and no model weights; it
  can only call the inference endpoint its network mode and Spin manifest
  allow.
