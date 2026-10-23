# KVM end-to-end test

`kvm-e2e.sh` runs example applications in real KubeSwift microVMs and
reaches them through the SpinApp Service from a client pod, as real clients
do. It is the only test in this repository that runs SpinApps in KubeSwift
sandboxes; the envtest and kind tests stand in for KubeSwift, and the
runtime image test runs Spin in Docker.

It has passed all nine phases on the KVM lab cluster; see
[Lab result](#lab-result).

## Phases

| Phase | Checks |
|---|---|
| 1 hello-http | SpinApp `hello-e2e` with a `/healthz` readiness check (`initialDelaySeconds: 1`) becomes `Available` (`ApplicationReady`) with `readyReplicas` 1; the time to `Available` is printed; HTTP through the Service returns the expected body and a 404 for an unknown route; scale to 3 gives 3 ready replicas and 3 ready Service endpoints, scale to 2 removes one; a variable change rolls both replicas to a new revision while a loop in the client pod sends requests, and no request may fail; scale back to 1 leaves one sandbox and one endpoint; deletion removes the sandboxes |
| 2 request-info | SpinApp `info-e2e` with 2 replicas, one literal variable and one from the Secret `e2e-greeting`: both replicas ready; the response contains the Secret value and the literal value and does not echo the `Authorization` header; leak scan for the Secret value |
| 3 serverless-ai | an OpenAI-compatible mock (`examples/tools/upstream`, requiring a token it reads from the Secret `e2e-llm-token`) runs as Pod and Service `upstream`; executor `kubeswift-e2e-egress` allows that Service on port 8090. SpinApp `ai-e2e` on that executor, with the token as a Secret-backed runtime-config option, gets a completion; SpinApp `ai-blocked-e2e` on `$EXECUTOR` without the allowlist must not (HTTP 502 or a client timeout); leak scan for the token |
| 4 warm pool | a SwiftSandboxPool `spin-e2e-warm` with the controller's runtime image, 1 CPU, 512Mi, `restricted` and port `http-app` 3000 gets a warm slot; SpinApp `warm-e2e` on executor `kubeswift-e2e-warm` becomes `Available`, the time is printed, a `CheckedOut` Event exists for `warm-e2e-0`, and HTTP through the Service works |
| 5 loadFromSecret | SpinApp `lfs-e2e` (serverless-ai) with `runtimeConfig.loadFromSecret` naming a Secret whose `runtime-config.toml` holds the mock's URL and token; the sandbox has a `secretFiles` entry for it; a completion succeeds, which needs the token; leak scan for the token |
| 6 private registry | an in-cluster registry with TLS (a CA created for the run) and htpasswd (a password created for the run) refuses anonymous pulls; a Job copies the hello-http artifact into it with the docker-registry Secret `e2e-registry-auth`. The test builds a runtime image that also trusts the run's CA from the configured runtime image and pushes it to `E2E_SCRATCH_REGISTRY`, because Spin speaks only HTTPS to registries and kubeswift-spin has no CA option. SpinApp `private-e2e` with `imagePullSecrets` on executor `kubeswift-e2e-private` (that runtime image, egress to the registry) becomes `Available` and serves HTTP; its sandbox has a `secretFiles` entry for the Secret; SpinApp `private-noauth-e2e` without `imagePullSecrets` fails (`SandboxFailed`) and never becomes ready; leak scan for the password and the base64 `auth` value |
| 7 ingress-from | executor `kubeswift-e2e-ingress` with `spin.kubeswift.io/ingress-from` admitting pods labelled `e2e-role=allowed`; SpinApp `ingress-e2e` becomes `Available`; KubeSwift's NetworkPolicy `ingress-e2e-0-restricted` has that peer; a labelled client gets HTTP 200 through the Service, the unlabelled client gets no connection; the unlabelled client is admitted after it gets the label and blocked again after it loses it |
| 8 liveness | SpinApp `live-e2e` (outbound-http) whose liveness check calls `/fetch`, which returns 502 when its in-cluster target (a busybox httpd) stops serving a file. After the file is removed: the sandbox becomes `Failed` (`LivenessProbeFailed`), `readyReplicas` drops to 0, kubeswift-spin replaces the sandbox, the replacement fails too while the target is broken, the file is restored, the next replacement (after a longer backoff) becomes `Available` and serves requests, one sandbox and one sandbox NetworkPolicy remain, nothing changes for 90 seconds, and the controller logged no error. Timings are printed |
| 9 teardown | deleting the SpinApps removes them, their sandboxes, their Services and the sandbox NetworkPolicies |

The leak scan searches every namespaced object in the test namespace except
Secrets (SpinApps, SwiftSandboxes, Pods and their command lines,
ConfigMaps, Events, NetworkPolicies and every other listable type), the
Events of all namespaces, and the logs of the kubeswift-spin controller,
the pods in `kubeswift-system` and the pods of the test namespace, for the
value and its base64 encodings at every byte alignment. Values are unique
to the run and never printed.

The script deletes everything it created when it exits, unless `KEEP=1`. On
failure it prints the SpinApps, sandboxes, Services and conditions first.

The printed durations are functional indicators, not latency measurements.
Waits for `Available` use `kubectl wait`, which watches the SpinApp; the
other timed steps poll every 0.5 seconds, and untimed steps every 3
seconds. Startup latency is measured with `make perf-startup` (see
[test/perf/startupbench](../perf/startupbench/README.md) and
[docs/performance.md](../../docs/performance.md)).

## Requirements

- A Linux Kubernetes cluster with at least one worker that has `/dev/kvm`
  and the label `kubeswift.io/kernel-node=true`
- KubeSwift v0.16.0 or later, including the sandbox and kernel CRDs. The
  script checks the published sandbox schema and stops on older versions.
- cert-manager and Spin Operator v0.6.1
- kubeswift-spin installed with its Helm chart, released or from
  `charts/kubeswift-spin` (the script looks for an available Deployment
  labelled `app.kubernetes.io/name=kubeswift-spin`)
  with an executor named `kubeswift` (or `$EXECUTOR`) in the test namespace.
  The executor must use network mode `restricted` without an egress
  allowlist, or the phase 3 blocking check fails.
- a Ready SwiftKernel named `sandbox` in the test namespace
- the hello-http, request-info, serverless-ai and outbound-http artifacts
  under one registry prefix the sandboxes can pull from without
  credentials. The defaults (`EXAMPLES`, `EXAMPLES_TAG`) name the artifacts
  published by the v0.1.1 release. To test other builds, push them to
  your registry and set `EXAMPLES` and `EXAMPLES_TAG`, for example:

```bash
make example-push EXAMPLE=hello-http EXAMPLE_REGISTRY=<registry>/kubeswift-spin-examples EXAMPLE_TAG=<tag>
```

```bash
make example-push EXAMPLE=request-info EXAMPLE_REGISTRY=<registry>/kubeswift-spin-examples EXAMPLE_TAG=<tag>
```

```bash
make example-push EXAMPLE=serverless-ai EXAMPLE_REGISTRY=<registry>/kubeswift-spin-examples EXAMPLE_TAG=<tag>
```

```bash
make example-push EXAMPLE=outbound-http EXAMPLE_REGISTRY=<registry>/kubeswift-spin-examples EXAMPLE_TAG=<tag>
```

- nodes that can pull `curlimages/curl:8.16.0`, `golang:1.26.10-bookworm`,
  `registry:2`, `gcr.io/go-containerregistry/crane:v0.22.1` and
  `busybox:1.37.0` (all pinned by digest in the script)
- a Calico or other CNI that enforces NetworkPolicy, for phase 7
- for phase 6: `docker`, `openssl` and Python 3 with `bcrypt` where the
  script runs, and `E2E_SCRATCH_REGISTRY`, a registry that the script can
  push to and the nodes can pull from without credentials. The image pushed
  there is the configured runtime image plus the run's public CA
  certificate; the CA key never leaves the script's temporary directory,
  which is deleted on exit. `ttl.sh`, an anonymous registry whose images
  expire, was used on the lab cluster (the tag `2h` sets the expiry).

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `E2E_NAMESPACE` | `kubeswift-spin-e2e` | namespace with the executor and the SwiftKernel |
| `EXAMPLES` | `ghcr.io/kubeswift-io/kubeswift-spin-examples` | registry prefix of the example artifacts |
| `EXAMPLES_TAG` | `v0.1.1` | tag of the example artifacts |
| `EXECUTOR` | `kubeswift` | executor for phases 1 to 3 |
| `TIMEOUT` | `300` | seconds to wait for each step |
| `E2E_PHASES` | `1 2 3 5 7 8 9` | phases to run; when set, `E2E_WARM_POOL` and `E2E_SCRATCH_REGISTRY` add no phases |
| `E2E_WARM_POOL` | unset | `1` also runs phase 4 |
| `E2E_SCRATCH_REGISTRY` | unset | registry for the phase 6 test runtime image; setting it also runs phase 6, and phase 6 stops the script if it is unset |
| `KUBESWIFT_NAMESPACE` | `kubeswift-system` | namespace of the KubeSwift pods whose logs the leak scan reads |
| `KEEP` | unset | `1` keeps the test objects afterwards |

## Run

The script uses the current kubeconfig context. From the repository root:

```bash
E2E_NAMESPACE=kubeswift-spin-e2e EXAMPLES_TAG=v0.1.1 E2E_WARM_POOL=1 E2E_SCRATCH_REGISTRY=ttl.sh test/e2e/kvm-e2e.sh
```

`make e2e` runs the same script and passes these variables through.

## Lab result

The script passed every check of all nine phases on the KVM lab cluster
described in
[compatibility.md](../../docs/compatibility.md#validation-of-v010), with
KubeSwift v0.16.3 and the v0.1.0 source. Earlier builds also passed it on
KubeSwift v0.16.0, v0.16.1 and v0.16.2. Not covered: arm64.

## CI

`.github/workflows/kvm-e2e.yaml` runs this script on a self-hosted runner
labelled `kvm`, on manual dispatch only. Public GitHub runners do not
provide KubeSwift-capable KVM nodes, so pull requests do not depend on it.
Its inputs are `examples`, `examples_tag`, `namespace`, `phases` and
`scratch_registry`. The workflow has not been run; no such runner is
registered.

The runner holds credentials for a cluster, and in a public repository any
pull request can edit a workflow to request the runner's labels. Before
registering one:

- put it in a runner group restricted to the workflow
  `kubeswift-io/kubeswift-spin/.github/workflows/kvm-e2e.yaml@refs/heads/main`;
- require approval of workflows from all outside collaborators;
- prefer ephemeral (just-in-time) runners;
- give it a kubeconfig limited to the test namespace plus the cluster-wide
  reads the script needs (CRDs, nodes, the OpenAPI schema, Events, the
  logs of the kubeswift-spin and KubeSwift pods).

## Compiled caches

`compiled-cache-e2e.sh` tests trusted compiled Wasmtime caches
([docs/compiled-cache.md](../../docs/compiled-cache.md)) with the released
KubeSwift export, verification and placement features:

| Phase | Checks |
|---|---|
| 1 functional | hello-http, multi-component, request-info, outbound-http, key-value, serverless-ai and the MCP example on the warm pool `kss-cc-e2e`: each SpinApp is `Available` before its cache exists; its record becomes Ready (compiled, exported, signed, placed); the second replica mounts the cache from its prepared node (`Mounted/Cached`); the application answers through its Service |
| 2 boundaries | a tenant (SpinApps only) cannot create or patch records, placements or sandbox status, or read the release namespace's Secrets; no sandbox spec names the signing key or a registry credential, and compiler sandboxes reference no Secret; records forged as a compromised status writer could (an unsigned export, another application's signed artifact, an artifact outside the cache repository and, with `CC_WRONG_KEY_JOB`, an export signed with another key) are refused by KubeSwift's platform verification or not attached at all, and the SpinApp stays `Available` |
| 3 restart | deleting the cache manager and signer pods during a compilation keeps the one compiler (attempt 1), trusts nothing before the export is signed, and ends Ready |
| 4 teardown | the pool's placements go with their consumers |

Requirements, on top of the [KVM e2e requirements](#requirements):

- KubeSwift v0.19.1 or later, and a SwiftKernel `sandbox` in the namespace
  on `kernels/sandbox:6.6.17` or later (outputs and warm mounts);
- kubeswift-spin installed with `compiledCache.enabled=true`, a signing
  key and a registry credential for the signer, and the namespace labelled
  `spin.kubeswift.io/compiled-cache=enabled`;
- the `SwiftArtifactExportPolicy` and `SwiftArtifactVerificationPolicy` of
  [docs/compiled-cache.md](../../docs/compiled-cache.md), with the export
  credential Secret in the KubeSwift namespace;
- the example artifacts pushed to a registry the sandboxes and KubeSwift
  can read, pinned by digest in a file:

```text
hello-http registry.example.com/kubeswift-spin-examples/hello-http@sha256:...
multi-component registry.example.com/kubeswift-spin-examples/multi-component@sha256:...
request-info registry.example.com/kubeswift-spin-examples/request-info@sha256:...
request-info-b registry.example.com/kubeswift-spin-examples/request-info-b@sha256:...
outbound-http registry.example.com/kubeswift-spin-examples/outbound-http@sha256:...
key-value registry.example.com/kubeswift-spin-examples/key-value@sha256:...
serverless-ai registry.example.com/kubeswift-spin-examples/serverless-ai@sha256:...
mcp-tools registry.example.com/kubeswift-spin-examples/mcp-tools@sha256:...
```

`request-info-b` is the request-info artifact pushed under another
repository name, a second identity for phases 2 and 3.

```bash
E2E_CONTEXT=<test cluster context> CC_IMAGES=cc-images.txt test/e2e/compiled-cache-e2e.sh
```

`E2E_CONTEXT` must name the current kubectl context. `CC_WRONG_KEY_JOB`
names a push credential for the cache repository in the test namespace;
create it for the run only and delete it afterwards. Even with `KEEP=1`,
the script restores the signer's replica count and deletes a record whose
status it forged.

Phase 2 scales the signer to zero while it runs and forges record status;
run it only on a test cluster.
