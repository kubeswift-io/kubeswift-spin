# Upstream KubeSwift requirements

Generic KubeSwift capabilities that this integration needs and that do not
exist in KubeSwift v0.15.1. Each document is written as a proposal for the
KubeSwift project: none of them is Spin-specific, and none is implemented in
this repository.

| Document | KubeSwift issue | Blocks in kubeswift-spin |
|---|---|---|
| [kubeswift-sandbox-service-exposure.md](kubeswift-sandbox-service-exposure.md) | [#729](https://github.com/kubeswift-io/kubeswift/issues/729) | Reaching any HTTP SpinApp; `Available=True` |
| [kubeswift-sandbox-health-probes.md](kubeswift-sandbox-health-probes.md) | [#729](https://github.com/kubeswift-io/kubeswift/issues/729) | `readyReplicas`, `spec.checks`, readiness-gated rollouts |
| [kubeswift-sandbox-secret-projection.md](kubeswift-sandbox-secret-projection.md) | [#730](https://github.com/kubeswift-io/kubeswift/issues/730) | Secret-backed variables and runtime configuration, `loadFromSecret`, `caCertSecret`, LLM tokens |
| [kubeswift-sandbox-artifact-projection.md](kubeswift-sandbox-artifact-projection.md) | [#731](https://github.com/kubeswift-io/kubeswift/issues/731) | Private application registries (`imagePullSecrets`), network mode `none` |
| [kubeswift-sandbox-egress-allowlist.md](kubeswift-sandbox-egress-allowlist.md) | [#732](https://github.com/kubeswift-io/kubeswift/issues/732) | Calling one in-cluster service without the `open` mode |
| [kubeswift-sandbox-pool-shape-enforcement.md](kubeswift-sandbox-pool-shape-enforcement.md) | [#733](https://github.com/kubeswift-io/kubeswift/issues/733) | Nothing (kubeswift-spin checks the shape itself); protects other pool users |

The first two matter most: without them a SpinApp runs but cannot serve
traffic.
