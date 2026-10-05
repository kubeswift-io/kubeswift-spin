# Security model

This document describes what kubeswift-spin protects, what it trusts, and
what it does not protect against. It covers this repository: the
controller, the runtime image and the Helm chart. KubeSwift and Spin
Operator have their own security models.

## The property

A Spin application realized by kubeswift-spin runs behind two independent
boundaries:

1. **WebAssembly**: Spin runs each component in Wasmtime with only the
   capabilities its manifest grants (outbound hosts, variables, key-value
   stores, databases, AI models).
2. **microVM**: Spin itself runs in a Cloud Hypervisor guest with its own
   kernel, started by KubeSwift.

An attacker who escapes the Wasm sandbox lands in an unprivileged process
inside a guest, not on the node. This does not eliminate risk in the
hypervisor, the guest or host kernels, the KubeSwift launcher, or the node.
It is defense in depth, not a guarantee.

The whole stack is not unprivileged. The KubeSwift launcher pod runs with
the privileges needed to operate KVM, set up guest networking and pass
through devices, and is part of the node trust boundary. Those privileges
are KubeSwift's responsibility; kubeswift-spin neither has nor needs them.

## Assets

- Integrity and availability of the cluster and nodes
- Confidentiality of Secrets in the cluster
- Integrity of the runtime image and of application artifacts
- Isolation between tenants' SpinApps and namespaces
- Capacity: microVM CPU and memory reserved on kernel nodes

## Trust boundaries and components

| Component | Trust | Notes |
|---|---|---|
| kubeswift-spin controller | trusted control plane component | Unprivileged pod. Reads SpinApps and executors, creates and deletes SwiftSandboxes, patches SpinApp status. No Secret, ConfigMap or Pod permission, but creating SwiftSandboxes in any namespace is equivalent to reading that namespace's Secrets (see [Kubernetes API access](#kubernetes-api-access)). |
| Spin Operator | trusted | Creates the SpinApp Service. Has broad permissions of its own, including Secrets. |
| KubeSwift controller and launcher | trusted, node trust boundary | Privileged on nodes; materializes rootfs images, boots microVMs. With KubeSwift v0.16.0 the launcher reads the Secrets a sandbox references, with its own per-sandbox ServiceAccount, and delivers them to the guest. |
| Runtime image | trusted supply chain artifact | Built and signed by this project; contains Spin and the entrypoint. |
| Spin runtime in the guest | trusted to enforce Wasm isolation | Runs as UID 65532 inside the guest after the entrypoint drops root. Holds the registry credentials and Secret-backed runtime configuration of its SpinApp. |
| Wasm components | untrusted | Application code, possibly third-party or generated. |
| Executor authors | trusted like SwiftSandbox creators | Whoever can create or update a SpinAppExecutor in a namespace decides, through the controller, the runtime image, node selector, kernel profile, network mode (`open` removes the cloud metadata block), egress allowlist and warm pool of every sandbox for that namespace. That is the power of creating SwiftSandboxes there. kubeswift-spin has no administrator policy that limits executor profiles; grant executor write access only to platform operators. |
| SpinApp authors | partially trusted | May create SpinApps in namespaces they have access to, and can make any Secret in that namespace available to their application (see [Secrets](#secrets)). Their `spec.podLabels` become launcher pod labels, so they can match pod-label based NetworkPolicy peers and Services in their namespace (see [Network isolation](#network-isolation)). |
| Application registries | untrusted content source | Artifacts are pulled by Spin inside the guest. |

## Threat actors

- A SpinApp author trying to escalate beyond their namespace, read Secrets,
  exhaust capacity, or attack other tenants.
- Malicious or compromised application code in a Wasm component.
- A compromised or spoofed registry serving a malicious artifact.
- A network attacker between the guest and its registry.
- A compromised dependency or build pipeline of this project.

## Kubernetes API access

The chart's ClusterRole (see `charts/kubeswift-spin/templates/rbac.yaml`):

| Resource | Verbs | Why |
|---|---|---|
| `spinapps`, `spinappexecutors` | get, list, watch | read desired state |
| `spinapps/status` | get, patch | report status |
| `spinapps/finalizers` | update | required to set `blockOwnerDeletion` on owner references; no finalizer is added |
| `swiftsandboxes` | get, list, watch, create, delete | realize replicas; specs are immutable so there is no update |
| `swiftsandboxpools` | get, list, watch | warm-pool compatibility check |
| `events.k8s.io` `events` | create, patch | Events |
| `leases` (release namespace only) | get, create, update | leader election |

There are no wildcards, and no access to Secrets, ConfigMaps, Pods or
Services. `make helm-lint` runs a policy check that fails the build if the
rendered chart gains a wildcard rule or access to Secrets, ConfigMaps or
Pods. At startup the controller checks its own permissions with
SelfSubjectAccessReviews and exits with the list of missing ones rather than
running with forbidden informers. With `metrics.secure=true` it also needs
`tokenreviews` and `subjectaccessreviews` create.

RBAC is cluster-scoped even with `--watch-namespaces`; the flag limits what
the controller watches, not what it is allowed to do.

Creating SwiftSandboxes is more powerful than the verb suggests. KubeSwift
gives each sandbox's launcher read access to the Secrets the sandbox
references and delivers them into the guest. Whoever holds the controller's
ServiceAccount token (after a compromise of the controller pod or its node)
can therefore create a sandbox in any namespace, with any image and open
egress, that reads any Secret in that namespace. Treat the controller's
credentials like a cluster-wide Secret reader. A chart mode with per-namespace
Roles is not implemented.

## Controller container

The chart runs the controller with `runAsNonRoot`, UID 65532,
`allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, all
capabilities dropped, and the `RuntimeDefault` seccomp profile. The image is
distroless `static` with a single static binary. The policy check enforces
these settings on the rendered chart.

## Secrets

kubeswift-spin never reads a Kubernetes Secret and never places a secret
value in any object, log or Event. The controller has no Secret, ConfigMap
or Pod permissions, and `make helm-lint` fails if the chart grants them.

KubeSwift writes a sandbox's literal `env`, `command` and `args` in plain
text into the `<sandbox>-runtime-intent` ConfigMap, so a value copied into
those fields would be readable by anyone who can read ConfigMaps or
SwiftSandboxes in the namespace. kubeswift-spin therefore passes Secrets
only as references, which KubeSwift v0.16.0 resolves:

| SpinApp field | In the SwiftSandbox | In the guest |
|---|---|---|
| `variables[].valueFrom.secretKeyRef` | `env` entry `SPIN_VARIABLE_<NAME>` with `valueFrom.secretKeyRef` | environment variable read by Spin |
| runtime-config option `valueFrom.secretKeyRef` | placeholder `kubeswift-spin-secret:KUBESWIFT_SPIN_SECRET_<n>` in the rendered TOML, and `env` entry `KUBESWIFT_SPIN_SECRET_<n>` with `valueFrom.secretKeyRef` | substituted into the runtime-config file by the entrypoint as root; the variables are removed before Spin starts |
| `runtimeConfig.loadFromSecret` | `secretFiles` entry for key `runtime-config.toml` | `/run/kubeswift-spin/runtime-config.toml` (root, 0400), copied to `/var/lib/kubeswift-spin/runtime-config.toml` (UID 65532, 0600) |
| `imagePullSecrets` | `secretFiles` entry for key `.dockerconfigjson` per Secret | `/run/kubeswift-spin/registry-auth/<i>.json` (root, 0400), merged into `/var/lib/kubeswift-spin/home/.docker/config.json` (UID 65532, 0600) |

The KubeSwift launcher reads the Secrets with its own per-sandbox
ServiceAccount and hands the values to the guest without writing them to
any object, log or node disk. Secret files need the sandbox kernel 6.6.14
or later.

The KVM e2e test exercises all four paths with values unique to the run
(Secret-backed variable, Secret-backed runtime-config option,
`loadFromSecret`, `imagePullSecrets` for an htpasswd registry). After each
one it searches every namespaced object in the test namespace except
Secrets (SpinApps, SwiftSandboxes, Pods and their command lines, ConfigMaps,
Events, NetworkPolicies), the Events of all namespaces, and the logs of the
kubeswift-spin controller, the KubeSwift system pods and the launcher pods
for the value and its base64 encodings. None was found; see
[compatibility.md](compatibility.md#tested-versions).

Trust consequences:

- **SpinApp authors can use any Secret in their namespace.** The KubeSwift
  webhook does not check that the author of a SpinApp may read the Secrets
  it references. Anyone who can create a SpinApp in a namespace can expose
  that namespace's Secrets to their application, which can return them in a
  response. This is the same trust as a Pod's `secretKeyRef`: keep Secrets
  that a namespace's SpinApp authors must not see out of that namespace.
- **Credentials are inside the guest.** Registry credentials are delivered
  into the KubeSwift guest so that Spin can authenticate to the application
  registry. They are not exposed through Kubernetes objects, and they are
  not exposed to Wasm components under Spin's capability model: a component
  gets no host filesystem access other than the files its application
  declares from its own package, and no environment other than the
  variables its manifest declares. That rests on Spin's semantics; the e2e
  test does not run an adversarial component. The credentials stay in the
  guest for the lifetime of the sandbox, although Spin uses them only to
  pull the application at startup. A process that compromises the Spin
  runtime inside the guest (UID 65532) can read them, and the same holds
  for Secret-backed runtime configuration. A Secret-backed variable is
  visible to the components that declare it. Pulling the application on
  the host side, outside the guest, would need Spin to run an application
  from a local OCI layout, which Spin 4.2.1 cannot do.
- **Rotation.** Values are delivered when a sandbox starts. The sandbox
  revision hashes Secret names and keys, not values, so rotating a Secret
  does not replace replicas; they keep the old value until replaced.

Still rejected:

- `configMapKeyRef` (variables and runtime-config options), because
  KubeSwift refuses every `valueFrom` source other than `secretKeyRef`;
  `fieldRef` and `resourceFieldRef` have no meaning in a guest.
- Non-empty literal values of credential-named runtime-config options and
  URLs with embedded credentials. The message asks for
  `valueFrom.secretKeyRef` instead.
- `deploymentConfig.caCertSecret` on an executor (`ExecutorInvalid`): not
  implemented.
- Every Secret reference on KubeSwift before v0.16.0
  (`UnsupportedConfiguration`).

Literal variable values and health-check `httpHeaders` values are copied
into the sandbox spec and the runtime-intent ConfigMap. Do not put secrets
in them.

KubeSwift's guest init exports the sandbox environment, including
Secret-backed variables, before it starts the workload. Treat any way of
running commands in a guest as access to that SpinApp's Secrets.
Condition messages, Events and logs name fields and variables, never
values; unit tests assert that findings and entrypoint errors never contain
the values they describe. Controller logs record names, namespaces,
revisions and reasons, not objects.

Two Secret names may appear in an executor profile
(`runtime-image-pull-secret`, `runtime-image-verify-key-secret`). They are
names only; KubeSwift reads them on the host side to pull and verify the
runtime image, and their contents never reach the guest. They are distinct
from `imagePullSecrets`, which Spin uses inside the guest to pull the
application.

## Runtime image and guest

- Based on distroless `static`: no shell, no package manager, no
  interpreters. `make runtime-test` asserts the absence of `/bin/sh`,
  package managers, `curl` and `wget`.
- KubeSwift starts the workload as root and ignores the image `USER`. The
  entrypoint therefore creates its working directories, clears
  supplementary groups, switches to UID and GID 65532, verifies the switch,
  sets `PR_SET_NO_NEW_PRIVS`, and execs Spin. Spin and its trigger processes
  run unprivileged in the guest (verified by `make runtime-test`).
- The entrypoint accepts only `up` and `--version` and requires the
  runtime-config flag and variable to match exactly. It reads secret files
  only from `/run/kubeswift-spin`, at most 64 KiB each, before it drops
  root, and never prints their contents.
- Spin listens on port 3000, so it needs no capability.
- The guest root filesystem is a read-only image with a memory-backed
  overlay; nothing persists across a replacement.

## Supply chain

- **Spin**: the static release binary is downloaded during the image build
  and verified against SHA-256 digests pinned in `runtime/Dockerfile`, taken
  from the release's `checksums-v4.2.1.txt`. A replaced release asset fails
  the build. `hack/verify-spin-release.sh` additionally checks the pins
  against the published checksums and verifies the Sigstore signature of
  each binary against the identity of Spin's release workflow for the tag;
  CI runs it on every change.
- **Base images** are pinned by digest. **Go modules** are pinned in
  `go.sum`; **Rust crates** in `examples/Cargo.lock`, with `spin-sdk` pinned
  to an exact version. **GitHub Actions** are pinned by commit SHA, and
  Dependabot proposes updates.
- **CI** (`.github/workflows/ci.yaml`) runs `govulncheck`, golangci-lint
  (including gosec), actionlint, Grype image scans that fail on fixable
  high or critical vulnerabilities, and generates SPDX SBOMs for both
  images.
- **Releases** (`.github/workflows/release.yaml`) run only for validated
  semver tags in the canonical repository after approval of the `release`
  environment. The approval protects only jobs that declare the
  environment: anyone who can push a `v*` tag can push one on a commit
  whose workflow omits it, and that run signs with the same workflow
  identity. Tag creation is therefore restricted to maintainers by a
  repository ruleset (see [releasing.md](releasing.md#repository-settings)),
  and verifiers should check the exact certificate identity of the tag
  they verify, not a pattern. They build multi-architecture images with BuildKit SBOM and
  provenance attestations, sign images and the chart with cosign keyless
  signing, and pin the image digests into the published chart. An existing
  runtime image is reused only after `cosign verify` confirms it was signed
  by this release workflow. Jobs that run third-party build code (cargo)
  have read-only tokens; checkouts do not persist credentials.

The KVM e2e workflow runs on a self-hosted runner with cluster
credentials. No such runner is registered. Whoever registers one must put
it in a runner group restricted to that workflow on `main`, because any
pull request can otherwise request its labels (see
[test/e2e](../test/e2e/README.md#ci)).

## Application artifacts

Spin pulls `spec.image` inside the guest over TLS, validating certificates
against the runtime image's CA bundle. kubeswift-spin does not pass
`--insecure`. It does not verify application artifact signatures and does
not resolve tags to digests, so a mutable tag can change the code that runs
on the next replacement. Use digest references
(`registry/app@sha256:...`). KubeSwift v0.16.0 can mount OCI artifacts
read-only (`spec.artifacts`), but kubeswift-spin does not use it: Spin 4.2.1
runs an application only from a manifest, a `.wasm` file or a registry
reference, not from a local OCI layout
([upstream/kubeswift-sandbox-artifact-projection.md](upstream/kubeswift-sandbox-artifact-projection.md)).

The runtime image itself can be verified by KubeSwift before boot when the
executor names a cosign key Secret.

## Network isolation

- Inbound (KubeSwift v0.16.0): KubeSwift creates a NetworkPolicy for each
  sandbox (in both `restricted` and `open` modes) that admits
  traffic to the Spin HTTP port (3000, named `http-app`) and nothing else.
  Without the executor annotation `spin.kubeswift.io/ingress-from`, any
  source in the cluster may connect to that port, directly or through the
  SpinApp Service. The annotation's NetworkPolicy peers (pod selector,
  namespace selector or `ipBlock`) become the `from` list of that policy,
  so the identity of a client is whatever those peers match, evaluated by
  the cluster's NetworkPolicy implementation. A `podSelector` peer is only
  as strong as the trust in everyone who can label pods in the selected
  namespaces, and that includes SpinApp authors through `spec.podLabels`;
  prefer `namespaceSelector` peers for namespaces you control. A CNI that does not enforce
  NetworkPolicy enforces neither the port restriction nor the annotation.
  KubeSwift's readiness and liveness probes run inside the launcher pod and
  are not affected. On KubeSwift v0.15.1 all inbound traffic is denied.
- kubeswift-spin creates no NetworkPolicy, Service, EndpointSlice or proxy
  and never patches, labels or execs into launcher pods. Launcher pod labels
  come only from `spec.podMetadata`, which KubeSwift applies and which
  refuses keys under `kubeswift.io` domains; kubeswift-spin also rejects
  `spec.podLabels` keys under `core.spinkube.dev`, so a SpinApp cannot add
  itself to another SpinApp's Service selector through `podLabels`.
- Outbound: the executor's network mode (`restricted` by default), its
  egress allowlist (`spin.kubeswift.io/egress-allow`, only with
  `restricted`), plus the component's `allowed_outbound_hosts`.
  `169.254.0.0/16` stays blocked under `restricted` even with an allowlist.
  `open` removes all egress restriction, including the cloud metadata
  block, and should only be used for trusted applications. See
  [networking.md](networking.md).

## Namespace tenancy

- A SpinApp can only use an executor in its own namespace, and executors
  are created by whoever can create SpinAppExecutors there (by default the
  Helm chart). Profile settings are under the control of executor authors,
  not SpinApp authors. However, any SpinApp author in a namespace can select
  any executor in that namespace: nothing ties an executor to particular
  applications. Put executors with weaker isolation, in particular
  `network-mode: open` or a broad `egress-allow`, only in namespaces whose
  SpinApp authors are trusted with them.
- Sandboxes are created in the SpinApp's namespace, and KubeSwift resolves
  kernels and Secrets in that namespace.
- Ownership is by controller owner reference UID, so a user cannot make
  kubeswift-spin adopt, modify or delete a SwiftSandbox it did not create,
  even with matching labels. A name collision is reported as
  `SandboxConflict`.
- The controller cache holds only SwiftSandboxes with its managed-by label.

## Malicious SpinApp input

- Every SpinApp field is classified; unsupported fields block
  reconciliation instead of being dropped.
- The application reference must parse as an OCI reference, start with a
  registry host (`ghcr.io/...`, `docker.io/...`, `localhost:5000/...`) and
  may not start with `-` or contain whitespace. `spin up --from` loads an
  existing local path before trying a registry, and the host requirement
  keeps guest paths such as `var/lib/...` out; all user-derived arguments are passed in
  `--flag=value` form, so they cannot inject Spin flags. Arguments are passed
  as an argv list, never through a shell.
- Component IDs, variable names and runtime-config keys are validated
  against Spin's naming rules; runtime-config options named `type` are
  rejected so they cannot override the store type.
- Names of child objects are derived deterministically and length-bounded,
  with a 48-bit hash suffix for long or dotted names. Literal names that
  already end in a hash-like suffix are hashed too, so a SpinApp cannot
  claim another SpinApp's sandbox names by choosing its name; a collision
  needs a hash collision and only blocks (it never adopts).
- Condition messages are bounded (five problems, 64-character names, 4096
  bytes) and Event notes are capped at 1000 bytes, so oversized input cannot
  make status updates or Events fail.

## Denial of service and capacity

Each replica reserves at least one vCPU and the configured memory for its
lifetime. Controls:

- `--max-replicas` (default 20) per SpinApp,
- `--max-vcpus` (default 8) and `--max-memory` (default 16Gi) per replica,
- `--min-memory` (default 256Mi) rejects shapes too small to run Spin,
- CPU is rounded up, never down, and the rounding is visible in the
  sandbox spec,
- failed replicas are replaced with exponential backoff (10 seconds to
  5 minutes), which resets only after 10 minutes of healthy running, so a
  crashing application cannot churn microVMs.

These limits are per SpinApp, not per namespace. Use Kubernetes
ResourceQuota on `count/swiftsandboxes.sandbox.kubeswift.io` (object count
quota) to cap the number of sandboxes per namespace.

## Finalizers and deletion

kubeswift-spin adds no finalizer. Deleting a SpinApp removes its sandboxes
through owner-reference garbage collection, so a broken or removed
controller can never block deletion. Sandboxes are deleted with foreground
propagation so a replacement does not race KubeSwift's cleanup.

## Metrics endpoint

By default metrics are served over plain HTTP without authentication. They
carry no tenant identifiers (no namespace or application labels). With
`metrics.secure=true`, the endpoint requires an authorized bearer token, but
controller-runtime serves a self-signed certificate and the chart's
ServiceMonitor skips certificate verification, so the scraper's token is
sent without server authentication. Provide a certificate and configure the
ServiceMonitor TLS settings if that matters in your environment.

## Status and Event leakage

Status messages may include KubeSwift failure messages (truncated to 200
characters), which can contain image references and registry error text.
They never contain values of variables or runtime-config options.

## Known limitations

- Any SpinApp author in a namespace can expose that namespace's Secrets to
  their application; KubeSwift does not check the author's access.
- Registry credentials and Secret-backed configuration are readable by the
  Spin process in the guest for the lifetime of the sandbox. Rotated Secret
  values take effect only when a replica is replaced.
- Without `spin.kubeswift.io/ingress-from`, any cluster source can reach the
  Spin HTTP port. Ingress restriction depends on the CNI enforcing
  NetworkPolicy; it was tested with Calico.
- Application artifacts are not signature-verified and tags are not
  pinned to digests.
- Literal variable values are stored in plain text in SwiftSandbox specs and
  KubeSwift runtime-intent ConfigMaps.
- `open` network mode grants unrestricted egress.
- `deploymentConfig.caCertSecret` is not implemented.
- The KVM path was validated on one lab cluster, on linux/amd64 only (see
  [compatibility.md](compatibility.md#tested-versions)). arm64 images are
  published but untested.

## Reporting vulnerabilities

See [SECURITY.md](../SECURITY.md).
