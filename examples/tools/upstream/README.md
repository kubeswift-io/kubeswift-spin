# upstream

Local stand-in for the services the examples call, so tests never depend on
a third-party endpoint.

| Request | Response |
|---|---|
| `GET /` | `outbound-ok` (target for outbound-http) |
| `POST /v1/chat/completions` | deterministic OpenAI-style completion (for serverless-ai) |

```bash
go run ./examples/tools/upstream --listen 127.0.0.1:8090
```

It is a test fixture, not an inference server.
