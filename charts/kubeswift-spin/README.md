# kubeswift-spin Helm chart

Installs the kubeswift-spin controller and, optionally, KubeSwift-backed
SpinAppExecutors. It does not install KubeSwift or Spin Operator: their CRDs
must exist first, and the controller exits at startup if they do not.

The released chart is published as an OCI artifact and pins both images by
digest:

```bash
helm install kubeswift-spin oci://ghcr.io/kubeswift-io/charts/kubeswift-spin --version 0.1.0-rc3 --namespace kubeswift-spin-system --create-namespace
```

From a source checkout, at the repository root (the images must exist in
the configured repositories; see the repository README for building them):

```bash
helm install kubeswift-spin charts/kubeswift-spin --namespace kubeswift-spin-system --create-namespace
```

The default values create the executor `kubeswift` in namespace `default`;
see [Executors](#executors) to change that.

## Values

| Value | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/kubeswift-io/kubeswift-spin` | controller image |
| `image.tag` | chart appVersion | controller image tag |
| `image.digest` | `""` (the released chart sets the release digest) | pull by digest (`sha256:...`); overrides the tag |
| `runtimeImage.repository` | `ghcr.io/kubeswift-io/kubeswift-spin-runtime` | runtime rootfs booted by every sandbox |
| `runtimeImage.tag` / `runtimeImage.digest` | `spin-4.2.1-r1` / `""` (the released chart sets the digest) | versioned independently of the controller (`runtime/VERSION`); changing it replaces every replica using it. A digest takes precedence over the tag. Its entrypoint must implement the runtime contract the controller renders (Secret placeholders, secret files): use the runtime image of the same release, or for a source build, one built from the same commit as the controller |
| `imagePullSecrets` | `[]` | pull secrets for the controller image (`- name: <secret>`), not for sandboxes |
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

Other values: `controller.extraArgs`, `serviceAccount.create`,
`serviceAccount.name`, `serviceAccount.annotations`, `podAnnotations`,
`podLabels`, `resources`, `nodeSelector`, `tolerations`, `affinity`,
`priorityClassName`, `metrics.port`, `metrics.service.annotations`,
`metrics.serviceMonitor.interval` and `metrics.serviceMonitor.labels`; see
`values.yaml`. `values.schema.json` rejects unknown keys and invalid enum
values.

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

`sandboxPool` names a SwiftSandboxPool that you create in the same
namespace. Its `image` must equal the controller's runtime image reference
exactly, which the released chart passes by digest
(`ghcr.io/kubeswift-io/kubeswift-spin-runtime@sha256:...`); see
[docs/executor-contract.md](../../docs/executor-contract.md#warm-pools).

`runtimeImagePullSecret` names a docker-registry Secret, in each listed
namespace, that KubeSwift uses to pull the runtime image on the nodes.

Spin Operator adds a finalizer to executors and refuses to delete one while
SpinApps still use it, so `helm uninstall` leaves such executors in a
terminating state until those SpinApps are removed.
