# Third-party licenses

kubeswift-spin is licensed under the GNU Affero General Public License v3.0,
matching KubeSwift, whose Go API it links. This page lists the third-party
components it builds on and ships. The authoritative license texts are in
each component's source.

## Controller and entrypoint (Go)

Direct dependencies, from `go.mod`:

| Module | License |
|---|---|
| github.com/kubeswift-io/kubeswift | AGPL-3.0 |
| github.com/spinkube/spin-operator | Apache-2.0 |
| sigs.k8s.io/controller-runtime | Apache-2.0 |
| k8s.io/api, k8s.io/apimachinery, k8s.io/client-go, k8s.io/utils | Apache-2.0 |
| github.com/google/go-containerregistry | Apache-2.0 |
| github.com/prometheus/client_golang | Apache-2.0 |
| github.com/pelletier/go-toml/v2 | MIT |
| go.uber.org/zap | MIT |
| golang.org/x/sys | BSD-3-Clause |

The full dependency graph, including indirect modules, is in `go.sum`, and
release SBOMs list every component with its license.

## Container images

| Image | Contains | License |
|---|---|---|
| kubeswift-spin | controller binary on `gcr.io/distroless/static-debian12:nonroot` | AGPL-3.0; distroless contents under their Debian package licenses |
| kubeswift-spin-runtime | Spin v4.2.1 static binary | Apache-2.0 WITH LLVM-exception (license file at `/usr/share/doc/spin/LICENSE`) |
| | entrypoint binary | AGPL-3.0 |
| | `gcr.io/distroless/static-debian12:nonroot` base (CA certificates, tzdata, passwd) | Debian package licenses |

## Example applications (Rust)

Pinned in `examples/Cargo.lock`. Main dependencies:

| Crate | License |
|---|---|
| spin-sdk | Apache-2.0 WITH LLVM-exception |
| anyhow | MIT OR Apache-2.0 |
| serde, serde_json | MIT OR Apache-2.0 |
| sha2 | MIT OR Apache-2.0 |

The examples are part of this repository and licensed AGPL-3.0, like the
rest of it.
