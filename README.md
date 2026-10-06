# kubeswift-spin

kubeswift-spin is a SpinKube execution backend that runs Spin applications
inside KubeSwift microVM sandboxes. Developers keep using the standard
`SpinApp` API; platform operators choose a KubeSwift-backed
`SpinAppExecutor` and each replica runs in its own Cloud Hypervisor microVM,
behind both the WebAssembly sandbox and a hardware virtualization boundary.

- **Spin** owns the application model.
- **SpinKube** owns the Kubernetes application API.
- **kubeswift-spin** owns the adapter.
- **KubeSwift** owns the isolated compute primitive.

kubeswift-spin adds no CRD and requires no change to Spin, SpinKube or
KubeSwift.

## Status

v0.1.0-rc3 is a release candidate. It differs from v0.1.0-rc2 only in a
chart permission for leader-election Events and in CI (the v0.1.0-rc1
release run failed before it finished). It validates the initial
architecture and the core execution path on real KVM hardware and is meant
for evaluation and integration testing while the project builds broader
compatibility and operational experience. It is not production-ready.

Tested on linux/amd64 only, with KubeSwift v0.16.0, Spin Operator v0.6.1
and Kubernetes 1.34 (one k0s lab cluster with Calico). arm64 images are
built and published but not validated.
[docs/compatibility.md](docs/compatibility.md) lists what was tested and the support status of every SpinApp field.

What works on KubeSwift v0.16.0, exercised by the KVM end-to-end test
([test/e2e](test/e2e/README.md)):

- HTTP SpinApps become `Available` and are reached through the Service
  Spin Operator creates; `readyReplicas` follows KubeSwift's readiness
  probes. Scaling, rolling updates without failed requests, and deletion.
- A replica whose liveness check fails is replaced, with a growing backoff
  while it keeps failing.
- Secrets passed by reference, never copied into objects: Secret-backed
  variables and runtime-config options, `runtimeConfig.loadFromSecret`, and
  private application registries through `imagePullSecrets`.
- Egress allowlists (`spin.kubeswift.io/egress-allow`), ingress
  restriction (`spin.kubeswift.io/ingress-from`) and warm pools.

On KubeSwift v0.15.1, SpinApps run but report `Available=False` with reason
`NetworkUnavailable`, and Secret-backed configuration is rejected.

Read [why kubeswift-spin](docs/why-kubeswift-spin.md) for when a microVM
around a Wasm application is worth its cost, and when standard SpinKube is
the better choice.

## How it works

```
SpinApp  ->  Spin Operator  ->  kubeswift-spin  ->  SwiftSandbox  ->  microVM  ->  spin up
             (executor with                         one per replica    runtime image +
              createDeployment: false)                                 your Spin OCI artifact
```

1. An operator creates a SpinAppExecutor labelled
   `spin.kubeswift.io/managed-by: kubeswift-spin` with
   `createDeployment: false`. Annotations select the sandbox profile
   (network mode, egress allowlist, warm pool, kernel, runtime image).
2. A developer applies a normal SpinApp that names that executor.
3. kubeswift-spin creates SwiftSandboxes `<app>-0` to `<app>-N-1`. Each boots
   the generic `kubeswift-spin-runtime` image, whose entrypoint drops root
   and runs `spin up --from=<spec.image>`.
4. On KubeSwift v0.16.0 each sandbox exposes Spin's port and carries the
   label that Spin Operator's SpinApp Service selects, so the Service routes
   to replicas that pass their readiness probe.
5. kubeswift-spin reports progress in the SpinApp's standard `Available`
   and `Progressing` conditions and `readyReplicas`.

Details: [docs/architecture.md](docs/architecture.md),
[docs/executor-contract.md](docs/executor-contract.md).

## Prerequisites

- Kubernetes 1.31 or later (the chart's minimum; only 1.34 was tested on a
  cluster)
- KubeSwift v0.16.0 or later with the sandbox CRDs, for exposure, readiness,
  Secrets and egress allowlists (only v0.16.0 was tested). KubeSwift
  v0.15.1 works in a degraded mode: SpinApps run but cannot be reached, and
  Secret-backed configuration is rejected.
- a node labelled `kubeswift.io/kernel-node=true`
- a Ready SwiftKernel named `sandbox` in every namespace that runs SpinApps,
  version 6.6.14 or later for SpinApps that use secret files
  (`runtimeConfig.loadFromSecret`, `imagePullSecrets`)
- Spin Operator v0.6.1 (with cert-manager, as Spin Operator requires)
- Helm 3.8 or later (OCI chart support)
- optional: `swiftctl`, the KubeSwift CLI, to read a sandbox's guest console

kubeswift-spin does not install KubeSwift or Spin Operator. It checks for
their APIs at startup and exits with an actionable message if they are
missing, then reads the SwiftSandbox schema to find out which KubeSwift
features it can use and logs `SwiftSandbox features detected`.

## Install

The release images, chart and example artifacts are public on
`ghcr.io/kubeswift-io`, signed with cosign keyless signing; see
[docs/releasing.md](docs/releasing.md#verifying-a-release) for
verification. The chart pins the controller and runtime images by digest.

The namespace for SpinApps (`demo` here) must exist before the chart
creates an executor in it, and needs a Ready SwiftKernel named `sandbox`:

```bash
kubectl create namespace demo
```

Install the chart with a `kubeswift` executor in `demo`:

```bash
helm install kubeswift-spin oci://ghcr.io/kubeswift-io/charts/kubeswift-spin --version 0.1.0-rc3 \
  --namespace kubeswift-spin-system --create-namespace \
  --set 'executors[0].name=kubeswift' --set 'executors[0].namespaces={demo}'
```

```bash
kubectl -n kubeswift-spin-system rollout status deployment/kubeswift-spin
```

Chart values are documented in
[charts/kubeswift-spin](charts/kubeswift-spin/README.md).

## Run hello-http

```bash
kubectl -n demo apply -f https://raw.githubusercontent.com/kubeswift-io/kubeswift-spin/v0.1.0-rc3/examples/hello-http/spinapp.yaml
```

```bash
kubectl -n demo wait spinapp/hello-http --for=condition=Available --timeout=300s
```

Call it through the Service Spin Operator created, from a pod in the
cluster:

```bash
kubectl -n demo run curl --rm -i --restart=Never --image=curlimages/curl:8.16.0 --command -- curl -sS http://hello-http.demo.svc/hello
```

The response is `Hello from Spin on KubeSwift`. To see the sandboxes and the
guest console:

```bash
kubectl -n demo get spinapp,swiftsandbox
```

```bash
swiftctl -n demo sandbox logs hello-http-0
```

More examples, including request-info with Secret-backed variables,
key-value, outbound HTTP, serverless AI with an egress allowlist and an
experimental MCP server, are in [examples](examples/).

## Build from source

Build both images from the same commit and push them to a registry your
cluster can pull from (`make help` lists the targets). The runtime image
has its own version, read from `runtime/VERSION` (currently
`spin-4.2.1-r1`), so that controller upgrades do not replace running
replicas:

```bash
make image runtime-image REGISTRY=<registry> VERSION=v0.1.0-dev
```

```bash
docker push <registry>/kubeswift-spin:v0.1.0-dev
```

```bash
docker push <registry>/kubeswift-spin-runtime:spin-4.2.1-r1
```

Then install from the source tree:

```bash
helm install kubeswift-spin charts/kubeswift-spin \
  --namespace kubeswift-spin-system --create-namespace \
  --set image.repository=<registry>/kubeswift-spin --set image.tag=v0.1.0-dev \
  --set runtimeImage.repository=<registry>/kubeswift-spin-runtime \
  --set 'executors[0].name=kubeswift' --set 'executors[0].namespaces={demo}'
```

KubeSwift pulls the runtime image on the nodes; if the registry is private,
create a docker-registry Secret in each SpinApp namespace and set the
executor's `runtimeImagePullSecret` value. The controller image uses the
chart's `imagePullSecrets` value instead. Application artifacts in a
private registry are pulled by Spin inside the guest with the SpinApp's
`spec.imagePullSecrets`; see
[docs/executor-contract.md](docs/executor-contract.md#registry-credentials).
To publish an example to your own registry, see
[examples](examples/README.md).

## Documentation

- [Why kubeswift-spin](docs/why-kubeswift-spin.md): when it is worth it, trust model, costs
- [Architecture](docs/architecture.md): design, verified upstream contracts, diagrams
- [Executor contract](docs/executor-contract.md): profiles, translation, replicas, status
- [Compatibility](docs/compatibility.md): tested versions and SpinApp field support
- [Networking](docs/networking.md): exposure through the SpinApp Service, egress, feature detection
- [Security model](docs/security-model.md)
- [Runtime image](docs/runtime-image.md)
- [Observability](docs/observability.md)
- [Serverless AI](docs/serverless-ai.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Releasing](docs/releasing.md): release process and artifact verification
- [Upstream KubeSwift requirements](docs/upstream/README.md)
- [Architecture decisions](docs/adr/README.md)

## Development

```bash
make verify
```

runs the format check, vet, lint, the workflow lint, the generated-code and
dependency-license checks, the prose check, unit and envtest tests, and the
chart checks. `make verify-all` adds the vulnerability scans (`govulncheck`
and OSV-Scanner, both need network access), the example tests and the
runtime image tests. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and
[docs/third-party-licenses.md](docs/third-party-licenses.md).
