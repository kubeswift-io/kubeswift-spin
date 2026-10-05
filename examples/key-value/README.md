# key-value

Exercises Spin's key-value API (`spin_sdk::key_value::Store`) on the
`default` store.

| Request | Effect |
|---|---|
| `PUT /kv/<key>` (body) | store a value, at most 64 KiB |
| `GET /kv/<key>` | read a value |
| `DELETE /kv/<key>` | delete a value |
| `GET /kv` | list keys as JSON |

## Run locally

```bash
../../bin/spin build
```

```bash
../../bin/spin up --listen 127.0.0.1:3000
```

```bash
curl -X PUT --data-binary hello http://127.0.0.1:3000/kv/greeting
```

```bash
curl http://127.0.0.1:3000/kv/greeting
```

## Runtime configuration in a sandbox

`spinapp.yaml` configures the `default` store through
`spec.runtimeConfig.keyValueStores`. kubeswift-spin renders it into a Spin
runtime-config file inside the sandbox:

```toml
[key_value_store.default]
type = "spin"
path = "/var/lib/kubeswift-spin/state/kv.db"
```

Without runtime configuration, the default store of a registry application
is held in memory by Spin.

## Persistence

There is none. A SwiftSandbox root filesystem is a read-only image with a
memory-backed overlay, so the database file lives in guest RAM. Each replica
has its own store, and data is lost when a sandbox is replaced (scale down,
rollout, failure). KubeSwift scratch disks are raw block devices that the
workload must format and mount itself, which kubeswift-spin does not do. Use
an external store (for example a Redis key-value store type) for shared or
durable data. Its credentials can come from a Secret with
`valueFrom.secretKeyRef` on the option (KubeSwift v0.16.0; see
[docs/executor-contract.md](../../docs/executor-contract.md#runtime-configuration)),
and an in-cluster store needs an egress allowlist entry in the executor.
This has not been tested with an external store.
