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

Requires KubeSwift v0.16.0 or later. `spinapp.yaml` sets
`spec.runtimeConfig.llmCompute`, which kubeswift-spin renders into the
sandbox runtime-config file, and uses the `kubeswift-egress` executor: the
inference Service `llm` in namespace `inference` is a cluster address, which
the `restricted` network mode blocks, and that executor allows exactly that
Service on port 8000. Adjust the URL and the executor's
`spin.kubeswift.io/egress-allow` annotation to your inference Service.

Spin requires an `auth_token` option. `spinapp.yaml` reads it from key
`token` of the Secret `llm-token`. Create the Secret in the SpinApp's
namespace:

```bash
kubectl -n <namespace> create secret generic llm-token --from-literal=token=<token>
```

From the repository root, create the executor and deploy:

```bash
kubectl -n <namespace> apply -f config/executor/kubeswift-egress.yaml
```

```bash
make example-deploy EXAMPLE=serverless-ai EXAMPLE_REGISTRY=ghcr.io/<you> EXAMPLE_TAG=v0.1.0 NAMESPACE=<namespace>
```

The token never appears in the SwiftSandbox: the rendered runtime
configuration holds a placeholder, KubeSwift delivers the value to the
guest, and the runtime entrypoint substitutes it before Spin starts. It is
readable by the Spin process in the guest, not by the Wasm component. If the
endpoint needs no token, replace `valueFrom` with `value: ""`.

Call the application through the SpinApp Service:

```bash
kubectl -n <namespace> run curl --rm -i --restart=Never --image=curlimages/curl:8.16.0 --command -- curl -sS -X POST --data-binary 'What is a microVM?' http://serverless-ai.<namespace>.svc/ask
```

The KVM e2e test ran this setup on a lab cluster against the mock server
as an in-cluster Service, and checked that a restricted executor without
the allowlist entry cannot reach it.

## Pointing at a KubeSwift GPU workload

The intended architecture runs the inference server in a KubeSwift GPU
sandbox or guest and keeps Spin sandboxes GPU-free. See
[docs/serverless-ai.md](../../docs/serverless-ai.md) for the layout. That
document describes a design and ships no inference manifests; the GPU path
has not been validated by this project.
