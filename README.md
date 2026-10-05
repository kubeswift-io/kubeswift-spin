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

Early, unreleased (v0.1.0 in development). Not production-ready. What works
today:

- SpinApps are translated, reconciled, scaled, rolled and deleted. This is
  covered by unit tests, an envtest suite against the real CRDs, a kind
  integration test with Spin Operator installed, and Docker tests of the
  runtime image.
- **The KVM path was validated on one lab cluster** with KubeSwift v0.16.0
  ([test/e2e](test/e2e/README.md)): HTTP SpinApps became `Available` and
  were reached through the SpinApp Service, scaled, rolled without failed
  requests, read a Secret-backed variable and token, called an in-cluster
  service through an egress allowlist, and started from a warm pool.
- **HTTP applications are reachable on KubeSwift v0.16.0** through the
  Service Spin Operator creates; `readyReplicas` follows KubeSwift's
  readiness probes. On KubeSwift v0.15.1, SpinApps run but report
  `Available=False` with reason `NetworkUnavailable`.
- **Secrets are passed by reference.** On KubeSwift v0.16.0, Secret-backed
  variables and runtime-config options (tested on the lab cluster),
  `runtimeConfig.loadFromSecret` and private application registries
  (`imagePullSecrets`; both tested with Docker only) are supported.
  kubeswift-spin never reads a Secret; KubeSwift delivers the values to the
  guest.

Not tested: `loadFromSecret` and private registries in a microVM,
`spin.kubeswift.io/ingress-from`, replacement after a liveness failure,
arm64 and the GitHub workflows. See [docs/compatibility.md](docs/compatibility.md)
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

- Kubernetes 1.31 or later
- KubeSwift v0.16.0 or later with the sandbox CRDs, for exposure, readiness,
  Secrets and egress allowlists. KubeSwift v0.15.1 works in a degraded mode:
  SpinApps run but cannot be reached, and Secret-backed configuration is
  rejected.
- a node labelled `kubeswift.io/kernel-node=true`
- a Ready SwiftKernel named `sandbox` in every namespace that runs SpinApps,
  version 6.6.14 or later for SpinApps that use secret files
  (`runtimeConfig.loadFromSecret`, `imagePullSecrets`)
- Spin Operator v0.6.1 (with cert-manager, as Spin Operator requires)
- Helm 3

kubeswift-spin does not install KubeSwift or Spin Operator. It checks for
their APIs at startup and exits with an actionable message if they are
missing, then reads the SwiftSandbox schema to find out which KubeSwift
features it can use and logs `SwiftSandbox features detected`.

## Install

No images or chart have been published yet. Build and push them to a
registry your cluster can pull from (`make` targets are listed by
`make help`). Build both images from the same commit. The runtime image has
its own version, read from `runtime/VERSION` (currently `spin-4.2.1-r1`), so
that controller upgrades do not replace running replicas:

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

Spin pulls the artifact inside the sandbox. For a private registry, add
`spec.imagePullSecrets` to the SpinApp.

```bash
make example-deploy EXAMPLE=hello-http EXAMPLE_REGISTRY=<registry> EXAMPLE_TAG=v0.1.0-dev NAMESPACE=demo
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

## Documentation

- [Architecture](docs/architecture.md): design, verified upstream contracts, diagrams
- [Executor contract](docs/executor-contract.md): profiles, translation, replicas, status
- [Compatibility](docs/compatibility.md): tested versions and SpinApp field support
- [Networking](docs/networking.md): exposure through the SpinApp Service, egress, feature detection
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

Apache License 2.0. See [LICENSE](LICENSE) and
[docs/third-party-licenses.md](docs/third-party-licenses.md).
