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
| `runtimeImage.tag` / `runtimeImage.digest` | `spin-4.2.1-r1` / `""` | versioned independently of the controller (`runtime/VERSION`); changing it replaces every replica using it. Its entrypoint must implement the runtime contract the controller renders (Secret placeholders, secret files), so build it from the same commit as the controller. No runtime image has been published yet: point `runtimeImage.repository` and the tag at an image you built |
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
  - name: kubeswift-egress
    namespaces: [team-a]
    egressAllow:
      - service: {name: llm, namespace: inference}
        ports: [{port: 8000}]
    ingressFrom:
      - namespaceSelector:
          matchLabels: {kubernetes.io/metadata.name: frontend}
```

Fields: `networkMode`, `rootfsMode`, `kernelProfile`, `sandboxPool`,
`nodeSelector`, `defaultCPU`, `defaultMemory`, `runtimeImage`,
`runtimeImagePullSecret`, `runtimeImageVerifyKeySecret`, `egressAllow`,
`ingressFrom`, `otel`. The namespaces must exist before installing.

`egressAllow` (1 to 32 rules, each a `service` with `name` and optional
`namespace`, or a `cidr`, with optional `ports`) renders the
`spin.kubeswift.io/egress-allow` annotation and is valid only with network
mode `restricted`. `ingressFrom` (1 to 16 NetworkPolicy peers) renders
`spin.kubeswift.io/ingress-from`. Both are JSON-encoded into the annotation
and need KubeSwift v0.16.0; on older KubeSwift the executor is invalid.

Spin Operator adds a finalizer to executors and refuses to delete one while
SpinApps still use it, so `helm uninstall` leaves such executors in a
terminating state until those SpinApps are removed.
