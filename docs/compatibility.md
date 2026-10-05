# Compatibility

## Tested versions

Only the combinations below have been exercised. "Tested" names the test
that ran; nothing else is implied.

| Component | Version | How it was tested |
|---|---|---|
| kubeswift-spin | v0.1.0 (unreleased, `main`) | all tests below |
| Spin | v4.2.1 | runtime image (`make runtime-test`), examples (`make example-test`), KVM e2e |
| spin-sdk (Rust, examples) | 7.0.0 | examples built with Rust 1.97.1 |
| Spin Operator | v0.6.1 | Go API and CRDs in envtest; operator installed in the kind integration test (`WITH_SPIN_OPERATOR=1`) and on the KVM e2e cluster |
| KubeSwift | v0.16.0 | KVM e2e on a lab cluster (below); contract test of `internal/sandboxapi` against the v0.16.0 Go types and CRDs; v0.16.0 sandbox CRDs in envtest |
| KubeSwift | v0.15.1 | legacy mode only: unit and envtest tests with a feature detector that reports no optional features. Legacy mode uses only the API subset that was contract-tested against v0.15.1 before the module moved to v0.16.0. Not run on a v0.15.1 cluster since the v0.16.0 integration. |
| cert-manager | v1.21.1 | KVM e2e cluster (the kind integration test pins v1.21.2) |
| Kubernetes API server | 1.34.1, 1.37.0 | envtest controller suite |
| Kubernetes (k0s) | v1.34.3 | KVM e2e cluster |
| Kubernetes (kind) | v1.34.0 | kind integration test |

### KVM e2e lab run (2026-10-05)

Cluster: k0s Kubernetes v1.34.3, three nodes, two of them kernel nodes with
KVM; KubeSwift v0.16.0, cert-manager v1.21.1, Spin Operator v0.6.1.
kubeswift-spin was a development build installed from
`charts/kubeswift-spin`, with controller, runtime and example images built
from the same commit (development tags `v0.1.0-dev.<commit>` and
`spin-4.2.1-r1-dev.<commit>`).

`test/e2e/kvm-e2e.sh` with `E2E_WARM_POOL=1` passed every check:

- hello-http became `Available` 19 to 22 seconds after the SpinApp was
  created, and answered through the SpinApp Service from a client pod.
- Scaling to 2 replicas and back to 1 updated `readyReplicas` and the
  Service endpoints.
- A rolling update under continuous traffic had 0 failed requests out of
  about 470.
- request-info with a Secret-backed variable returned the Secret value; the
  value was in no SwiftSandbox and no ConfigMap in the namespace.
- serverless-ai reached an in-cluster OpenAI-compatible mock through the
  egress allowlist with a Secret-backed token. The same application on a
  restricted executor without the allowlist could not reach the mock (the
  request timed out: packets are dropped).
- A replica checked out from a warm pool was `Available` in 6 to 7 seconds.

See [test/e2e](../test/e2e/README.md) for the phases.

### Not tested

- Secret files in a microVM: private application registries
  (`imagePullSecrets`) and `runtimeConfig.loadFromSecret`. Covered only by
  the Docker runtime test, which pulls from an htpasswd-protected local
  registry with the merged Docker config, refuses the pull without
  credentials, and reads a runtime config from a root-owned 0400 file.
- The `spin.kubeswift.io/ingress-from` annotation on a cluster.
- Replacement of a replica after a liveness probe failure.
- Spin Operator `main` after v0.6.1. Its Go module path changed to
  `github.com/spinframework/spin-operator` and it adds the status fields
  `deploymentName` and `serviceName`; kubeswift-spin builds against the
  v0.6.1 module (`github.com/spinkube/spin-operator`).
- arm64. The runtime image Dockerfile supports it and pins the arm64 Spin
  digest, but no arm64 build has been run.
- The GitHub workflows (CI, release, KVM e2e). None has run.

## Required APIs

The controller checks these at startup and exits with an error naming the
missing API:

- `core.spinkube.dev/v1alpha1`: `spinapps`, `spinapps/status`, `spinappexecutors`
- `sandbox.kubeswift.io/v1alpha1`: `swiftsandboxes`

`sandbox.kubeswift.io/v1alpha1` `swiftsandboxpools` is optional and only
needed by executors that set `spin.kubeswift.io/sandbox-pool`.

The API server must also publish the OpenAPI v3 schema of
`sandbox.kubeswift.io/v1alpha1`: the controller does not start until it has
read the SwiftSandbox features from it (see below).

## KubeSwift features

kubeswift-spin reads the OpenAPI v3 schema of `sandbox.kubeswift.io/v1alpha1`
and uses a SwiftSandbox feature only when its fields are present. All three
features first appear in KubeSwift v0.16.0.

| Feature | Detected when the schema has | Enables |
|---|---|---|
| exposure | `spec.network.ports`, `spec.readinessProbe` and `spec.podMetadata` | reaching replicas through the SpinApp Service, `readyReplicas`, `spec.checks`, `spec.podLabels`, `spin.kubeswift.io/ingress-from` |
| secrets | `spec.secretFiles` (the same release honors `env[].valueFrom.secretKeyRef`) | `secretKeyRef` variables and runtime-config options, `runtimeConfig.loadFromSecret`, `imagePullSecrets` |
| egress | `spec.network.egress` | `spin.kubeswift.io/egress-allow` |

On KubeSwift v0.15.1 none is detected. SpinApps then run without ports,
probes or launcher pod labels, report `Available=False` with reason
`NetworkUnavailable`, and Secret-backed configuration is rejected with
`UnsupportedConfiguration`. Detection is described in
[networking.md](networking.md#feature-detection).

## SpinApp field support

Every field of the v0.6.1 `SpinAppSpec` is classified below. A unit test
(`internal/compatibility`) fails if the upstream type gains a field that is
not classified, and checks that this table matches the code. "KubeSwift
v0.16.0" in a note means the field needs the feature detected above; on
older KubeSwift it is rejected.

A field marked unsupported or not applicable is never ignored: if it is set,
reconciliation stops for that SpinApp, `Progressing` becomes `False` with
reason `UnsupportedConfiguration`, and a Warning Event names the field.
Existing sandboxes keep running.

| Field | Support | Notes |
|---|---|---|
| `executor` | supported | Only executors labelled `spin.kubeswift.io/managed-by=kubeswift-spin` are realized. |
| `image` | supported | Passed to `spin up --from`. Must be a registry reference reachable from the sandbox; a private registry needs `imagePullSecrets`. |
| `replicas` | supported | One SwiftSandbox per replica, named `<app>-<ordinal>`. Bounded by `--max-replicas` (default 20). |
| `resources` | partially supported | `cpu` and `memory` size the microVM (see [executor-contract.md](executor-contract.md#cpu-and-memory)). Any other resource name is rejected. |
| `components` | supported | Passed as `spin up --component-id`, which Spin 4.2.1 marks experimental. |
| `variables` | partially supported | Literal values and `valueFrom.secretKeyRef` (KubeSwift v0.16.0) become `SPIN_VARIABLE_*` environment variables; a Secret is passed as a reference, never as a value. `configMapKeyRef`, `fieldRef` and `resourceFieldRef` are rejected. |
| `runtimeConfig` | partially supported | `keyValueStores`, `sqliteDatabases` and `llmCompute` are rendered into a runtime-config file; options may use `valueFrom.secretKeyRef` (KubeSwift v0.16.0). `loadFromSecret` (KubeSwift v0.16.0) delivers the Secret's `runtime-config.toml` key as a file and cannot be combined with the other fields. `configMapKeyRef`, non-empty literal credential options, URLs with embedded credentials and `llmCompute` type `spin` are rejected. |
| `checks` | supported | Become the sandbox readiness and liveness probes against the Spin HTTP port (KubeSwift v0.16.0). Without a readiness check, a TCP check is used. Only `httpGet` checks exist in SpinKube. On older KubeSwift they are accepted with a Warning Event but not enforced. |
| `imagePullSecrets` | supported | Delivered as secret files to the guest, where Spin pulls the application (KubeSwift v0.16.0). The credentials are readable by the Spin process, not by Wasm components. |
| `enableAutoscaling` | unsupported | SpinApp has no scale subresource and there is no Deployment for an HPA or KEDA to target. |
| `invocationLimits` | partially supported | `memory` maps to `SPIN_MAX_INSTANCE_MEMORY` (bytes), as in Spin Operator. Other keys are rejected. |
| `serviceAccountName` | unsupported | A sandbox guest has no Kubernetes identity. |
| `serviceAnnotations` | supported | Applied by Spin Operator, which still creates the SpinApp Service when `createDeployment` is false. |
| `deploymentAnnotations` | not applicable | No Deployment exists. Spin Operator's webhook also rejects it for this executor type. |
| `podAnnotations` | not applicable | No application pod exists. Spin Operator's webhook also rejects it for this executor type. |
| `podLabels` | supported | Added to the KubeSwift launcher pod (KubeSwift v0.16.0). Keys under `kubeswift.io` or its subdomains and under `core.spinkube.dev` are rejected. |
| `volumes` | unsupported | KubeSwift mounts only Secret files and OCI artifacts into a sandbox, not Kubernetes volumes. |
| `volumeMounts` | unsupported | KubeSwift mounts only Secret files and OCI artifacts into a sandbox, not Kubernetes volumes. |

When exposure is detected, a SpinApp name longer than 52 characters is
rejected: the label `core.spinkube.dev/app.<name>.status` that the SpinApp
Service selects would exceed the 63-character limit of a label name.

## SpinAppExecutor field support

| Field | Handling |
|---|---|
| `createDeployment` | Must be `false`; `true` makes the executor invalid (`ExecutorInvalid`). |
| `deploymentConfig.otel` | Supported: copied to the sandbox as `OTEL_EXPORTER_OTLP_*` environment variables. |
| `deploymentConfig.installDefaultCACerts` | Accepted; the runtime image always contains a CA bundle. |
| `deploymentConfig.caCertSecret` | Rejected: not implemented. KubeSwift v0.16.0 could deliver the Secret as a file, but kubeswift-spin does not install it as a trust anchor yet. |
| `deploymentConfig.runtimeClassName` | Rejected: not applicable to sandboxes. |
| `deploymentConfig.spinImage` | Rejected: use the `spin.kubeswift.io/runtime-image` annotation. |

The profile annotations, including `spin.kubeswift.io/egress-allow` and
`spin.kubeswift.io/ingress-from`, are listed in
[executor-contract.md](executor-contract.md#executor-profiles).

## Known upstream issues

- **Spin Operator v0.6.1 rejects an `httpGet` check without `httpHeaders`.**
  Its defaulting webhook writes `httpHeaders: null`, and its own validation
  then refuses the SpinApp with a message saying that `httpHeaders` must be
  of type array. Set `httpHeaders: []` explicitly:

  ```yaml
  spec:
    checks:
      readiness:
        httpGet:
          path: /healthz
          httpHeaders: []
  ```

## Upgrading upstream versions

- **Spin**: change the version and digests in `runtime/Dockerfile` and
  `hack/install-spin.sh`, then run `make runtime-test example-test`. See
  [runtime-image.md](runtime-image.md#updating-spin).
- **Spin Operator**: bump the Go module, run `make test`. The matrix test
  fails on new `SpinAppSpec` fields until they are classified here and in
  `internal/compatibility`.
- **KubeSwift**: bump the test-only Go module, run `make test` (the
  `internal/sandboxapi` contract test checks the fields kubeswift-spin uses)
  and the KVM e2e test. A new SwiftSandbox feature needs code changes and
  schema detection before it is used.
- **Upgrading KubeSwift on a cluster** from a version without a feature to
  one with it (for example v0.15.1 to v0.16.0) changes the rendered sandbox
  spec, so every replica is replaced once, one at a time. The controller
  re-reads the schema at most every `--capability-refresh` (default 5
  minutes) and renders the new spec at the next reconcile of each SpinApp;
  restarting the controller applies it at once.
