# multi-component

One Spin application with three components, each from another example:

| Route | Component | From |
|---|---|---|
| `/...` | `hello` | [hello-http](../hello-http/) |
| `/info/...` | `info` (variables `app_version`, `greeting`) | [request-info](../request-info/) |
| `/kv/...` | `kv` (key-value store `default`) | [key-value](../key-value/) |

Spin compiles every component before it listens, so this application shows
how startup grows with the number of components, and is the test case for
compiled caches with more than one entry
([docs/compiled-cache.md](../../docs/compiled-cache.md)).

## Build and run locally

```bash
make example-build EXAMPLE=multi-component
```

```bash
bin/spin up -f examples/multi-component/spin.toml --listen 127.0.0.1:3000
```

```bash
curl http://127.0.0.1:3000/hello
```

```bash
curl -X PUT --data-binary v1 http://127.0.0.1:3000/kv/a
```

```bash
curl 'http://127.0.0.1:3000/info/x?y=1'
```

## Deploy

Push it and apply [spinapp.yaml](spinapp.yaml) after setting `spec.image`
to the pushed reference:

```bash
make example-push EXAMPLE=multi-component EXAMPLE_REGISTRY=<registry>/kubeswift-spin-examples EXAMPLE_TAG=<tag>
```
