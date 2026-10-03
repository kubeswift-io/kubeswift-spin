# request-info

Returns a JSON description of the request: method, path, query, an
allowlist of headers (`accept`, `content-type`, `user-agent`,
`x-request-id`), and two application variables. Authorization headers,
cookies and every other header are never echoed.

Used to validate routing, variables and multiple replicas.

## Run locally

```bash
../../bin/spin build
```

```bash
SPIN_VARIABLE_APP_VERSION=1.2.3 ../../bin/spin up --listen 127.0.0.1:3000
```

```bash
curl -s 'http://127.0.0.1:3000/info?x=1' -H 'X-Request-Id: abc' -H 'Authorization: Bearer hidden'
```

The response includes `"app_version":"1.2.3"` and the `x-request-id`
header, and does not include the authorization header.

Spin reads variables from `SPIN_VARIABLE_<NAME>` environment variables.
kubeswift-spin uses the same mechanism: `spec.variables` literal values in
`spinapp.yaml` become `SPIN_VARIABLE_APP_VERSION` and `SPIN_VARIABLE_GREETING`
in the sandbox.

## Deploy

```bash
make example-deploy EXAMPLE=request-info EXAMPLE_REGISTRY=ghcr.io/<you> EXAMPLE_TAG=v0.1.0 NAMESPACE=<namespace>
```

The SpinApp requests two replicas, so kubeswift-spin creates
`request-info-0` and `request-info-1`.

Variables with `valueFrom` (Secret or ConfigMap references) are rejected
with `UnsupportedConfiguration`; see
[docs/compatibility.md](../../docs/compatibility.md).
