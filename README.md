# kubeswift-spin

kubeswift-spin is a SpinKube execution backend that runs Spin applications
inside KubeSwift microVM sandboxes. Developers keep using the standard
`SpinApp` API; platform operators choose a KubeSwift-backed
`SpinAppExecutor`, and each replica runs in its own Cloud Hypervisor
microVM, behind both the WebAssembly sandbox and a hardware virtualization
boundary.

- **Spin** owns the application model.
- **SpinKube** owns the Kubernetes application API.
- **kubeswift-spin** owns the adapter.
- **KubeSwift** owns the isolated compute primitive.

kubeswift-spin requires no change to Spin, SpinKube or KubeSwift, and adds
no CRD for applications: the optional compiled cache keeps platform records
in its own namespace ([ADR 0014](docs/adr/0014-compiled-wasmtime-cache.md)). Read [why kubeswift-spin](docs/why-kubeswift-spin.md) for when a
microVM around a Wasm application is worth its cost, and when standard
SpinKube is the better choice.

## Status

The current release is v0.1.1, which adds a configurable drain period for
deleted replicas. It was validated on a KVM lab cluster: Kubernetes 1.34
(k0s) with the Calico CNI on linux/amd64 nodes, Spin Operator v0.6.1 and
KubeSwift v0.16.3 (v0.1.0: the end-to-end test suite, rolling updates,
scaling and deletion under continuous traffic, network isolation during
termination, warm pools) and v0.16.4 (the drain period, rolling updates
under traffic, warm pools). [docs/compatibility.md](docs/compatibility.md)
lists what was tested.

It has been run on that one cluster only. Evaluate it with your own
workloads, and read the [known limitations](#known-limitations), before
relying on it in production.

## Features

- HTTP SpinApps served through the Service that Spin Operator creates;
  `readyReplicas` counts only replicas whose readiness KubeSwift verified.
- Rolling updates that keep `spec.replicas` replicas available, including
  for a single replica, plus scaling, deletion, and replacement of a replica
  whose liveness check fails.
- Fast readiness: a replica without its own readiness check is ready about
  0.1 seconds after Spin first answers.
- Secrets passed by reference, never copied into objects: Secret-backed
  variables and runtime-config options, `runtimeConfig.loadFromSecret`, and
  private application registries through `imagePullSecrets`.
- Egress allowlists, ingress restriction and warm pools, set per executor.
- A configurable drain period for deleted replicas, so that long requests
  in flight can finish ([drain period](docs/executor-contract.md#drain-period)).

## How it works

```
SpinApp  ->  Spin Operator  ->  kubeswift-spin  ->  SwiftSandbox  ->  microVM  ->  spin up
             (executor with                         one per replica    runtime image +
              createDeployment: false)                                 your Spin OCI artifact
```

1. A platform operator creates a SpinAppExecutor labelled
   `spin.kubeswift.io/managed-by: kubeswift-spin` with
   `createDeployment: false`. Its annotations set the executor profile.
2. A developer applies a normal SpinApp that names that executor.
3. kubeswift-spin creates one SwiftSandbox per replica. Each boots the
   `kubeswift-spin-runtime` image, which runs
   `spin up --from=<spec.image>` as an unprivileged user.
4. The Spin Operator Service routes to replicas that pass their readiness
   check, and kubeswift-spin reports the SpinApp's standard conditions.

Details: [architecture](docs/architecture.md) and
[executor contract](docs/executor-contract.md).

## Requirements

- Kubernetes 1.31 or later (the chart's minimum; 1.34 was tested)
- KubeSwift v0.16.4 or later, the recommended baseline (v0.16.3 is the
  minimum). kubeswift-spin relies on its termination order (a deleted
  replica leaves the Service, drains and is then stopped), on its network
  isolation being kept until a replica has stopped, and on a re-created
  sandbox never taking over the previous one's launcher. Older KubeSwift
  releases run with the limitations in
  [compatibility](docs/compatibility.md#kubeswift-features).
- a node labelled `kubeswift.io/kernel-node=true`, and a Ready SwiftKernel
  named `sandbox` in every namespace that runs SpinApps (6.6.14 or later
  for secret files)
- Spin Operator v0.6.1, with cert-manager
- Helm 3.8 or later

kubeswift-spin installs neither KubeSwift nor Spin Operator. At startup it
checks for their APIs and logs which KubeSwift features it detected
(`SwiftSandbox features detected`).

## Install

The images, chart and example artifacts are published on
`ghcr.io/kubeswift-io` and signed with cosign keyless signing
([verification](docs/releasing.md#verifying-a-release)). The chart pins the
controller and runtime images by digest.

The SpinApp namespace (`demo` here) must exist before the chart creates an
executor in it:

```bash
kubectl create namespace demo
```

```bash
helm install kubeswift-spin oci://ghcr.io/kubeswift-io/charts/kubeswift-spin --version 0.1.1 \
  --namespace kubeswift-spin-system --create-namespace \
  --set 'executors[0].name=kubeswift' --set 'executors[0].namespaces={demo}'
```

```bash
kubectl -n kubeswift-spin-system rollout status deployment/kubeswift-spin
```

Chart values: [charts/kubeswift-spin](charts/kubeswift-spin/README.md).

### Upgrade

Upgrade with `--reset-then-reuse-values` (Helm 3.14 or later), which keeps
your values and takes everything else, including the image digests, from
the new chart:

```bash
helm upgrade kubeswift-spin oci://ghcr.io/kubeswift-io/charts/kubeswift-spin --version 0.1.1 \
  --namespace kubeswift-spin-system --reset-then-reuse-values
```

Do not use `--reuse-values`: it also reuses the previous chart's values,
including the controller and runtime image digests that the released chart
pins, so the chart version changes but the controller does not. A plain
`helm upgrade` without your values resets them, including the executors.
With Helm before 3.14, save your values and pass them back:

```bash
helm get values kubeswift-spin --namespace kubeswift-spin-system --output yaml > kubeswift-spin-values.yaml
```

```bash
helm upgrade kubeswift-spin oci://ghcr.io/kubeswift-io/charts/kubeswift-spin --version 0.1.1 \
  --namespace kubeswift-spin-system --values kubeswift-spin-values.yaml
```

The controller logs its version at startup; after the upgrade it must be
the new one:

```bash
kubectl -n kubeswift-spin-system logs deployment/kubeswift-spin | grep 'starting kubeswift-spin'
```
Building from source: [CONTRIBUTING.md](CONTRIBUTING.md#build-from-source).

## Quick start

```bash
kubectl -n demo apply -f https://raw.githubusercontent.com/kubeswift-io/kubeswift-spin/v0.1.1/examples/hello-http/spinapp.yaml
```

```bash
kubectl -n demo wait spinapp/hello-http --for=condition=Available --timeout=300s
```

```bash
kubectl -n demo run curl --rm -i --restart=Never --image=curlimages/curl:8.16.0 --command -- curl -sS http://hello-http.demo.svc/hello
```

The response is `Hello from Spin on KubeSwift`. More examples, with
Secret-backed variables, key-value storage, outbound HTTP, serverless AI
and an experimental MCP server, are in [examples](examples/).

## Known limitations

- Validated on linux/amd64 and one cluster only; arm64 images are
  published but have not been run.
- A deleted replica (rolling update, scale-down, deletion) keeps running
  for its drain period, KubeSwift's 5 seconds unless the SpinApp or
  executor sets `spin.kubeswift.io/drain-period`, and is then stopped; a
  request still running after that is cut.
- Force-deleting a launcher pod (`--force`) bypasses the drain and the
  network isolation kept through it (not tested; see the
  [security model](docs/security-model.md#network-isolation)).
- The first checkout from a newly created warm pool takes about 12 seconds
  to `Available`, against about 3 seconds from an established pool. Create
  pools some time before relying on warm starts.
- Reaching a new replica through its Service also waits for kube-proxy to
  update its rules (in iptables and nftables modes at most once per
  `minSyncPeriod`, 1 second by default). Lowering it is optional cluster
  configuration.
- Startup figures in [performance](docs/performance.md) are lab
  measurements, not guarantees. The Wasmtime compilation cache measured
  there was a diagnostic; kubeswift-spin ships no prebuilt cache, and every
  sandbox starts with an empty one.
- Any SpinApp author in a namespace can make that namespace's Secrets
  readable by their application, and registry credentials and
  Secret-backed values are readable by the Spin process in the guest; see
  the [security model](docs/security-model.md).

## Documentation

- [Why kubeswift-spin](docs/why-kubeswift-spin.md): when it is worth it,
  trust model, costs
- [Architecture](docs/architecture.md) and
  [executor contract](docs/executor-contract.md)
- [Compatibility](docs/compatibility.md): tested versions and SpinApp field
  support
- [Networking](docs/networking.md), [security model](docs/security-model.md),
  [runtime image](docs/runtime-image.md)
- [Startup performance](docs/performance.md),
  [observability](docs/observability.md),
  [troubleshooting](docs/troubleshooting.md)
- [Serverless AI](docs/serverless-ai.md)
- [Releasing](docs/releasing.md): release process and artifact verification
- [Upstream KubeSwift requirements](docs/upstream/README.md) and
  [architecture decisions](docs/adr/README.md)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). `make verify` runs the checks a
change must pass.

## License

Apache License 2.0. See [LICENSE](LICENSE) and
[docs/third-party-licenses.md](docs/third-party-licenses.md).
