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
| 6 private registry | an in-cluster registry with TLS (a CA created for the run) and htpasswd (a password created for the run) refuses anonymous pulls; a Job copies the hello-http artifact into it with the docker-registry Secret `e2e-registry-auth`. The test builds a runtime image that also trusts the run's CA from the configured runtime image and pushes it to `E2E_SCRATCH_REGISTRY`, because Spin speaks only HTTPS to registries and kubeswift-spin has no CA option. With `E2E_PRIVATE_REGISTRY_DIR`, the registry, its CA and the Secret come from [`private-registry.sh`](#a-registry-behind-a-private-ca) instead, and the runtime image is built inside the cluster and pulled from that registry by KubeSwift, which trusts the CA. SpinApp `private-e2e` with `imagePullSecrets` on executor `kubeswift-e2e-private` (that runtime image, egress to the registry) becomes `Available` and serves HTTP; its sandbox has a `secretFiles` entry for the Secret; SpinApp `private-noauth-e2e` without `imagePullSecrets` fails (`SandboxFailed`) and never becomes ready; leak scan for the password and the base64 `auth` value |
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
  published by the v0.2.1 release. To test other builds, push them to
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
- for phase 6, either a registry behind a private CA from
  [`private-registry.sh`](#a-registry-behind-a-private-ca)
  (`E2E_PRIVATE_REGISTRY_DIR`, which also needs `E2E_CONTEXT`), or
  `docker`, `openssl` and Python 3 with `bcrypt` where the
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
| `EXAMPLES_TAG` | `v0.2.1` | tag of the example artifacts |
| `EXECUTOR` | `kubeswift` | executor for phases 1 to 3 |
| `TIMEOUT` | `300` | seconds to wait for each step |
| `E2E_PHASES` | `1 2 3 5 7 8 9` | phases to run; when set, `E2E_WARM_POOL` and `E2E_SCRATCH_REGISTRY` add no phases |
| `E2E_WARM_POOL` | unset | `1` also runs phase 4 |
| `E2E_SCRATCH_REGISTRY` | unset | registry for the phase 6 test runtime image; setting it also runs phase 6, and phase 6 stops the script if neither it nor `E2E_PRIVATE_REGISTRY_DIR` is set |
| `E2E_PRIVATE_REGISTRY_DIR` | unset | state directory of a registry from `private-registry.sh`: phase 6 uses it and builds its runtime image inside the cluster; setting it also runs phase 6 |
| `KUBESWIFT_NAMESPACE` | `kubeswift-system` | namespace of the KubeSwift pods whose logs the leak scan reads |
| `KEEP` | unset | `1` keeps the test objects afterwards |

## Run

The script uses the current kubeconfig context. From the repository root:

```bash
E2E_NAMESPACE=kubeswift-spin-e2e EXAMPLES_TAG=v0.2.1 E2E_WARM_POOL=1 E2E_SCRATCH_REGISTRY=ttl.sh test/e2e/kvm-e2e.sh
```

`make e2e` runs the same script and passes these variables through.

## A registry behind a private CA

`private-registry.sh` starts a registry like an organization's private
one in the test namespace: TLS from a CA created for the run, htpasswd
authentication, the docker-registry Secret `e2e-registry-auth`. It writes
the CA certificate and the credentials to a local state directory (mode
0700) and deletes the CA key once the server certificate is signed. It
pushes nothing from the machine that runs it: `copy` and `runtime` run in
a tool pod. Use it only on a test cluster.

```bash
E2E_CONTEXT=<test cluster context> PRIVATE_REGISTRY_DIR=/tmp/kss-private test/e2e/private-registry.sh up
```

KubeSwift then has to trust the CA: an administrator sets KubeSwift's chart
value `sandboxMaterialize.registryCABundle` to
`$PRIVATE_REGISTRY_DIR/ca.crt` (and, for compiled caches, kubeswift-spin's
`compiledCache.signer.registryCABundle`). Pods created before that, warm
slots included, do not trust it. Afterwards, restore KubeSwift's values and
remove the registry:

```bash
E2E_CONTEXT=<test cluster context> PRIVATE_REGISTRY_DIR=/tmp/kss-private test/e2e/private-registry.sh down
```

`private-registry.sh secret <namespace> <name> [<label>=<value>]` creates
a docker-registry Secret for the registry in another namespace (for
example the export credential, labelled `kubeswift.io/export-credentials=true`,
in the KubeSwift namespace); `down` removes those too.

## Lab result

The script passed every check of all nine phases on the KVM lab cluster
described in
[compatibility.md](../../docs/compatibility.md#validation-of-v010), with
KubeSwift v0.16.3 and the v0.1.0 source. Earlier builds also passed it on
KubeSwift v0.16.0, v0.16.1 and v0.16.2. The v0.2.1-rc1 release artifacts
passed all nine phases on KubeSwift v0.19.3, phase 6 against
`private-registry.sh`, and `compiled-cache-e2e.sh` in private mode (see
[compatibility.md](../../docs/compatibility.md#validation-of-v021-rc1)).
The v0.2.0-rc1 release artifacts
passed phases 1 to 5 and 7 to 9 on KubeSwift v0.19.1, and
`compiled-cache-e2e.sh` (see
[compatibility.md](../../docs/compatibility.md#validation-of-v020-rc1)).
Not covered: arm64.

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
| 2 boundaries | a tenant (SpinApps only) cannot create or patch records, placements or sandbox status, or read the release namespace's Secrets; no sandbox spec names the signing key or a registry credential, and compiler sandboxes reference no Secret but the application's pull Secret; with the signer stopped, a SpinApp whose cache waits for a signature is `Available`; records forged as a compromised status writer could (an unsigned export, another application's signed artifact, an artifact outside the cache repository and, with `CC_WRONG_KEY_JOB`, an export signed with another key) are refused by KubeSwift's platform verification or not attached at all, and the SpinApp stays `Available` |
| 3 restart | deleting the cache manager and signer pods during a compilation keeps the one compiler (attempt 1), trusts nothing before the export is signed, and ends Ready |
| 5 fallback | a replica that boots cold (the pool at `minWarm: 0`) mounts the cache its node already holds (`Mounted/Cached`, `Resolved=True`) and answers; a controller restart replaces no replica; a placement the API server refuses (a quota) leaves the SpinApp `Available` with `CompiledCache=True/PlacementFailed`; with the cache manager stopped and the record claiming a prepared pool, a warm and a cold replica carry a cache no node holds, KubeSwift skips it (`NotOnNode`, ...), the replicas become ready, the warm one is not replaced and the SpinApp starts no `DeliveryFailed` hold; with `CC_CACHE_PULL_SECRET`, a missing cache pull Secret (`PullSecretInvalid`) and one the registry refuses: skipped, ready, no anonymous pull |
| 4 teardown | the pool's placements go with their consumers (runs last) |

Requirements, on top of the [KVM e2e requirements](#requirements):

- KubeSwift v0.19.3 or later (v0.19.1 for kubeswift-spin v0.2.0), and a
  SwiftKernel `sandbox` in the namespace
  on `kernels/sandbox:6.6.17` or later (outputs and warm mounts);
- kubeswift-spin installed with `compiledCache.enabled=true`, a signing
  key and a registry credential for the signer, and the namespace labelled
  `spin.kubeswift.io/compiled-cache=enabled`;
- the `SwiftArtifactExportPolicy` and `SwiftArtifactVerificationPolicy` of
  [docs/compiled-cache.md](../../docs/compiled-cache.md), with the export
  credential Secret in the KubeSwift namespace;
- the example artifacts pushed to a registry the sandboxes and KubeSwift
  can read, pinned by digest in a file (with `private-registry.sh copy`
  for a registry behind a private CA):

```text
hello-http registry.example.com/kubeswift-spin-examples/hello-http@sha256:...
multi-component registry.example.com/kubeswift-spin-examples/multi-component@sha256:...
request-info registry.example.com/kubeswift-spin-examples/request-info@sha256:...
request-info-b registry.example.com/kubeswift-spin-examples/request-info-b@sha256:...
hello-http-b registry.example.com/kubeswift-spin-examples/hello-http-b@sha256:...
outbound-http registry.example.com/kubeswift-spin-examples/outbound-http@sha256:...
key-value registry.example.com/kubeswift-spin-examples/key-value@sha256:...
serverless-ai registry.example.com/kubeswift-spin-examples/serverless-ai@sha256:...
mcp-tools registry.example.com/kubeswift-spin-examples/mcp-tools@sha256:...
```

`request-info-b` is the request-info artifact pushed under another
repository name, a second identity for phases 2 and 3; `hello-http-b`
likewise, a cache identity that phase 5 compiles but never places.

```bash
E2E_CONTEXT=<test cluster context> CC_IMAGES=cc-images.txt test/e2e/compiled-cache-e2e.sh
```

For a registry behind a private CA, start it with `private-registry.sh`,
make KubeSwift and the signer trust its CA, copy the examples and a runtime
image into it (the runtime image listed in `compiledCache.extraRuntimes`),
create the cache pull Secret named by `compiledCache.pullSecretName` with
`private-registry.sh secret`, and run:

```bash
E2E_CONTEXT=<test cluster context> CC_IMAGES=cc-images.txt CC_PRIVATE_REGISTRY_DIR=/tmp/kss-private CC_RUNTIME_IMAGE=<runtime in the registry> CC_CACHE_PULL_SECRET=<pull Secret> test/e2e/compiled-cache-e2e.sh
```

In that mode the pool, the executor and every SpinApp pull with
`e2e-registry-auth` and reach the registry through the egress allowlist,
and phase 2 also checks that the registry password is in no sandbox spec
or Event. `E2E_CONTEXT` must name the current kubectl context. `CC_WRONG_KEY_JOB`
names a push credential for the cache repository in the test namespace;
create it for the run only and delete it afterwards. Even with `KEEP=1`,
the script restores the signer's and the cache manager's replica counts,
removes its quota, restores the cache pull Secret and deletes a record
whose status it forged.

Phase 2 scales the signer to zero while it runs and forges record status,
and phase 5 stops the cache manager and forges a record's placements; run
them only on a test cluster.

## Local applications

`local-application-e2e.sh` tests local applications
([docs/local-application.md](../../docs/local-application.md)): warm
replicas that start Spin from a KubeSwift-mounted, verified copy of their
own application artifact.

| Phase | Checks |
|---|---|
| 1 functional | hello-http, multi-component, request-info, outbound-http, key-value, serverless-ai and the MCP example on the warm pool `kss-la-e2e`: the SpinApp reports `LocalApplication=True/Attached` once its placement is prepared; its second replica carries the `application` artifact, KubeSwift mounts it (state `Mounted`), the guest console shows `local application used from /opt/kubeswift-spin-app`, and the application answers through its Service |
| 2 offline | (private registry) with the application placed, the registry's Service is pointed at no pod; a fresh warm replica mounts the application, uses it and serves; the Service is restored |
| 3 binding | (private registry) the same digest under another repository, without a pull Secret: the placement fails (`PlacementFailed`) and the replica carries no application, although the nodes hold the digest |
| 4 credentials | (private registry) with the pull Secret replaced by an invalid credential, and then with the Secret removed, a new replica does not mount the application; the Secret is restored |
| 5 fallback | a tag reference (`ImageNotPinned`) and a component selection (`ComponentsSelected`) become ready from the registry (the tag SpinApp's replica is checked to carry no application); a private-range IP registry, which KubeSwift would reach over plain HTTP, is `RegistryNotEligible` and never placed |

Requirements, on top of the [KVM e2e requirements](#requirements):

- KubeSwift v0.20.0 or later (the script reads the `console` container,
  `sandboxMaterialize.console=true`, the default) with its node artifact
  cache bounded (`sandboxMaterialize.artifactCache.maxBytes` or
  `maxPercent`), and a SwiftKernel `sandbox` in the namespace at
  `kernels/sandbox:6.6.18` or later (live warm-checkout console output)
  with warm mounts;
- kubeswift-spin installed with `localApplication.enabled=true`,
  `localApplication.nodeCacheBounded=true` and `runtimeImage.digest` set,
  and the namespace labelled `spin.kubeswift.io/local-application=enabled`;
- the example artifacts pinned by digest in a file, plus hello-http by tag:

```text
hello-http ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http@sha256:...
multi-component ghcr.io/kubeswift-io/kubeswift-spin-examples/multi-component@sha256:...
request-info ghcr.io/kubeswift-io/kubeswift-spin-examples/request-info@sha256:...
outbound-http ghcr.io/kubeswift-io/kubeswift-spin-examples/outbound-http@sha256:...
key-value ghcr.io/kubeswift-io/kubeswift-spin-examples/key-value@sha256:...
serverless-ai ghcr.io/kubeswift-io/kubeswift-spin-examples/serverless-ai@sha256:...
mcp-tools ghcr.io/kubeswift-io/kubeswift-spin-examples/mcp-tools@sha256:...
hello-http-tag ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http:<tag>
```

```bash
E2E_CONTEXT=<test cluster context> LA_IMAGES=la-images.txt test/e2e/local-application-e2e.sh
```

`E2E_NAMESPACE` (default `kubeswift-spin-e2e`), `E2E_PHASES` (default
`1 2 3 4 5`) and `TIMEOUT` (seconds per step, default 600) are optional.

Without `LA_PRIVATE_REGISTRY_DIR`, phases 2 to 4 are skipped. For them,
start a registry with [`private-registry.sh`](#a-registry-behind-a-private-ca):

```bash
E2E_CONTEXT=<test cluster context> PRIVATE_REGISTRY_DIR=/tmp/kss-private test/e2e/private-registry.sh up
```

Add its CA (`/tmp/kss-private/ca.crt`) to KubeSwift
(`sandboxMaterialize.registryCABundle`). Copy hello-http into it twice,
under different repositories:

```bash
E2E_CONTEXT=<test cluster context> PRIVATE_REGISTRY_DIR=/tmp/kss-private test/e2e/private-registry.sh copy ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http@sha256:<digest> apps/hello-http:e2e
```

```bash
E2E_CONTEXT=<test cluster context> PRIVATE_REGISTRY_DIR=/tmp/kss-private test/e2e/private-registry.sh copy ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http@sha256:<digest> apps-b/hello-http:e2e
```

Build a runtime image that trusts the CA, and list the reference it prints
in `localApplication.extraRuntimes`:

```bash
E2E_CONTEXT=<test cluster context> PRIVATE_REGISTRY_DIR=/tmp/kss-private test/e2e/private-registry.sh runtime <runtime image> kubeswift-spin-runtime:e2e
```

Add the two copies to the image file as `private-hello-http` and
`private-hello-http-b`, and run:

```bash
E2E_CONTEXT=<test cluster context> LA_IMAGES=la-images.txt LA_PRIVATE_REGISTRY_DIR=/tmp/kss-private LA_RUNTIME_IMAGE=<runtime in the registry> test/e2e/local-application-e2e.sh
```

Phase 2 points the registry's Service at no pod while it runs, and phase 4
replaces the pull Secret; the script restores both, also when it fails.
It deletes its SpinApps, pool, executor and the pool's application
placements afterwards unless `KEEP=1`.
