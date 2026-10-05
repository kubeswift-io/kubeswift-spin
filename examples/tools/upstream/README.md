# upstream

Local stand-in for the services the examples call, so tests never depend on
a third-party endpoint.

| Request | Response |
|---|---|
| `GET /` | `outbound-ok` (target for outbound-http) |
| `POST /v1/chat/completions` | deterministic OpenAI-style completion (for serverless-ai) |

From the repository root:

```bash
go run ./examples/tools/upstream --listen 127.0.0.1:8090
```

With `--require-token <token>` (or the environment variable
`UPSTREAM_REQUIRE_TOKEN`), chat completions require the header
`Authorization: Bearer <token>`; the runtime image test and the KVM e2e
test use this to check that a Secret-backed token arrives.

It is a test fixture, not an inference server.
