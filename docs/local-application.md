# Local applications

kubeswift-spin can start new warm replicas of a SpinApp from a copy of
their own application artifact that KubeSwift has already placed on the
pool's nodes, instead of having Spin pull it from the registry inside the
guest on every start. The canonical application stays the Spin OCI artifact
in `SpinApp.spec.image`: there is no new application format, no derived
artifact and no new signature. The design is
[ADR 0015](adr/0015-local-application-delivery.md).

It is an optimization. A replica that cannot use the local copy starts with
`spin up --from=<spec.image>`, exactly as without the feature, and a
SpinApp becomes `Available` the same way either way.

## Status of this revision

Local applications are new in kubeswift-spin v0.3.0 and need KubeSwift
v0.19.3 or later.

| Stage | State |
|---|---|
| Opt-in (chart, namespace label, executor and SpinApp annotations) | implemented |
| Eligibility and the `LocalApplication` condition and Events | implemented |
| Application placements on warm pools (cache manager) | implemented |
| Artifact on new warm replicas, failure hold | implemented |
| Runtime verification, copy and `spin trigger http` start | implemented |

Validated on the amd64 KVM lab only. arm64 follows the same path but has
not been run on arm64 hardware.

## What changes for a replica

For an eligible SpinApp, each new replica checked out of a warm pool
carries one more KubeSwift artifact: its own `spec.image`, mounted
read-only as an OCI image layout at `/opt/kubeswift-spin-app`, and the
expected manifest digest in `KUBESWIFT_SPIN_APP_DIGEST`. The replica's
arguments are unchanged.

Before Spin starts, the runtime entrypoint:

1. checks that the mount is read-only and that the layout lists the
   expected digest;
2. reads every file with `openat2` beneath the mount (no symlinks, no
   other mounts), decodes the JSON strictly, and hashes the manifest, the
   locked application and every blob it uses against their digests;
3. copies those blobs into the guest's tmpfs while hashing them, so Spin
   never reads application code or files from the mount;
4. writes the locked application as `spin up` would, and starts
   `spin trigger http` with it.

Any failed check, a deadline (5 seconds), too little guest memory for the
copy, or an application feature the local runner does not support leads to
`spin up --from` with the same arguments and environment as with the
feature off. The replica's console says which path ran:

```text
kubeswift-spin-entrypoint: local application used from /opt/kubeswift-spin-app
kubeswift-spin-entrypoint: local application not used: <reason>
```

Read it with `kubectl logs <launcher pod> -c console` (KubeSwift chart
value `sandboxMaterialize.console=true`).

The runtime supports applications whose triggers are all `http`, without
component dependencies, with component sources and static files from the
artifact's layers. Anything else falls back.

## Requirements

- KubeSwift v0.19.3 or later. kubeswift-spin relies on optional artifacts
  being skipped, not failing the sandbox, and on a warm re-dispatch when a
  node does not hold the artifact (kubeswift-io/kubeswift#843, #844).
  kubeswift-spin detects the fields it needs from the published schema, but
  cannot tell v0.19.3 from an older v0.19 release: upgrade KubeSwift first.
  Without the fields, SpinApps report `LocalApplication=False/Unsupported`.
- KubeSwift's node artifact cache bounded: KubeSwift chart value
  `sandboxMaterialize.artifactCache.maxBytes` or `maxPercent`. Tenants'
  application artifacts are placed on every node of a pool, in a disk all
  tenants share. The chart refuses `localApplication.enabled` unless
  `localApplication.nodeCacheBounded=true` records that it is. KubeSwift
  limits each artifact to 10 GiB by default.
- The runtime image pinned by digest (`runtimeImage.digest`), and any other
  runtime image an executor names listed in
  `localApplication.extraRuntimes`. The converter supports exactly the Spin
  build in kubeswift-spin's runtime image (Spin 4.2.5); a runtime image with
  another Spin must not be listed.
- A warm pool on the executor (`spin.kubeswift.io/sandbox-pool`). Replicas
  of executors without a pool get no artifact (`NoWarmPool`).
- For a registry behind a private CA: KubeSwift's
  `sandboxMaterialize.registryCABundle` must trust it (KubeSwift fetches
  the artifact), and so must the runtime image (the fallback path pulls in
  the guest).

## Turning it on

Three layers, all required:

1. Platform, in the kubeswift-spin chart:

   ```yaml
   localApplication:
     enabled: true
     nodeCacheBounded: true
   ```

2. Administrator: label each namespace that may use it. The built-in
   namespace `admin` role cannot change Namespace labels, so tenants cannot
   enable it for themselves.

   ```bash
   kubectl label namespace <namespace> spin.kubeswift.io/local-application=enabled
   ```

3. Executor: the annotation `spin.kubeswift.io/local-application: enabled`
   on the `SpinAppExecutor`.

A SpinApp can opt out with `spin.kubeswift.io/local-application: disabled`.
It cannot opt in on its own. Turning the feature on or off at any layer
never replaces running replicas; it affects new replicas only.

Other chart values:

| Value | Default | Meaning |
|---|---|---|
| `localApplication.extraRuntimes` | `[]` | runtime images, besides `runtimeImage`, allowed to run local applications (`repository@sha256:...`) |
| `localApplication.registries` | `[]` | when set, the only registry hosts eligible; empty allows any registry KubeSwift reaches over HTTPS |
| `localApplication.maxPlacementsPerNamespace` | `8` | application placements per namespace, retained ones included |
| `localApplication.maxNewPerHour` | `10` | placements a namespace may have created in the last hour |
| `localApplication.retention` | `24h` | how long a placement no SpinApp wants is kept, so that a rollback finds it |

With `localApplication.registries` empty, KubeSwift's control plane
contacts the registry hosts tenants name in `spec.image`. Set it where that
is not acceptable.

## Eligibility

A SpinApp's new replicas get the artifact only when:

- `spec.image` is pinned by digest (`repository@sha256:...`). A tag gets
  no artifact (`ImageNotPinned`): the controller has no registry
  credentials to resolve it, and a tag can move.
- The registry is one KubeSwift reaches over HTTPS. `localhost`, names
  under `.localhost`, and loopback or private-range IP addresses are
  fetched over plain HTTP by KubeSwift and are never eligible
  (`RegistryNotEligible`); in-cluster names such as
  `registry.ns.svc.cluster.local` are fetched over HTTPS and stay eligible.
- The SpinApp has at most one image pull Secret, and none when the executor
  sets a runtime image pull Secret (`PullSecrets`).
- The SpinApp selects no components (`ComponentsSelected`).
- The executor's runtime image is allowed (`RuntimeNotAllowed`).
- The pool's application placement reports every node prepared
  (`Placing`, `PlacementFailed`, `PlacementLimit`, `PlacementBlocked`).
- No failure hold is active (`DeliveryFailed`).

## Placements

The cache manager keeps one `SwiftArtifactPlacement` per namespace, warm
pool, image and pull Secret name that at least one eligible SpinApp with
replicas wants. Its name is `kss-app-` and a hash of that key; it carries
the labels `app.kubernetes.io/managed-by=kubeswift-spin` and
`spin.kubeswift.io/component=application-placement`. KubeSwift fetches the
artifact once per node with the SpinApp's pull Secret and keeps it in its
node artifact cache.

The controller attaches the artifact to new replicas only once the
placement reports `Prepared` for its current generation. A placement it did
not create, or whose spec does not match the key, is never trusted or
modified (`PlacementBlocked`).

A placement no SpinApp wants any more is kept for
`localApplication.retention` and then deleted. At the per-namespace limit,
retained placements are deleted first; SpinApps beyond either limit report
`PlacementLimit` and start from the registry.

## Credentials and revocation

The artifact and the placement name the SpinApp's pull Secret; neither the
controller nor the cache manager reads it. KubeSwift resolves the artifact
with that Secret for every new replica, and reuses a node's verified copy
for a repository only after the repository served the digest with the
replica's own credentials. Removing or invalidating the Secret, revoking
access at the registry, or deleting the artifact there makes new replicas
fall back to the registry path (which then fails to pull, as without the
feature) within KubeSwift's authorization TTL (default 5 minutes). Running
replicas keep their mount.

On the local path the entrypoint does not write Spin's registry
credentials into the guest home, so neither Spin nor the application's
components hold them.

## What a SpinApp reports

The `LocalApplication` condition is present only for SpinApps whose
executor enables the feature and that did not opt out. It never affects
`Available`, `Progressing` or `readyReplicas`.

| Status | Reason | Meaning |
|---|---|---|
| True | `Attached` | new warm replicas carry the application artifact |
| False | `NotConfigured` | the executor asks, but the chart value is off |
| False | `NamespaceNotEnabled` | the namespace has no `spin.kubeswift.io/local-application=enabled` label |
| False | `Unsupported` | KubeSwift's schema lacks the artifact fields or placements |
| False | `ImageNotPinned`, `RegistryNotEligible`, `PullSecrets`, `ComponentsSelected`, `RuntimeNotAllowed`, `NoWarmPool` | see [Eligibility](#eligibility) |
| False | `Placing` | the placement is not prepared on every node yet |
| False | `PlacementFailed`, `PlacementLimit`, `PlacementBlocked` | see [Placements](#placements) |
| False | `DeliveryFailed` | a replica that carried the artifact failed; new replicas start from the registry for 30 minutes |

`True/Attached` says what new replicas carry, not that Spin used the copy;
the replica's console says that. Events `LocalApplicationReady`,
`LocalApplicationSkipped` and `LocalApplicationFailed` fire once per change
of the reason. Metrics are listed in [observability.md](observability.md).

## Failure hold

A replica that carried the artifact and failed before KubeSwift reported it
`WorkloadReady` (other than for its runtime image), or that KubeSwift
failed for an artifact reason, starts a 30-minute hold for its SpinApp: new replicas get no artifact. The hold is
kept in the controller's memory and ends on its own or when the controller
restarts.

## Rollouts

The artifact is added after the revision fingerprint, so it never changes a
SpinApp's revision, and a placement becoming ready, failing or going away
never replaces a healthy replica. During a rollout from A to B, each new B
replica uses the local copy if B's placement is prepared when the replica
is created; otherwise it starts from the registry.

## Upgrading and turning it off

- Upgrade KubeSwift to v0.19.3 or later before kubeswift-spin.
- Remove `spin.kubeswift.io/local-application` from executors before
  rolling the controller back below v0.3.0: older controllers reject
  unknown `spin.kubeswift.io/` executor annotations.
- A runtime image older than v0.3.0 ignores the artifact and starts from
  the registry.
- Setting `localApplication.enabled=false`, removing the namespace label or
  the executor annotation stops new replicas from carrying the artifact.
  The cache manager stops managing application placements when the feature
  is off; delete leftover placements with:

  ```bash
  kubectl delete swiftartifactplacements -A -l spin.kubeswift.io/component=application-placement
  ```

## Limits of this revision

- Only warm replicas. A pool replica that boots cold because no slot is
  free keeps the artifact and uses it when its node holds it.
- The local path starts Spin's hidden `spin trigger http` subcommand, which
  has no documented stability. kubeswift-spin pins the exact Spin build the
  converter supports. A supported way to run a pre-resolved application is
  proposed upstream as
  [spinframework/spin#3753](https://github.com/spinframework/spin/issues/3753).
- Applications with non-HTTP triggers, component dependencies or selected
  components start from the registry.
