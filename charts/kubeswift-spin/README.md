# kubeswift-spin Helm chart

Installs the kubeswift-spin controller and, optionally, KubeSwift-backed
SpinAppExecutors. It does not install KubeSwift or Spin Operator: their CRDs
must exist first, and the controller exits at startup if they do not.

```bash
helm install kubeswift-spin charts/kubeswift-spin --namespace kubeswift-spin-system --create-namespace
```

## Values

| Value | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/kubeswift-io/kubeswift-spin` | controller image |
| `image.tag` | chart appVersion | controller image tag |
| `image.digest` | `""` | pull by digest (`sha256:...`); overrides the tag |
| `runtimeImage.repository` | `ghcr.io/kubeswift-io/kubeswift-spin-runtime` | runtime rootfs booted by every sandbox |
| `runtimeImage.tag` / `runtimeImage.digest` | chart appVersion / `""` | as above |
| `replicaCount` | `1` | controller replicas; leader election is always on |
| `controller.defaultCPU` | `"1"` | CPU when a SpinApp sets none |
| `controller.defaultMemory` | `512Mi` | memory when a SpinApp sets none |
| `controller.minMemory` / `maxMemory` | `256Mi` / `16Gi` | accepted guest memory per replica |
| `controller.maxVCPUs` | `8` | largest vCPU count per replica |
| `controller.maxReplicas` | `20` | largest `spec.replicas` per SpinApp |
| `controller.watchNamespaces` | `[]` | namespaces to watch; empty watches all (RBAC stays cluster-wide) |
| `controller.logLevel` | `info` | `debug`, `info` or `error` |
| `executors` | one `kubeswift` executor in `default` | executors to create; see below |
| `metrics.enabled` | `true` | serve metrics and create the metrics Service |
| `metrics.secure` | `false` | HTTPS with Kubernetes token authentication and authorization |
| `metrics.serviceMonitor.enabled` | `false` | create a Prometheus Operator ServiceMonitor |
| `podSecurityContext`, `securityContext` | hardened | non-root, read-only root filesystem, no capabilities, RuntimeDefault seccomp |

`values.schema.json` rejects unknown keys and invalid enum values.

## Executors

SpinAppExecutor is namespaced, so each entry lists the namespaces to create
it in. Profile settings map to `spin.kubeswift.io/` annotations
([docs/executor-contract.md](../../docs/executor-contract.md#executor-profiles)):

```yaml
executors:
  - name: kubeswift
    namespaces: [team-a, team-b]
  - name: kubeswift-open
    namespaces: [team-a]
    networkMode: open
    otel:
      exporter_otlp_endpoint: http://otel-collector.observability.svc:4318
  - name: kubeswift-warm
    namespaces: [team-a]
    sandboxPool: spin-warm
```

Fields: `networkMode`, `rootfsMode`, `kernelProfile`, `sandboxPool`,
`nodeSelector`, `defaultCPU`, `defaultMemory`, `runtimeImage`,
`runtimeImagePullSecret`, `runtimeImageVerifyKeySecret`, `otel`. The
namespaces must exist before installing.

Spin Operator adds a finalizer to executors and refuses to delete one while
SpinApps still use it, so `helm uninstall` leaves such executors in a
terminating state until those SpinApps are removed.
