# Upstream KubeSwift requirements

Generic KubeSwift capabilities that this integration needed and that did not
exist in KubeSwift v0.15.1. Each document was written as a proposal for the
KubeSwift project; none of them is Spin-specific, and none is implemented in
this repository. KubeSwift v0.16.0 implemented all of them and closed the
issues below.

| Document | KubeSwift issue | KubeSwift v0.16.0 | Used by kubeswift-spin for |
|---|---|---|---|
| [kubeswift-sandbox-service-exposure.md](kubeswift-sandbox-service-exposure.md) | [#729](https://github.com/kubeswift-io/kubeswift/issues/729) | `spec.network.ports`, `spec.network.ingress`, `spec.podMetadata` | reaching HTTP SpinApps through the SpinApp Service; `spin.kubeswift.io/ingress-from`; `spec.podLabels` |
| [kubeswift-sandbox-health-probes.md](kubeswift-sandbox-health-probes.md) | [#729](https://github.com/kubeswift-io/kubeswift/issues/729) | `spec.readinessProbe`, `spec.livenessProbe`, `WorkloadReady` | `readyReplicas`, `spec.checks`, readiness-gated rollouts |
| [kubeswift-sandbox-secret-projection.md](kubeswift-sandbox-secret-projection.md) | [#730](https://github.com/kubeswift-io/kubeswift/issues/730) | `env[].valueFrom.secretKeyRef`, `spec.secretFiles` | Secret-backed variables and runtime configuration, `loadFromSecret`, `imagePullSecrets`, LLM tokens (not `caCertSecret`) |
| [kubeswift-sandbox-artifact-projection.md](kubeswift-sandbox-artifact-projection.md) | [#731](https://github.com/kubeswift-io/kubeswift/issues/731) | `spec.artifacts` | not used: Spin 4.2.1 cannot run an application from a local OCI layout |
| [kubeswift-sandbox-egress-allowlist.md](kubeswift-sandbox-egress-allowlist.md) | [#732](https://github.com/kubeswift-io/kubeswift/issues/732) | `spec.network.egress.allow` | `spin.kubeswift.io/egress-allow`: calling in-cluster services without the `open` mode |
| [kubeswift-sandbox-pool-shape-enforcement.md](kubeswift-sandbox-pool-shape-enforcement.md) | [#733](https://github.com/kubeswift-io/kubeswift/issues/733) | full slot shape compared at checkout | nothing new: kubeswift-spin still checks the shape itself |

kubeswift-spin uses these features only when the installed KubeSwift
serves them ([ADR 0012](../adr/0012-feature-detection.md)). On v0.15.1 it
runs in the degraded mode described in
[compatibility.md](../compatibility.md#kubeswift-features).
