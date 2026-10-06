# Compatibility

## Tested versions

Only the combinations below have been exercised. "Tested" names the test
that ran; nothing else is implied.

| Component | Version | How it was tested |
|---|---|---|
| kubeswift-spin | v0.1.0-rc2 | all tests below; the first KVM run used a development build of the commit before the version change, the second the published v0.1.0-rc2 artifacts |
| Spin | v4.2.1 | runtime image (`make runtime-test`), examples (`make example-test`), KVM e2e |
| spin-sdk (Rust, examples) | 7.0.0 | examples built with Rust 1.97.1 |
| Spin Operator | v0.6.1 | Go API and CRDs in envtest; operator installed in the kind integration test (`WITH_SPIN_OPERATOR=1`) and on the KVM e2e cluster |
| KubeSwift | v0.16.0 | KVM e2e on a lab cluster (below); contract test of `internal/sandboxapi` against the v0.16.0 Go types and CRDs; v0.16.0 sandbox CRDs in envtest |
| KubeSwift | v0.15.1 | legacy mode only: unit and envtest tests with a feature detector that reports no optional features. Legacy mode uses only the API subset that was contract-tested against v0.15.1 before the module moved to v0.16.0. Not run on a v0.15.1 cluster since the v0.16.0 integration. |
| cert-manager | v1.21.1 | KVM e2e cluster (the kind integration test pins v1.21.2) |
| Kubernetes API server | 1.34.1, 1.37.0 | envtest controller suite |
| Kubernetes (k0s) | v1.34.3, Calico CNI, linux/amd64 nodes | KVM e2e cluster |
| Architecture | linux/amd64 | everything above; arm64 images are built and published but not validated (never run) |
| Kubernetes (kind) | v1.34.0 | kind integration test |

### KVM e2e lab run (2026-10-05)

Cluster: k0s Kubernetes v1.34.3, three linux/amd64 nodes, two of them
kernel nodes with KVM, Calico; KubeSwift v0.16.0, cert-manager v1.21.1,
Spin Operator v0.6.1. kubeswift-spin was a development build of commit
4e5f606 (controller `v0.1.0-dev.4e5f606`, runtime
`spin-4.2.1-r1-dev.4e5f606`) installed from `charts/kubeswift-spin`, with
the example artifacts tagged `v0.1.0-dev.49ded6b`.

`test/e2e/kvm-e2e.sh` with all nine phases (`E2E_WARM_POOL=1`,
`E2E_SCRATCH_REGISTRY=ttl.sh`) passed every check:

- hello-http became `Available` 26 seconds after the SpinApp was created
  (19 to 26 seconds over several runs) and answered through the SpinApp
  Service from a client pod. Scaling to 3, 2 and 1 replicas updated
  `readyReplicas` and the Service endpoints.
- A rolling update of two replicas took 104 seconds, with 0 failed
  requests out of 503 sent during it.
- Secret-backed variable, Secret-backed runtime-config option,
  `loadFromSecret` and `imagePullSecrets`: each value reached Spin, and a
  leak scan found none of them outside Secrets (objects, Events, logs; see
  [security-model.md](security-model.md#secrets)).
- serverless-ai reached an in-cluster mock through the egress allowlist; a
  restricted sandbox without the allowlist could not (the request timed
  out).
- A replica checked out from a warm pool was `Available` in 7 seconds (6
  to 7 seconds over several runs).
- Private registry: Spin pulled hello-http from an in-cluster htpasswd
  registry with `imagePullSecrets` (`Available` in 29 seconds); without the
  Secret the sandbox failed and the SpinApp never became ready.
- `ingress-from`: a labelled client was admitted, an unlabelled client got
  no connection, and relabelling the same pod flipped the result.
- Liveness: after the check started failing, the sandbox was `Failed`
  (`LivenessProbeFailed`) within 7 seconds and `readyReplicas` dropped to
  0. kubeswift-spin deleted it 10 seconds after the failure and the next
  failed replacement 20 seconds after its failure (backoff), recreating
  each once the foreground deletion finished (12 to 22 seconds). After
  the target recovered, the replacement was `Available` 48 seconds after
  recovery and stayed unchanged for 90 seconds. The controller logged no
  error.
- Teardown left no SpinApp, sandbox, Service or sandbox NetworkPolicy.

See [test/e2e](../test/e2e/README.md) for the phases.

### v0.1.0-rc2 from published artifacts (2026-10-05)

On the same lab cluster, after removing the previous installation (Helm
release, cluster role and binding, the `kubeswift-spin-system` and test
namespaces, executors and SpinApps; the SwiftKernel was recreated), v0.1.0-rc2
was installed with the README command from
`oci://ghcr.io/kubeswift-io/charts/kubeswift-spin` version `0.1.0-rc2`. The
controller ran the published digest, sandboxes booted the published runtime
digest (`spin-4.2.1-r1`, never used on the cluster before), and Spin pulled
the published example artifacts.

- hello-http applied from the release tag became `Available` in 25 seconds
  and answered through the Service. Scaled to 3 replicas on both kernel
  nodes, each replica answered directly and 9 of 9 requests through the
  Service succeeded; scaling back to 1 and deleting left no sandbox,
  Service or sandbox NetworkPolicy.
- `test/e2e/kvm-e2e.sh` from the tag, with all nine phases and the default
  (published) example artifacts, passed all 75 checks: cold start 19
  seconds, warm pool 6 seconds, rolling update with 0 failed requests out
  of 504, private registry `Available` in 31 seconds, no Secret value found
  outside Secrets, liveness replacement with the same timings as in the
  development-build run above.
- The controller logged no error or warning during the run, except one
  rejected LeaderElection Event at startup (the chart did not allow core
  Events in its namespace; fixed after rc2).

### Not tested

- arm64 (images are built and published but not validated), other CNIs
  than Calico, other Kubernetes distributions, and more than one cluster.
- A Wasm component actively trying to read guest files or credentials; the
  credential boundary rests on Spin's capability model.
- Spin Operator `main` after v0.6.1. Its Go module path changed to
  `github.com/spinframework/spin-operator` and it adds the status fields
  `deploymentName` and `serviceName`; kubeswift-spin builds against the
  v0.6.1 module (`github.com/spinkube/spin-operator`).
- The KVM e2e workflow on a self-hosted runner (none is registered); the
  KVM test was run by hand.
- KubeSwift v0.15.1 on a cluster since the v0.16.0 integration.

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
| `image` | supported | Passed to `spin up --from`. Must be a registry reference that starts with a registry host (write `docker.io/org/app:tag`, not `org/app:tag`) and is reachable from the sandbox; a private registry needs `imagePullSecrets`. |
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
  of type array. The API server rejects the SpinApp at admission, before
  any controller sees it, so kubeswift-spin cannot detect or correct it.
  Set `httpHeaders: []` explicitly:

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
