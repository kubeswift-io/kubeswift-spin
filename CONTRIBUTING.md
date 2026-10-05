# Contributing to kubeswift-spin

## Ground rules

- The user API is the standard SpinKube `SpinApp`. Do not add CRDs without
  an ADR in `docs/adr`.
- Keep Spin semantics out of KubeSwift and KubeSwift internals out of this
  repository. A capability useful without Spin belongs in KubeSwift:
  propose it under `docs/upstream` instead of working around it here
  ([ADR 0002](docs/adr/0002-no-spin-semantics-in-kubeswift.md)).
- Never silently ignore a SpinApp field. Classify it in
  `internal/compatibility` and `docs/compatibility.md`.
- Never put secret values in objects, logs, Events or status messages.
- Never modify KubeSwift launcher pods, NetworkPolicies or other objects
  KubeSwift owns.
- Never import `github.com/kubeswift-io/kubeswift` outside tests: it is
  AGPL-3.0 and kubeswift-spin is Apache-2.0. Declare needed sandbox fields in
  `internal/sandboxapi` ([ADR 0011](docs/adr/0011-apache-license-and-sandbox-api.md));
  `make check-deps` enforces this.

## Development setup

You need Go (version in `go.mod`), Docker, Helm 3, kubectl, kind, Rust via
rustup, and Python 3 with PyYAML (for the chart policy check) and bcrypt
(for the private registry in `make runtime-test`). Pinned
developer tools (golangci-lint, govulncheck, setup-envtest, kubeconform,
actionlint and the Spin CLI) are installed into `bin/` by the Makefile on
first use.

```bash
make help
```

## Before sending a change

```bash
make verify
```

This runs `gofmt`, `go vet`, golangci-lint, actionlint on the workflows, the
prose check, unit tests, the envtest controller suite and the Helm chart
checks. For changes to the
runtime image, the entrypoint or the examples, also run:

```bash
make verify-all
```

For controller changes that affect cluster behavior:

```bash
make kind-test
```

## Tests

| Layer | Where | Command |
|---|---|---|
| Unit | `internal/...`, `cmd/...` | `make test-unit` |
| Controller (envtest, real CRDs) | `internal/controller` | `make test` |
| Manifests | `test/manifests` | `make test` |
| Examples under `spin up` | `hack/test-examples.sh` | `make example-test` |
| Runtime image (Docker) | `hack/test-runtime-image.sh` | `make runtime-test` |
| Integration (kind, no KVM) | `test/integration` | `make kind-test` |
| End to end (KVM) | `test/e2e` | `make e2e` |

Do not describe mocked or kind-based tests as end-to-end tests.

## Writing

Documentation, comments, commit messages and user-facing messages:

- plain technical English; say what is implemented and tested, nothing more
- no emojis, no em dashes (U+2014), no section signs (U+00A7);
  `make check-prose` enforces the last two
- condition and Event messages name fields and objects, never values
- every command in documentation must work as written

## Commits and sign-off

Small, focused commits with a summary line in the imperative mood.

Every commit must carry a `Signed-off-by` trailer certifying the
[Developer Certificate of Origin](https://developercertificate.org/):

```bash
git commit -s
```

CI rejects pull requests with unsigned-off commits. Contributions are
licensed under the Apache License 2.0.

## Reporting security issues

See [SECURITY.md](SECURITY.md). Do not open public issues for
vulnerabilities.
