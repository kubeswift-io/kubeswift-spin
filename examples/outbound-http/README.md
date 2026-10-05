# outbound-http

`GET /fetch` makes one GET request to the `target_url` variable and returns
whether it succeeded:

```json
{"target":"http://127.0.0.1:8090/","ok":true,"status":200,"error":null}
```

A failed request returns HTTP 502 with `"ok":false`.

Two independent controls decide whether the request can leave:

1. Spin's `allowed_outbound_hosts`, set from the `target_origin` variable.
   Spin refuses any other host before a packet is sent.
2. The KubeSwift sandbox network mode of the executor and, under
   `restricted`, its egress allowlist (`spin.kubeswift.io/egress-allow`).

## Run locally

The target is the local fixture in [../tools/upstream](../tools/upstream/),
so the test never depends on a third-party service. From the repository
root:

```bash
go run ./examples/tools/upstream --listen 127.0.0.1:8090
```

In another terminal, from this directory:

```bash
../../bin/spin build
```

```bash
../../bin/spin up --listen 127.0.0.1:3000
```

```bash
curl -s http://127.0.0.1:3000/fetch
```

To see Spin's own allowlist refuse a request, start the app with a
different allowed origin:

```bash
SPIN_VARIABLE_TARGET_ORIGIN=http://127.0.0.1:9999 ../../bin/spin up --listen 127.0.0.1:3000
```

## Expected results by executor profile

`spinapp.yaml` targets the in-cluster Service
`upstream.kubeswift-spin-examples.svc.cluster.local:8090` and uses the
`kubeswift` executor; `spinapp-open.yaml` uses `kubeswift-open`. Both read
`/fetch` through the SpinApp Service on KubeSwift v0.16.0, for example:

```bash
kubectl -n <namespace> run curl --rm -i --restart=Never --image=curlimages/curl:8.16.0 --command -- curl -sS --max-time 30 http://outbound-http.<namespace>.svc/fetch
```

| Executor profile | In-cluster target | Public internet target |
|---|---|---|
| `restricted` (default) | fails: packets are dropped, so the request times out and `/fetch` returns 502 or the client gives up first | succeeds |
| `restricted` with an `egress-allow` entry for the target Service | succeeds | succeeds |
| `open` | succeeds | succeeds |
| `none` | not available: kubeswift-spin rejects the executor, because Spin downloads the application over the network at start | |

For the allowlist case, an executor like
`config/executor/kubeswift-egress.yaml` with this annotation:

```yaml
spin.kubeswift.io/egress-allow: '[{"service":{"name":"upstream","namespace":"kubeswift-spin-examples"},"ports":[{"port":8090}]}]'
```

Only the first two rows were measured, in the KVM e2e test. This
application ran in a restricted sandbox with an `egress-allow` entry for an
in-cluster Service and fetched from it (phase 8, which uses `/fetch` as a
liveness check). The serverless-ai application in a restricted sandbox
without the allowlist could not reach an in-cluster Service (the request
timed out). The
`open` and public internet rows follow the KubeSwift documentation and were
not measured, except that Spin pulled the example applications from
`ghcr.io` under `restricted`.
