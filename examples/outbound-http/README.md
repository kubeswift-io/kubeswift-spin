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
2. The KubeSwift sandbox network mode of the executor.

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

## Expected results by network mode

`spinapp.yaml` targets an in-cluster Service and uses the `kubeswift`
executor; `spinapp-open.yaml` uses `kubeswift-open`.

| Executor network mode | In-cluster target | Public internet target |
|---|---|---|
| `restricted` (default) | fails (cluster addresses blocked) | succeeds |
| `open` | succeeds | succeeds |
| `none` | not available: kubeswift-spin rejects the executor, because Spin downloads the application over the network at start | |

Observing these results in a sandbox requires calling `/fetch`, which is
not possible until KubeSwift exposes sandbox ports (see
[docs/networking.md](../../docs/networking.md)). The table records the
KubeSwift network modes as documented in KubeSwift v0.15.1; it has not been
measured through this example.
