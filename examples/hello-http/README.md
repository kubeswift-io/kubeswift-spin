# hello-http

The smallest useful Spin HTTP application and the primary kubeswift-spin
smoke test. Responses are fixed so tests can compare them exactly.

| Path | Response |
|---|---|
| `/`, `/hello` | `200 Hello from Spin on KubeSwift` |
| `/healthz` | `200 ok` |
| anything else | `404 not found` |

## Run locally

From this directory, with the Spin CLI from `make spin`:

```bash
../../bin/spin build
```

```bash
../../bin/spin up --listen 127.0.0.1:3000
```

In another terminal:

```bash
curl -i http://127.0.0.1:3000/hello
```

## Publish

From the repository root:

```bash
make example-push EXAMPLE=hello-http EXAMPLE_REGISTRY=ghcr.io/<you> EXAMPLE_TAG=v0.1.0
```

## Deploy on Kubernetes

Requires KubeSwift, Spin Operator and kubeswift-spin (see the repository
README), a `kubeswift` executor in the namespace, and a Ready SwiftKernel
named `sandbox` in the namespace.

```bash
make example-deploy EXAMPLE=hello-http EXAMPLE_REGISTRY=ghcr.io/<you> EXAMPLE_TAG=v0.1.0 NAMESPACE=<namespace>
```

```bash
kubectl -n <namespace> get spinapp,swiftsandbox
```

`spinapp.yaml` sets a 500m CPU limit, which becomes 1 vCPU, and 256Mi of
guest memory.

## Validate

Confirm that Spin is serving inside the guest:

```bash
swiftctl -n <namespace> sandbox logs hello-http-0
```

The log contains `Serving http://0.0.0.0:3000`. With KubeSwift v0.15.1 the
listener is not reachable from outside the sandbox and the SpinApp reports
`Available=False` (`NetworkUnavailable`). Once KubeSwift exposes sandbox
ports, `curl http://hello-http.<namespace>.svc/hello` through the Service
Spin Operator creates will be the end-to-end check; see
[docs/upstream/kubeswift-sandbox-service-exposure.md](../../docs/upstream/kubeswift-sandbox-service-exposure.md).
