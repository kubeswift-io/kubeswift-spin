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
| kubeswift-spin cache signer | trusted, holds the signing key | Only with compiled caches enabled. Its own ServiceAccount and image: gets compiler SwiftSandboxes, reads and patches the status of records in the release namespace. The signing key (or KMS access) and a registry credential that writes signatures are volumes of its pod; it has no Secret RBAC and never runs tenant code. It parses exported artifacts with size and entry limits. Prefer a KMS key, so a compromise of the pod does not reveal key material. An optional CA bundle (`compiledCache.signer.registryCABundle`) is an immutable ConfigMap volume trusted for all its TLS connections: who can replace ConfigMaps and the signer's Deployment in the release namespace controls its trust anchors, so limit that to the chart's operators. |
| kubeswift-spin cache manager | trusted control plane component | Only with compiled caches enabled. Its own ServiceAccount: reads SpinApps, executors, pools and Namespaces, and writes `SpinCompiledCache` records in the release namespace. It also creates compiler SwiftSandboxes with SpinApps' pull Secrets and SwiftArtifactPlacements that name the cache pull Secret, in any namespace, so, like the controller, treat its credentials like a cluster-wide Secret reader. It changes or deletes only placements that carry a record's labels and UID. See [compiled-cache.md](compiled-cache.md). |

## Threat actors

- A SpinApp author trying to escalate beyond their namespace, read Secrets,
  exhaust capacity, or attack other tenants.
- Malicious or compromised application code in a Wasm component.
- A compromised or spoofed registry serving a malicious artifact.
- A network attacker between the guest and its registry.
- A compromised dependency or build pipeline of this project.

## Kubernetes API access

The chart's ClusterRole, and its leader-election Role for the rows marked
release namespace only (see `charts/kubeswift-spin/templates/rbac.yaml`):

| Resource | Verbs | Why |
|---|---|---|
| `spinapps`, `spinappexecutors` | get, list, watch | read desired state |
| `spinapps/status` | get, patch | report status |
| `spinapps/finalizers` | update | required to set `blockOwnerDeletion` on owner references; no finalizer is added |
| `swiftsandboxes` | get, list, watch, create, delete | realize replicas; specs are immutable so there is no update |
| `swiftsandboxpools` | get, list, watch | warm-pool compatibility check |
| `events.k8s.io` `events` | create, patch | Events |
| `leases` (release namespace only) | get, create, update | leader election |
| core `events` (release namespace only) | create, patch | the LeaderElection Event, which controller-runtime records through the core/v1 API |

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
[compatibility.md](compatibility.md#validation-of-v010).

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
  from the release's `checksums-v4.2.5.txt`. A replaced release asset fails
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
  high or critical vulnerabilities, and generates SPDX SBOMs for the
  controller, runtime and cache signer images. OSV-Scanner runs in its
  own workflows (see [Vulnerability scanning](#vulnerability-scanning)).
- **Releases** (`.github/workflows/release.yaml`) run only for validated
  semver tags in the canonical repository after approval of the `release`
  environment. The approval protects only jobs that declare the
  environment: anyone who can push a `v*` tag can push one on a commit
  whose workflow omits it, and that run signs with the same workflow
  identity. Tag creation is therefore restricted to maintainers by a
  repository ruleset (see [releasing.md](releasing.md#repository-settings)),
  and verifiers should check the exact certificate identity of the tag
  they verify, not a pattern (the runtime image, reused across releases,
  is the exception; see
  [releasing.md](releasing.md#verifying-a-release)). The release jobs
  build multi-architecture images with BuildKit SBOM and provenance
  attestations, sign the images, the chart and the example artifacts with
  cosign keyless signing, and pin the image digests into the published
  chart. An existing runtime image is reused only after `cosign verify`
  confirms it was signed by this release workflow. Jobs that run third-party build code (cargo)
  have read-only tokens; checkouts do not persist credentials, except in
  the OSV pull request scan, whose upstream reusable workflow keeps its
  token (`contents: read`, `actions: read`, `security-events: write`) to
  check out the base branch.

The KVM e2e workflow runs on a self-hosted runner with cluster
credentials. No such runner is registered. Whoever registers one must put
it in a runner group restricted to that workflow on `main`, because any
pull request can otherwise request its labels (see
[test/e2e](../test/e2e/README.md#ci)).

## Vulnerability scanning

Three scanners run, each for a different question; none replaces another:

| Scanner | Question it answers | Where |
|---|---|---|
| `govulncheck` | Does kubeswift-spin's Go code call a vulnerable function? Call-graph analysis of every package in the Go module (`./...`), including the controller and the runtime entrypoint. It does not cover the nested `signer/cosign` module, which only OSV-Scanner and Grype scan. | CI Go job (Kubernetes 1.37 leg), `make vulncheck` |
| OSV-Scanner | Which dependencies in `go.mod` and `examples/Cargo.lock` have known vulnerabilities, today? Inventory against the OSV database, with Go call analysis to mark uncalled findings. | `.github/workflows/osv-scanner-pr.yaml`, `osv-scanner.yaml`, the release workflow, `make osv-scan` |
| Grype | What do the built images contain? Packages that Syft catalogs in the controller and runtime images: the base image packages and the Go modules of the controller and entrypoint binaries. Fails on fixable high or critical findings. | CI image jobs |

A Syft catalog of the published `spin-4.2.1-r1` runtime image lists the
Debian packages and the entrypoint's Go modules but not Spin, so none of
the three scanners inventories Spin's own Rust dependencies; they change
only when the pinned Spin release changes.

OSV-Scanner runs in three places, all through Google's reusable workflows
from `google/osv-scanner-action` v2.6.0. The workflows and the actions they
call are pinned by commit, but those actions run the scanner from the
container image `ghcr.io/google/osv-scanner-action:v2.6.0`, a tag
(`sha256:71ad04ab2f8798be47870f9b18817ad317c2f8f2f97aa6726ba10d5578bc174a`
when this was written). Whoever can move that tag could change what the
scan reports and use the job's `security-events: write` permission; the
job holds no package, signing or contents write permission, and the
release's example build does not restore Actions caches. Pinning the image
would mean replacing Google's workflows with a copy; this project uses the
supported integration and accepts that risk.

Every scan names its inputs, `go.mod` and `examples/Cargo.lock`, with
`--lockfile`, because a recursive scan follows `.gitignore` even for
tracked files. `make verify` runs `hack/check-osv-inputs.sh`, which fails
when that list (in the Makefile and in every OSV workflow) differs from the
tracked dependency manifests.

- **Pull requests**: a differential scan of the base branch and the pull
  request. It fails only on vulnerabilities the pull request introduces, so
  a finding already on `main` does not block unrelated changes. Its check,
  `OSV-Scanner / osv-scan`, is a required status check on `main` (see
  [releasing.md](releasing.md#repository-settings)). Results go to code
  scanning for pull requests from this repository; pull requests from
  forks have a read-only token and get annotations and the check result
  only.
- **Daily and on every push to `main`**: a full scan, so that
  vulnerabilities disclosed after a change merged are reported in
  Security, Code scanning. It fails on any finding that the scanner does not
  classify as uncalled.
- **Releases**: the same full scan runs on the tagged commit at the start
  of the release workflow, and every publishing job depends on it, so a
  finding that fails the scan blocks publication even if the pull requests
  were clean.

`make osv-scan` runs the same CLI version locally and is part of
`make verify-all`, with `govulncheck`. Neither is in `make verify`: both
query live vulnerability databases, so their results change without any
change to the repository.

Every finding is classified as affecting, not affecting, unreachable,
test-only, tooling-only, false positive, or requires investigation. An
affecting finding is fixed before release. An exception is added to an
`osv-scanner.toml` at the repository root only for one vulnerability ID at
a time, with the technical reason and an `ignoreUntil` review date;
`hack/check-osv-inputs.sh` rejects nested configuration files, entries
without a reason or review date, and package-wide ignores. Exceptions
still need careful review: the `main` ruleset requires no approving review,
and a pull request's own scan reads its own configuration. Dependency
presence alone is not treated as exploitability, and neither is its absence
from a call graph treated as a reason to stop tracking it. The exceptions
are the two cosign advisories of the cache signer listed below.

The release gate covers the source dependency manifests. The images are
scanned by Grype in CI on every change to `main`, not again at release
time, so a vulnerability in the base image or the entrypoint's modules
disclosed between the last CI run and the release is not caught by the
release itself.

Findings the scanner classifies as uncalled do not fail a scan, but the full
scans still send them to code scanning, where they stay open as alerts so
that they remain visible until the dependency is upgraded.

Current findings:

| ID | Package | Classification | Reason |
|---|---|---|---|
| GO-2026-6094 | `github.com/google/cel-go` v0.29.2 (fixed in v0.30.0 according to the Go vulnerability database; the GitHub advisory GHSA-gcjh-h69q-9w9g lists v0.29.0, so Grype does not report it), indirect through `k8s.io/apiserver` and controller-runtime's metrics authorization | unreachable | The vulnerable symbols (`ext.NativeTypes`, `ext.ParseStructTag`) are not called: `govulncheck` reports no affected code, and OSV-Scanner's Go call analysis classifies it as uncalled, which does not fail the scan. cel-go stays at the version Kubernetes pins; it is upgraded with the Kubernetes dependencies. |
| GO-2026-6225 | cosign v3.1.3 (`signer/cosign`), its Azure Container Registry credential helper | exception, review by 2027-01-31 | The helper can send Azure credentials from `AZURE_*` variables to registries that are not Azure. The chart refuses `azurekms://` keys and `AZURE_*` variables for the signer, so there are none to send. No fixed cosign release exists. |
| GO-2026-5932 | `golang.org/x/crypto/openpgp`, through cosign (`signer/cosign`) | exception, review by 2027-01-31 | Deprecated and unmaintained. The signer signs with a cosign or KMS key and never reads or verifies OpenPGP data. No fixed version exists. |

## Application artifacts

Spin pulls `spec.image` inside the guest over TLS, validating certificates
against the runtime image's CA bundle. kubeswift-spin does not pass
`--insecure`. It does not verify application artifact signatures and does
not resolve tags to digests, so a mutable tag can change the code that runs
on the next replacement. Use digest references (`registry/app@sha256:...`).
KubeSwift v0.16.0 can mount OCI artifacts read-only (`spec.artifacts`), but
kubeswift-spin does not use it: Spin 4.2.1 runs an application only from a
manifest, a `.wasm` file or a registry reference, not from a local OCI
layout
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
  prefer `namespaceSelector` peers for namespaces you control. A CNI that
  does not enforce NetworkPolicy enforces neither the port restriction nor
  the annotation.
  KubeSwift's readiness and liveness probes run inside the launcher pod and
  are not affected. On KubeSwift v0.15.1 all inbound traffic is denied.
- KubeSwift v0.16.3 creates the per-sandbox NetworkPolicy before the
  launcher pod and keeps it, with a finalizer, until the launcher has
  stopped, so a replica being deleted stays isolated through its drain, as
  long as the cluster's NetworkPolicy implementation keeps enforcing a
  policy that is being deleted until it is gone (observed with Calico). A
  launcher pod deleted with `--force` is not covered: its guest may still
  run while the policy goes.
  Observed on the lab cluster (KubeSwift v0.16.3, Calico): through a rolling
  update and through a SpinApp deletion, a pod outside the `ingress-from`
  peers that probed the replica being deleted got no successful connection
  (0 of 176 attempts during the drain across the two runs, 0 of 72
  before it), while an allowed pod was served until the drain ended; the
  policy was removed only after the launcher pod had ended `Succeeded`.
  On KubeSwift v0.16.2 and earlier the policy was removed as
  soon as the sandbox was deleted while the launcher pod and its guest
  kept running for up to the 30-second termination grace period: any pod
  in the cluster could reach the Spin port by pod IP and `ingress-from` was
  not enforced (observed on the lab cluster with v0.16.2). That is one
  reason v0.16.3 is the minimum supported release. The egress restriction is
  enforced inside the launcher pod and is not affected by any of this.
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

- A SpinApp can only use an executor in its own namespace, and executors are
  created by whoever can create SpinAppExecutors there (by default the Helm
  chart). Profile settings are under the control of executor authors, not
  SpinApp authors, with one exception: a SpinApp author can set the drain
  period of the SpinApp's replicas (`spin.kubeswift.io/drain-period`), up to
  the executor's `spin.kubeswift.io/max-drain-period` (KubeSwift's 10
  minutes by default; see [Denial of service and
  capacity](#denial-of-service-and-capacity)). However, any SpinApp author
  in a namespace can select any executor in that namespace: nothing ties an
  executor to particular applications. Put executors with weaker isolation,
  in particular `network-mode: open` or a broad `egress-allow`, only in
  namespaces whose SpinApp authors are trusted with them.
- Sandboxes are created in the SpinApp's namespace, and KubeSwift resolves
  kernels and Secrets in that namespace.
- Ownership is by controller owner reference UID, so a user cannot make
  kubeswift-spin adopt, modify or delete a SwiftSandbox it did not create,
  even with matching labels. A name collision is reported as
  `SandboxConflict`, except that a sandbox of a deleted SpinApp with the
  same name that is still terminating is waited for, for at most 2 minutes
  plus that sandbox's drain period (up to about 12 minutes). On KubeSwift
  v0.16.3 a pod named like a replica's launcher (`<spinapp>-<ordinal>`) that
  someone else created blocks that replica (`LauncherPodConflict`);
  KubeSwift does not adopt it.
- The controller cache holds only SwiftSandboxes with its managed-by label.

## Malicious SpinApp input

- Every SpinApp field is classified; unsupported fields block
  reconciliation instead of being dropped.
- The application reference must parse as an OCI reference, start with a
  registry host (`ghcr.io/...`, `docker.io/...`, `localhost:5000/...`) and
  may not start with `-` or contain whitespace. `spin up --from` loads an
  existing local path before trying a registry, and the host requirement
  keeps guest paths such as `var/lib/...` out; all user-derived arguments
  are passed in `--flag=value` form, so they cannot inject Spin flags.
  Arguments are passed as an argv list, never through a shell.
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
  crashing application cannot churn microVMs. A failed replica from an
  earlier revision waits 10 seconds after its failure, so a spec that
  changes often still replaces a failing replica at most once per 10
  seconds,
- a SwiftSandbox create that a quota or admission policy refuses stops the
  remaining creates of that reconcile; the SpinApp is requeued after 10
  seconds, doubling per consecutive refusal up to 5 minutes, and an earlier
  change to the SpinApp or its sandboxes also retries.

These limits are per SpinApp, not per namespace. Use Kubernetes
ResourceQuota on `count/swiftsandboxes.sandbox.kubeswift.io` (object count
quota) to cap the number of sandboxes per namespace.

During a rolling update a SpinApp runs one extra sandbox, the surge
replica ([ADR 0013](adr/0013-surge-replacement.md)), and a scale-down can
keep replicas above the new count until the remaining ones are available;
a SpinApp never runs more than `--max-replicas + 1` sandboxes (21 at the
default). The
bound holds for replacements kubeswift-spin makes, which delete with
foreground propagation and recreate an ordinal only when its old sandbox
is gone. On KubeSwift v0.16.3 it also holds when a
user deletes SwiftSandboxes with the default background propagation: the
sandbox keeps a termination finalizer until its launcher has stopped (or
until 30 seconds past the launcher's grace period, when KubeSwift lets it
go, for example for an unreachable node), so the ordinal is normally
recreated only afterwards. On KubeSwift v0.16.2 and earlier
the object disappeared at once, a cold-started replacement attached to the
old terminating launcher pod and failed when it exited (see
[compatibility.md](compatibility.md#known-upstream-issues)), and a
replacement that claimed a warm-pool slot ran alongside it, one more
microVM for up to 30 seconds; delete with `--cascade=foreground` there. Size
quotas and node capacity with one sandbox of headroom per SpinApp that may
roll at the same time. A change that rolls every SpinApp at once, such as
upgrading KubeSwift to v0.16.2 (replicas that get readiness startup are
replaced) or changing the default runtime image, needs that headroom for
all of them together. A surge replica that cannot be created or started
stalls the rollout; the old replicas keep running.

A rollout removes an available old replica only while `spec.replicas` other
replicas are available, so the first one goes only after the surge replica
at the new revision is ready. If the new revision never becomes ready, the
old replicas keep running with their old configuration, which matters when
the change is a security fix (a new runtime image, a narrower network
profile). To remove an old revision sooner, delete its SwiftSandboxes with
foreground propagation, including the surge replica `<spinapp>-<replicas>`
if its `spin.kubeswift.io/revision` label is not the current one:

```bash
kubectl delete swiftsandbox <name> -n <namespace> --cascade=foreground
```

Each deleted ordinal is recreated at the new revision once the old one is
gone, and the surge replica at the new revision too while the rollout lasts.
A deleted replica drains for 5 seconds, or for the
`spin.kubeswift.io/drain-period` of its SpinApp or executor (up to 10
minutes), and is then stopped (KubeSwift v0.16.3; up to 30 seconds on
earlier releases). Deleting old SwiftSandboxes therefore removes them only
after their drain: until then each keeps running with its old
configuration (runtime image, egress allowlist, secret files) and stays
reachable by its `ingress-from` peers, although no longer through the
Service. `spec.replicas: 0` is not supported and leaves every sandbox in
place.

The drain period is the only kubeswift-spin setting a SpinApp author
controls, and it affects the platform:

- A deleted replica keeps its node capacity for its drain, up to about 10
  minutes and 15 seconds alongside its replacement. Draining SwiftSandboxes
  and their launcher pods count against ResourceQuota until they are gone.
- A rollout replaces one replica per drain period of the old replicas, so
  an executor change made by the platform operator, such as a new runtime
  image, reaches every replica of a SpinApp with a 10-minute drain only
  after about N times 10 minutes.
- KubeSwift sets the launcher pod's termination grace period to the drain
  plus 15 seconds, so draining a node, upgrading it or evicting the pod
  waits up to about 10 minutes and 15 seconds per launcher pod.

The platform operator bounds this per executor with
`spin.kubeswift.io/max-drain-period` (chart value
`executors[].maxDrainPeriod`): a SpinApp that asks for more is reported
`UnsupportedConfiguration` and nothing is created or replaced for it. An
admission policy on SpinApps (for example a ValidatingAdmissionPolicy) can
forbid the annotation entirely.

## Finalizers and deletion

kubeswift-spin adds no finalizer. Deleting a SpinApp removes its sandboxes
through owner-reference garbage collection, so a broken or removed
kubeswift-spin controller can never block deletion. Sandboxes are deleted
with foreground propagation so a replacement does not race KubeSwift's
cleanup.

KubeSwift v0.16.3 adds its own finalizers: `kubeswift.io/sandbox-termination`
on every SwiftSandbox and `kubeswift.io/launcher-isolation` on every
launcher NetworkPolicy. Deleting SwiftSandboxes, and so namespaces that
contain them (and SpinApps deleted with `--cascade=foreground`), then needs
the KubeSwift controller running; a SpinApp deleted with the default
propagation goes at once and leaves its sandboxes terminating. The sandbox
finalizer is released by KubeSwift at the latest 30 seconds after the
launcher's grace period (the drain period plus 15 seconds, so up to about
10 minutes and 45 seconds after the deletion), and the NetworkPolicy finalizer only once the
launcher pod has finished. Before downgrading KubeSwift, remove these
finalizers as its release notes describe.

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
- kubeswift-spin still runs on KubeSwift v0.16.0 to v0.16.2, where a
  deleted replica loses its NetworkPolicy while it can still serve for up
  to 30 seconds, so `ingress-from` is not enforced in that window. The
  controller logs this once at startup; use KubeSwift v0.16.3 or later.
- Application artifacts are not signature-verified and tags are not
  pinned to digests.
- Literal variable values are stored in plain text in SwiftSandbox specs and
  KubeSwift runtime-intent ConfigMaps.
- `open` network mode grants unrestricted egress.
- `deploymentConfig.caCertSecret` is not implemented.
- The KVM path was validated on one lab cluster, on linux/amd64 only (see
  [compatibility.md](compatibility.md#tested-versions)). arm64 images are
  built and published but not validated.

## Reporting vulnerabilities

See [SECURITY.md](../SECURITY.md).

## Compiled caches

Compiled Wasmtime caches are native code mounted into replicas
([ADR 0014](adr/0014-compiled-wasmtime-cache.md)). The trust anchor is
KubeSwift's platform verification of the signer's signature and claims, not
the `SpinCompiledCache` record. With the feature enabled the chart adds:

| Identity | Resource | Verbs | Scope |
|---|---|---|---|
| controller | `spincompiledcaches` | get, list, watch | release namespace |
| controller | `namespaces` | get, list, watch | cluster |
| cache manager | `spinapps`, `spinappexecutors`, `swiftsandboxpools`, `namespaces` | get, list, watch | cluster |
| cache manager | `swiftsandboxes` (compilers) | get, list, watch, create, delete | cluster |
| cache manager | `swiftartifactplacements` | get, list, watch, create, update, delete | cluster |
| cache manager | `spincompiledcaches` | get, list, watch, create, delete | release namespace |
| cache manager | `spincompiledcaches/status` | get, patch | release namespace |
| cache manager | `leases` | create; get, update on its own Lease | release namespace |
| cache manager | core and `events.k8s.io` `events` | create, patch | release namespace |
| cache signer | `swiftsandboxes` | get | cluster |
| cache signer | `spincompiledcaches` | get, list, watch | release namespace |
| cache signer | `spincompiledcaches/status` | get, patch | release namespace |
| cache signer | `leases` | create; get, update on its own Lease | release namespace |
| cache signer | core `events` | create, patch | release namespace |

`hack/check_chart_policy.py` ties each rule set to the ServiceAccount
allowed to hold it, so the controller cannot be granted the cache manager's
write access by a chart change.

Tenants must not be able to write in the release namespace: install
kubeswift-spin into a namespace of its own. Namespaces that use compiled
caches must meet the [prerequisites](compiled-cache.md#prerequisites); in
particular, no non-platform principal may exec into, attach to or modify
pods there, because KubeSwift's fetch pods can write the node's shared
artifact cache.

Residual risks:

- A tenant's application can influence the native code compiled for that
  tenant's namespace, as a Cranelift bug would let any Wasm module do.
- Claims bind a namespace by name: a namespace deleted and re-created by
  another principal inherits its predecessor's signed caches until they are
  removed from the registry.
- The controller sets the expected identity on each replica and the
  entrypoint compares it with the mounted metadata, but the guarantee that
  the mounted code is the signed code is KubeSwift's: the entrypoint cannot
  verify a signature. A mount that is not read-only, or whose metadata
  names anything else, is not used.
