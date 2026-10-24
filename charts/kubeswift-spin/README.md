# kubeswift-spin Helm chart

Installs the kubeswift-spin controller and, optionally, KubeSwift-backed
SpinAppExecutors. It does not install KubeSwift or Spin Operator: their CRDs
must exist first, and the controller exits at startup if they do not.

The released chart is published as an OCI artifact and pins the
controller, runtime and cache signer images by digest:

```bash
helm install kubeswift-spin oci://ghcr.io/kubeswift-io/charts/kubeswift-spin --version 0.2.0 --namespace kubeswift-spin-system --create-namespace
```

From a source checkout, at the repository root (the images must exist in
the configured repositories; see the repository README for building them):

```bash
helm install kubeswift-spin charts/kubeswift-spin --namespace kubeswift-spin-system --create-namespace
```

The default values create the executor `kubeswift` in namespace `default`;
see [Executors](#executors) to change that.

To upgrade, use `helm upgrade --reset-then-reuse-values` (Helm 3.14 or
later), or pass your saved values with `--values`. Do not use
`--reuse-values`: it also reuses the previous chart's pinned image
digests, so the controller, runtime and cache signer images do not
change. See
[Upgrade](../../README.md#upgrade) in the repository README.

## Values

| Value | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/kubeswift-io/kubeswift-spin` | controller image |
| `image.tag` | chart appVersion | controller image tag |
| `image.digest` | `""` (the released chart sets the release digest) | pull by digest (`sha256:...`); overrides the tag |
| `runtimeImage.repository` | `ghcr.io/kubeswift-io/kubeswift-spin-runtime` | runtime rootfs booted by every sandbox |
| `runtimeImage.tag` / `runtimeImage.digest` | `spin-4.2.1-r3` / `""` (the released chart sets the digest) | versioned independently of the controller (`runtime/VERSION`); changing it replaces every replica using it. A digest takes precedence over the tag. Its entrypoint must implement the runtime contract the controller renders (Secret placeholders, secret files): use the runtime image of the same release, or for a source build, one built from the same commit as the controller |
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
| `compiledCache.enabled` | `false` | install the SpinCompiledCache CRD, the cache manager and the cache signer ([docs/compiled-cache.md](../../docs/compiled-cache.md)); needs KubeSwift v0.19.3 or later and `runtimeImage.digest` |
| `compiledCache.repository` | `""` | base repository of compiled caches; each namespace's go to `<repository>/<namespace>/wasmtime-cache` |
| `compiledCache.architecture` | `amd64` | architecture for executors whose node selector does not name `kubernetes.io/arch` |
| `compiledCache.extraRuntimes` | `[]` | runtime images (`repository@sha256:...`), besides `runtimeImage`, that caches are built for |
| `compiledCache.retention` / `maxRecordsPerNamespace` | `24h` / `32` | how long an unused record is kept, and most records per namespace |
| `compiledCache.pullSecretName` | `""` | `kubernetes.io/dockerconfigjson` Secret in every enabled namespace that reads only that namespace's caches; create it before labelling the namespace |
| `compiledCache.fetchTimeout` | `0s` | how long a replica that does not come from a warm pool may wait for its cache (`0s` to `5m`); `0s` gives such replicas no cache |
| `compiledCache.compiler.*` | 1 CPU, `1Gi`, `10m` | compiler sandbox size and timeout, largest export (`maxSize`, `maxEntries`) and concurrency (`maxConcurrent`, `maxConcurrentPerNamespace`) |
| `compiledCache.signer.image.*` | `ghcr.io/kubeswift-io/kubeswift-spin-cache-signer`, chart appVersion | cache signer image (the released chart sets the digest) |
| `compiledCache.signer.key.secretName` / `kmsRef` | `""` | the signing key: a Secret in the release namespace with `cosign.key` and `cosign.password`, or a KMS reference |
| `compiledCache.signer.registryCredentialsSecret` | `""` | `kubernetes.io/dockerconfigjson` Secret in the release namespace that reads exported caches and pushes signatures |

Other values: `controller.extraArgs`, `serviceAccount.create`,
`serviceAccount.name`, `serviceAccount.annotations`, `podAnnotations`,
`podLabels`, `resources`, `nodeSelector`, `tolerations`, `affinity`,
`priorityClassName`, `metrics.port`, `metrics.service.annotations`,
`metrics.serviceMonitor.interval`, `metrics.serviceMonitor.labels`,
`compiledCache.signer.extraEnv`, `compiledCache.signer.serviceAccountAnnotations`,
`compiledCache.signer.logLevel`, `compiledCache.signer.extraArgs`,
`compiledCache.signer.resources` and `compiledCache.manager.*`; see
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
`ingressFrom`, `readinessStartup`, `drainPeriod`, `maxDrainPeriod`, `otel`,
`compiledCache`. The namespaces must exist before installing.

`compiledCache` (`enabled` or `disabled`) renders
`spin.kubeswift.io/compiled-cache`: with `enabled`, the executor's SpinApps
get signed compiled Wasmtime caches when `compiledCache.enabled` is set and
the namespace is labelled; replicas from its `sandboxPool` mount them. See
[docs/compiled-cache.md](../../docs/compiled-cache.md).

`egressAllow` (1 to 32 rules, each a `service` with `name` and optional
`namespace`, or a `cidr`, with optional `ports`) renders the
`spin.kubeswift.io/egress-allow` annotation and is valid only with network
mode `restricted`. `ingressFrom` (1 to 16 NetworkPolicy peers) renders
`spin.kubeswift.io/ingress-from`. Both are JSON-encoded into the annotation
and need KubeSwift v0.16.0; on older KubeSwift the executor is invalid.

`readinessStartup` (`auto`, `always` or `never`) renders
`spin.kubeswift.io/readiness-startup`. With the default `auto`, a replica
of a SpinApp without a readiness check is reported ready about when Spin
first accepts connections (KubeSwift v0.16.2). `always` does the same for
SpinApps that declare an HTTP readiness check, whose `initialDelaySeconds`
then does not delay the first checks; see
[docs/executor-contract.md](../../docs/executor-contract.md#readiness-startup).
On KubeSwift before v0.16.2, `always` makes the executor invalid and
`auto` changes nothing.

`drainPeriod` (a duration with units from `0s` to `10m`, for example `30s`)
renders `spin.kubeswift.io/drain-period`: how long a deleted replica keeps
running after its readiness was withdrawn, so requests in flight can finish,
before KubeSwift stops it. It is the default for the executor's SpinApps; a
SpinApp's own `spin.kubeswift.io/drain-period` annotation overrides it.
Without either, KubeSwift's default applies (5 seconds for exposed
sandboxes) and requests running longer than that are cut when a replica is
replaced. A rolling update of N replicas takes at least N times the drain
period, and values below about 2 seconds leave little margin for kube-proxy
and can fail requests during rolling updates. It needs KubeSwift v0.16.3 or
later; setting or changing it replaces the affected replicas once; see
[docs/executor-contract.md](../../docs/executor-contract.md#drain-period).
`maxDrainPeriod` renders `spin.kubeswift.io/max-drain-period`, the longest
drain period the executor's SpinApps may set (default: KubeSwift's 10
minutes); a SpinApp asking for more is reported `UnsupportedConfiguration`.

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
