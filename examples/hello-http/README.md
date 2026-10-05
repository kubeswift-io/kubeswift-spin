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

Requires KubeSwift v0.16.0 or later, Spin Operator and kubeswift-spin (see
the repository README), a `kubeswift` executor in the namespace, and a Ready
SwiftKernel named `sandbox` in the namespace.

```bash
make example-deploy EXAMPLE=hello-http EXAMPLE_REGISTRY=ghcr.io/<you> EXAMPLE_TAG=v0.1.0 NAMESPACE=<namespace>
```

```bash
kubectl -n <namespace> get spinapp,swiftsandbox
```

`spinapp.yaml` sets a 500m CPU limit, which becomes 1 vCPU, and 256Mi of
guest memory. It has no `spec.checks`, so a replica is ready once Spin
accepts TCP connections on port 3000.

## Validate

Wait until the SpinApp is available:

```bash
kubectl -n <namespace> wait spinapp/hello-http --for=condition=Available --timeout=300s
```

Then call it through the Service Spin Operator created, from a pod in the
cluster:

```bash
kubectl -n <namespace> run curl --rm -i --restart=Never --image=curlimages/curl:8.16.0 --command -- curl -sS http://hello-http.<namespace>.svc/hello
```

The response is `Hello from Spin on KubeSwift`. On the KVM lab cluster the
SpinApp became `Available` 19 to 22 seconds after it was created (see
[docs/compatibility.md](../../docs/compatibility.md#kvm-e2e-lab-run-2026-10-05)).

To use `/healthz` as the readiness check instead of the TCP check, add it to
the SpinApp. `httpHeaders: []` is required by Spin Operator v0.6.1:

```yaml
spec:
  checks:
    readiness:
      httpGet:
        path: /healthz
        httpHeaders: []
```

The guest console shows Spin's output, including
`Serving http://0.0.0.0:3000`:

```bash
swiftctl -n <namespace> sandbox logs hello-http-0
```

With KubeSwift v0.15.1 the listener is not reachable and the SpinApp
reports `Available=False` (`NetworkUnavailable`); see
[docs/networking.md](../../docs/networking.md).
