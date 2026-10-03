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

Early, unreleased (v0.1.0 in development). Read this before deploying:

- SpinApps are translated, reconciled, scaled, rolled and deleted. This is
  covered by unit tests, an envtest suite against the real CRDs and a kind
  integration test with Spin Operator installed. The runtime image is tested
  with Docker. Booting that image in a KubeSwift microVM has **not** yet
  been run end to end on KVM for this release
  ([test/e2e](test/e2e/README.md)).
- **HTTP applications are not reachable yet.** KubeSwift v0.15.1 sandboxes
  deny all ingress and cannot expose ports. kubeswift-spin does not work
  around that; SpinApps report `Available=False` with reason
  `NetworkUnavailable` and `readyReplicas: 0`. The KubeSwift feature that
  fixes this is specified in
  [docs/upstream/kubeswift-sandbox-service-exposure.md](docs/upstream/kubeswift-sandbox-service-exposure.md).
- **No secrets.** Secret-backed variables and runtime configuration,
  private application registries and service accounts are rejected with a
  clear condition, because KubeSwift cannot deliver secrets to a sandbox
  securely yet.

Not production-ready. See [docs/compatibility.md](docs/compatibility.md)
for what was tested and the full field support matrix.

## How it works

```
SpinApp  ->  Spin Operator  ->  kubeswift-spin  ->  SwiftSandbox  ->  microVM  ->  spin up
             (executor with                         one per replica    runtime image +
              createDeployment: false)                                 your Spin OCI artifact
```

1. An operator creates a SpinAppExecutor labelled
   `spin.kubeswift.io/managed-by: kubeswift-spin` with
   `createDeployment: false`. Annotations select the sandbox profile
   (network mode, warm pool, kernel, runtime image).
2. A developer applies a normal SpinApp that names that executor.
3. kubeswift-spin creates SwiftSandboxes `<app>-0` to `<app>-N-1`. Each boots
   the generic `kubeswift-spin-runtime` image, whose entrypoint drops root
   and runs `spin up --from=<spec.image>`.
4. kubeswift-spin reports progress in the SpinApp's standard `Available`
   and `Progressing` conditions.

Details: [docs/architecture.md](docs/architecture.md),
[docs/executor-contract.md](docs/executor-contract.md).

## Prerequisites

- Kubernetes 1.31 or later
- KubeSwift v0.15.1 or later with the sandbox CRDs, a node labelled
  `kubeswift.io/kernel-node=true`, and a Ready SwiftKernel named `sandbox` in
  every namespace that runs SpinApps
- Spin Operator v0.6.1 (with cert-manager, as Spin Operator requires)
- Helm 3

kubeswift-spin does not install KubeSwift or Spin Operator. It checks for
their APIs at startup and exits with an actionable message if they are
missing.

## Install

No images or chart have been published yet. Build and push them to a
registry your cluster can pull from (`make` targets are listed by
`make help`). The runtime image has its own version, read from
`runtime/VERSION` (currently `spin-4.2.1-r1`), so that controller upgrades
do not replace running replicas:

```bash
make image runtime-image REGISTRY=<registry> VERSION=v0.1.0-dev
```

```bash
docker push <registry>/kubeswift-spin:v0.1.0-dev
```

```bash
docker push <registry>/kubeswift-spin-runtime:spin-4.2.1-r1
```

KubeSwift pulls the runtime image on the nodes; if the registry is private,
create a docker-registry Secret in each SpinApp namespace and set the
executor's `runtimeImagePullSecret` value. The controller image uses the
chart's `imagePullSecrets` value instead.

Install the chart and create a `kubeswift` executor in namespace `demo`:

```bash
helm install kubeswift-spin charts/kubeswift-spin \
  --namespace kubeswift-spin-system --create-namespace \
  --set image.repository=<registry>/kubeswift-spin --set image.tag=v0.1.0-dev \
  --set runtimeImage.repository=<registry>/kubeswift-spin-runtime \
  --set 'executors[0].name=kubeswift' --set 'executors[0].namespaces={demo}'
```

The namespace must exist before the chart creates executors in it. Chart
values are documented in [charts/kubeswift-spin](charts/kubeswift-spin/README.md).

## Run hello-http

```bash
make spin
```

```bash
make example-push EXAMPLE=hello-http EXAMPLE_REGISTRY=<registry> EXAMPLE_TAG=v0.1.0-dev
```

The artifact must be pullable without credentials from inside the sandbox.

```bash
make example-deploy EXAMPLE=hello-http EXAMPLE_REGISTRY=<registry> EXAMPLE_TAG=v0.1.0-dev NAMESPACE=demo
```

```bash
kubectl -n demo get spinapp,swiftsandbox
```

```bash
swiftctl -n demo sandbox logs hello-http-0
```

The guest console shows `Serving http://0.0.0.0:3000` once Spin is
listening. The SpinApp reports `NetworkUnavailable` until KubeSwift can
expose sandbox ports. More examples, including key-value, outbound HTTP,
serverless AI and an experimental MCP server, are in [examples](examples/).

## Documentation

- [Architecture](docs/architecture.md): design, verified upstream contracts, diagrams
- [Executor contract](docs/executor-contract.md): profiles, translation, replicas, status
- [Compatibility](docs/compatibility.md): tested versions and SpinApp field support
- [Networking](docs/networking.md): why apps are not reachable yet
- [Security model](docs/security-model.md)
- [Runtime image](docs/runtime-image.md)
- [Observability](docs/observability.md)
- [Serverless AI](docs/serverless-ai.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Upstream KubeSwift requirements](docs/upstream/README.md)
- [Architecture decisions](docs/adr/README.md)

## Development

```bash
make verify
```

runs formatting, vet, lint, the workflow lint, the prose check, unit and
envtest tests, and the chart checks. `make verify-all` adds the
vulnerability scan, the example tests and the runtime image tests. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

GNU Affero General Public License v3.0, the license of KubeSwift. See
[LICENSE](LICENSE) and [docs/third-party-licenses.md](docs/third-party-licenses.md).
