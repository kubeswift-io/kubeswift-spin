# KVM end-to-end test

`kvm-e2e.sh` runs example applications in real KubeSwift microVMs and
reaches them through the SpinApp Service from a client pod, as real clients
do. It is the only test in this repository that exercises execution; the
envtest and kind tests stand in for KubeSwift.

It passed on a lab cluster on 2026-10-05 with KubeSwift v0.16.0 and
`E2E_WARM_POOL=1`; see [Lab result](#lab-result).

## Phases

| Phase | Checks |
|---|---|
| 1 hello-http | SpinApp `hello-e2e` with a `/healthz` readiness check becomes `Available` (`ApplicationReady`) with `readyReplicas` 1; the time to `Available` is printed; HTTP through the Service returns the expected body and a 404 for an unknown route; scale to 2 gives 2 ready replicas and 2 ready Service endpoints; a variable change rolls both replicas to a new revision while a loop in the client pod sends requests, and no request may fail; scale back to 1 leaves one sandbox and one endpoint; deletion removes the sandboxes |
| 2 request-info | SpinApp `info-e2e` with 2 replicas, one literal variable and one from the Secret `e2e-greeting`: both replicas ready; the response contains the Secret value and the literal value and does not echo the `Authorization` header; the Secret value is in no SwiftSandbox and no ConfigMap in the namespace |
| 3 serverless-ai | an OpenAI-compatible mock (`examples/tools/upstream`, requiring a token) runs as Pod and Service `upstream`; executor `kubeswift-e2e-egress` allows that Service on port 8090. SpinApp `ai-e2e` on that executor, with the token from the Secret `e2e-llm-token`, gets a completion; SpinApp `ai-blocked-e2e` on `$EXECUTOR` without the allowlist must not (HTTP 502 or a client timeout) |
| 4 warm pool (`E2E_WARM_POOL=1`) | a SwiftSandboxPool `spin-e2e-warm` with the controller's runtime image, 1 CPU, 512Mi, `restricted` and port `http-app` 3000 gets a warm slot; SpinApp `warm-e2e` on executor `kubeswift-e2e-warm` becomes `Available`, the time is printed, a `CheckedOut` Event exists for `warm-e2e-0`, and HTTP through the Service works |

The script deletes everything it created when it exits, unless `KEEP=1`. On
failure it prints the SpinApps, sandboxes, Services and conditions first.

## Requirements

- A Linux Kubernetes cluster with at least one worker that has `/dev/kvm`
  and the label `kubeswift.io/kernel-node=true`
- KubeSwift v0.16.0 or later, including the sandbox and kernel CRDs. The
  script checks the published sandbox schema and stops on older versions.
- cert-manager and Spin Operator v0.6.1
- kubeswift-spin installed from `charts/kubeswift-spin` (the script looks
  for an available Deployment labelled `app.kubernetes.io/name=kubeswift-spin`)
  with an executor named `kubeswift` (or `$EXECUTOR`) in the test namespace.
  The executor must use network mode `restricted` without an egress
  allowlist, or the phase 3 blocking check fails.
- a Ready SwiftKernel named `sandbox` in the test namespace
- the hello-http, request-info and serverless-ai artifacts pushed under one
  registry prefix the sandboxes can pull from without credentials, for
  example:

```bash
make example-push EXAMPLE=hello-http EXAMPLE_REGISTRY=<registry>/kubeswift-spin-examples EXAMPLE_TAG=<tag>
```

```bash
make example-push EXAMPLE=request-info EXAMPLE_REGISTRY=<registry>/kubeswift-spin-examples EXAMPLE_TAG=<tag>
```

```bash
make example-push EXAMPLE=serverless-ai EXAMPLE_REGISTRY=<registry>/kubeswift-spin-examples EXAMPLE_TAG=<tag>
```

- nodes that can pull `curlimages/curl:8.16.0` and `golang:1.26.8-bookworm`
  (both pinned by digest in the script) for the client and mock pods

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `E2E_NAMESPACE` | `kubeswift-spin-e2e` | namespace with the executor and the SwiftKernel |
| `EXAMPLES` | `ghcr.io/kubeswift-io/kubeswift-spin-examples` | registry prefix of the example artifacts |
| `EXAMPLES_TAG` | `v0.1.0` | tag of the example artifacts; no tag has been published yet, so set it |
| `EXECUTOR` | `kubeswift` | executor for phases 1 to 3 |
| `TIMEOUT` | `300` | seconds to wait for each step |
| `E2E_WARM_POOL` | unset | `1` also runs phase 4 |
| `KEEP` | unset | `1` keeps the test objects afterwards |

## Run

The script uses the current kubeconfig context. From the repository root:

```bash
E2E_NAMESPACE=kubeswift-spin-e2e EXAMPLES=<registry>/kubeswift-spin-examples EXAMPLES_TAG=<tag> E2E_WARM_POOL=1 test/e2e/kvm-e2e.sh
```

`make e2e` runs the same script and passes these variables through.

## Lab result

On 2026-10-05 the script passed every check with `E2E_WARM_POOL=1` on a k0s
Kubernetes v1.34.3 cluster with three nodes (two kernel nodes with KVM),
KubeSwift v0.16.0, cert-manager v1.21.1 and Spin Operator v0.6.1, with a
kubeswift-spin development build from `charts/kubeswift-spin`:

- hello-http `Available` 19 to 22 seconds after creation
- rolling update: 0 failed requests out of about 470
- warm-pool checkout `Available` in 6 to 7 seconds
- the request to the mock without the allowlist timed out

Not covered by this script: secret files (private application registries
through `imagePullSecrets`, `runtimeConfig.loadFromSecret`),
`spin.kubeswift.io/ingress-from`, replacement after a liveness failure, and
arm64.

## CI

`.github/workflows/kvm-e2e.yaml` is meant to run this script on a
self-hosted runner labelled `kvm`, on manual dispatch only. Public GitHub
runners do not provide KubeSwift-capable KVM nodes, so pull requests do not
depend on it. Its inputs are `examples`, `examples_tag` and `namespace`, and
it runs all four phases. The workflow has not been run.
