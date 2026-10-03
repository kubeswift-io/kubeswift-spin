# serverless-ai

`POST /ask` sends the request body as a prompt to a language model through
Spin's LLM API (`spin_sdk::llm::infer_with_options`) and returns the
completion as JSON:

```json
{"model":"default-model","text":"...","prompt_tokens":4,"generated_tokens":12}
```

The application does not know where inference runs. Spin's runtime
configuration decides that. With `llm_compute` type `remote_http` and
`api_type = "open_ai"`, Spin 4.2 sends `POST <url>/v1/chat/completions` to
any OpenAI-compatible server. This keeps the endpoint URL out of the
application and lets one artifact target different inference backends.

```
Spin component  --Spin LLM API-->  Spin runtime  --HTTP-->  OpenAI-compatible server
(application plane, Wasm)                                   (inference plane, GPU)
```

## Model name

Spin only lets a component use models listed in `ai_models` in
`spin.toml`. This example lists `default-model`, so the inference server
must serve a model under that name:

- vLLM: `--served-model-name default-model`
- llama.cpp server: `--alias default-model`
- LocalAI: a model configuration with `name: default-model`

## Run locally against the mock endpoint

No GPU is needed. [../tools/upstream](../tools/upstream/) returns
deterministic OpenAI-style completions. From the repository root:

```bash
go run ./examples/tools/upstream --listen 127.0.0.1:8090
```

In another terminal, from this directory:

```bash
../../bin/spin build
```

```bash
../../bin/spin up --listen 127.0.0.1:3000 --runtime-config-file runtime-config.toml
```

```bash
curl -s -X POST --data-binary 'What is a microVM?' http://127.0.0.1:3000/ask
```

## Deploy on Kubernetes

`spinapp.yaml` sets `spec.runtimeConfig.llmCompute`, which kubeswift-spin
renders into the sandbox runtime-config file, and uses the
`kubeswift-open` executor because the inference Service is inside the
cluster and the `restricted` profile blocks cluster egress.

Credentials: Spin requires an `auth_token` option. kubeswift-spin accepts
only an empty value. A non-empty literal would be stored in plain text in the
SwiftSandbox spec, and secret-backed values need a KubeSwift secret
projection feature that does not exist yet
([docs/upstream/kubeswift-sandbox-secret-projection.md](../../docs/upstream/kubeswift-sandbox-secret-projection.md)).
Use an inference endpoint that does not require a token, reachable only
from the cluster.

## Pointing at a KubeSwift GPU workload

The intended architecture runs the inference server in a KubeSwift GPU
sandbox or guest and keeps Spin sandboxes GPU-free. See
[docs/serverless-ai.md](../../docs/serverless-ai.md) for the layout and an
example SwiftSandbox running vLLM. That document describes a design; the
GPU path has not been validated by this project.
