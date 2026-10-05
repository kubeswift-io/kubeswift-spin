# mcp (experimental)

A minimal Model Context Protocol (MCP) tool server over the Streamable HTTP
transport, implemented directly against the MCP specification in about 150
lines of Rust. It exposes two pure tools:

- `word_count`: counts whitespace-separated words in `text`
- `sha256`: returns the hex SHA-256 digest of `text`

## Why it is experimental

- It implements only the tool subset of MCP: `initialize`, `ping`,
  `tools/list`, `tools/call`, and accepts notifications. There are no
  resources, prompts, sampling, SSE streams or sessions.
- It negotiates protocol revisions `2025-11-25`, `2025-06-18` and
  `2025-03-26`, whose tool messages are the same for this subset. It has
  been tested with curl only, not with an MCP client library.
- The Spin ecosystem has no Spin-maintained MCP SDK. The third-party
  [wasmcp](https://github.com/wasmcp/wasmcp) project builds MCP servers from
  WebAssembly components and is the place to look for a fuller
  implementation; it is not used here to avoid an unversioned dependency.

## Security behavior

- Requests with an `Origin` header are rejected with 403 unless the origin
  is listed in the `allowed_origins` variable. This is the DNS-rebinding
  protection the transport specification requires. Requests without an
  `Origin` header (non-browser clients) are accepted.
- Bodies are limited to 64 KiB and tool input to 32 KiB.
- The component has no outbound network access (`allowed_outbound_hosts`
  is empty).

## Run locally

```bash
../../bin/spin build
```

```bash
../../bin/spin up --listen 127.0.0.1:3000
```

```bash
curl -s http://127.0.0.1:3000/mcp -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

```bash
curl -s http://127.0.0.1:3000/mcp -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sha256","arguments":{"text":"abc"}}}'
```

## Run on Kubernetes

On KubeSwift v0.16.0 the server is reachable through the SpinApp Service
like any HTTP SpinApp, at `http://<app>.<namespace>.svc/mcp`. This example
ships no `spinapp.yaml` and has not been run in a sandbox; a SpinApp for it
looks like the hello-http one with the image changed. Restrict who may call
the tools with the executor annotation `spin.kubeswift.io/ingress-from`
(see [docs/networking.md](../../../docs/networking.md#who-may-connect)).

## Why run tool servers in KubeSwift

Agent tool servers are often third-party or generated code. Running one as
a Spin component gives WebAssembly capability isolation: the component can
reach only the hosts, variables and stores its manifest allows. Running Spin
inside a KubeSwift microVM adds a second, independent boundary: a guest
kernel and a hypervisor between the tool and the node. Neither boundary
removes the need to trust the KubeSwift launcher, the hypervisor or the host
kernel; see [docs/security-model.md](../../../docs/security-model.md).
