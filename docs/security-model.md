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
| kubeswift-spin controller | trusted control plane component | Unprivileged pod. Reads SpinApps and executors, creates and deletes SwiftSandboxes, patches SpinApp status. No Secret, ConfigMap or Pod access. |
| Spin Operator | trusted | Creates the SpinApp Service. Has broad permissions of its own, including Secrets. |
| KubeSwift controller and launcher | trusted, node trust boundary | Privileged on nodes; materializes rootfs images, boots microVMs. |
| Runtime image | trusted supply chain artifact | Built and signed by this project; contains Spin and the entrypoint. |
| Spin runtime in the guest | trusted to enforce Wasm isolation | Runs as UID 65532 inside the guest after the entrypoint drops root. |
| Wasm components | untrusted | Application code, possibly third-party or generated. |
| SpinApp authors | partially trusted | May create SpinApps in namespaces they have access to. |
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

## Controller container

The chart runs the controller with `runAsNonRoot`, UID 65532,
`allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, all
capabilities dropped, and the `RuntimeDefault` seccomp profile. The image is
distroless `static` with a single static binary. The policy check enforces
these settings on the rendered chart.

## Secrets

kubeswift-spin never reads a Kubernetes Secret and never places a secret
value in any object, log or Event. This is a design decision driven by how
KubeSwift v0.15.1 handles sandbox input: `spec.env`, `command` and `args` are
written in plain text into the `<sandbox>-runtime-intent` ConfigMap, and
`env[].valueFrom` is dropped. Copying a resolved Secret into those fields
would expose it to anyone who can read ConfigMaps or SwiftSandboxes in the
namespace.

Consequently:

- `variables[].valueFrom.secretKeyRef`, `runtimeConfig.loadFromSecret`,
  secret-backed runtime-config options, `imagePullSecrets` and executor
  `caCertSecret` are rejected with `UnsupportedConfiguration`.
- Runtime-config options whose names denote credentials must be empty, and
  URLs with embedded credentials are rejected, so a user cannot paste a
  token into the SpinApp and have it copied into a ConfigMap.
- Literal variable values are copied into the sandbox spec. They are
  visible to anyone who can read the SpinApp, and additionally to anyone who
  can read SwiftSandboxes or ConfigMaps in the namespace. Do not use
  variables for secrets.
- Condition messages, Events and logs name fields and variables, never
  values. Unit tests assert that findings never contain the values they
  describe. Controller logs record names, namespaces, revisions and reasons,
  not objects.
- The runtime-config file inside the guest is mode 0600, owned by the Spin
  user, and its environment variable is removed before Spin starts.

The KubeSwift feature that would allow secure delivery is specified in
[upstream/kubeswift-sandbox-secret-projection.md](upstream/kubeswift-sandbox-secret-projection.md).

Two Secret names may appear in an executor profile
(`runtime-image-pull-secret`, `runtime-image-verify-key-secret`). They are
names only; KubeSwift reads them on the host side to pull and verify the
runtime image, and their contents never reach the guest. They are distinct
from credentials Spin would need inside the guest to pull a private
application artifact, which are not supported.

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
  runtime-config flag and variable to match exactly.
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
  environment. They build multi-architecture images with BuildKit SBOM and
  provenance attestations, sign images and the chart with cosign keyless
  signing, and pin the image digests into the published chart. An existing
  runtime image is reused only after `cosign verify` confirms it was signed
  by this release workflow. Jobs that run third-party build code (cargo)
  have read-only tokens; checkouts do not persist credentials.

None of these workflows has run yet, because the repository has not been
published. Locally, the equivalent `make` targets have been run.

## Application artifacts

Spin pulls `spec.image` inside the guest over TLS, validating certificates
against the runtime image's CA bundle. kubeswift-spin does not pass
`--insecure`. It does not verify application artifact signatures and does
not resolve tags to digests, so a mutable tag can change the code that runs
on the next replacement. Use digest references
(`registry/app@sha256:...`) for production SpinApps. Digest pinning at
reconcile time and host-side verification depend on
[upstream/kubeswift-sandbox-artifact-projection.md](upstream/kubeswift-sandbox-artifact-projection.md).

The runtime image itself can be verified by KubeSwift before boot when the
executor names a cosign key Secret.

## Network isolation

- Inbound: denied by KubeSwift for every sandbox. kubeswift-spin creates no
  NetworkPolicy, Service, EndpointSlice or proxy and never modifies launcher
  pods.
- Outbound: the executor's network mode (`restricted` by default) plus the
  component's `allowed_outbound_hosts`. `open` removes all egress
  restriction, including the cloud metadata block, and should only be used
  for trusted applications. See [networking.md](networking.md).

## Namespace tenancy

- A SpinApp can only use an executor in its own namespace, and executors
  are created by whoever can create SpinAppExecutors there (by default the
  Helm chart). Profile settings are under the control of executor authors,
  not SpinApp authors. However, any SpinApp author in a namespace can select
  any executor in that namespace: nothing ties an executor to particular
  applications. Put executors with weaker isolation, in particular
  `network-mode: open`, only in namespaces whose SpinApp authors are
  trusted with them.
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
- The application reference must parse as an OCI reference and may not start
  with `-` or contain whitespace; all user-derived arguments are passed in
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

- No secret delivery to sandboxes; no private application registries.
- Application artifacts are not signature-verified and tags are not
  pinned to digests.
- Literal variable values are stored in plain text in SwiftSandbox specs and
  KubeSwift runtime-intent ConfigMaps.
- `open` network mode grants unrestricted egress.
- The KVM execution path has not been end-to-end tested for this release.
- The CI and release workflows have not run on GitHub yet.

## Reporting vulnerabilities

See [SECURITY.md](../SECURITY.md).
