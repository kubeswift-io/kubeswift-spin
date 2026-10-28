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

Local applications are new in kubeswift-spin v0.3.0 and need
KubeSwift v0.19.3 or later. The KVM e2e passed on the amd64 lab with
KubeSwift v0.20.0, also with the v0.3.0-rc1 release artifacts (see
[compatibility.md](compatibility.md#validation-of-v030-rc1)). arm64
follows the same path but is untested.

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

When KubeSwift skipped the artifact, for example because the node does not
hold it, the reason is `no application is mounted`.

On KubeSwift v0.20.0 or later the line is in the console container of the
replica's launcher pod (KubeSwift chart value `sandboxMaterialize.console`,
default `true`). A warm replica's line appears while Spin runs only with
SwiftKernel `kernels/sandbox:6.6.18` or later; with an older kernel it
appears when Spin exits. Find the launcher pod:

```bash
kubectl -n <namespace> get swiftsandbox <replica> -o jsonpath='{.status.podRef}'
```

Then read its console:

```bash
kubectl -n <namespace> logs <launcher pod> -c console
```

The runtime supports applications whose triggers are all `http`, without
component dependencies, with component sources and static files from the
artifact's layers. Anything else falls back.

## Requirements

- KubeSwift v0.19.3 or later. kubeswift-spin relies on optional artifacts
  being skipped, not failing the sandbox, and on a warm re-dispatch when a
  node does not hold the artifact (kubeswift-io/kubeswift#843, #844).
  kubeswift-spin detects the fields it needs from the published schema, but
  cannot tell v0.19.3 from an older v0.19 release: upgrade KubeSwift first.
  Without the fields or the SwiftArtifactPlacement API, SpinApps report
  `LocalApplication=False/Unsupported`.
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
| `localApplication.registries` | `[]` | when set, the only registry hosts eligible, written as images normalize them (Docker Hub is `index.docker.io`; `docker.io` is refused at startup); empty allows any registry KubeSwift reaches over HTTPS |
| `localApplication.maxPlacementsPerNamespace` | `8` | application placements per namespace, retained ones included |
| `localApplication.maxNewPerHour` | `10` | placements a namespace may have created in the last hour |
| `localApplication.retention` | `24h` | how long a placement no SpinApp wants is kept, so that a rollback finds it |

With `localApplication.registries` empty, KubeSwift's control plane and
its fetch pods contact the registry hosts tenants name in `spec.image`,
over HTTPS, with the tenant's pull Secret. That includes in-cluster names
such as `<service>.<namespace>.svc.cluster.local` in other namespaces, so
tenants can make KubeSwift send registry requests to hosts their own
egress rules would not reach, and learn from the placement's reason
whether a host answers like a registry. Set `registries` to the registries
tenants should use wherever that matters.

## Eligibility

A SpinApp's new replicas get the artifact only when:

- `spec.image` is pinned by digest (`repository@sha256:...`). A tag gets
  no artifact (`ImageNotPinned`): the controller has no registry
  credentials to resolve it, and a tag can move.
- The registry is one KubeSwift reaches over HTTPS. Registries KubeSwift
  may reach over plain HTTP, and other local addresses, are never eligible
  (`RegistryNotEligible`): `localhost`, names under `.localhost`, and
  loopback, private-range, link-local or unspecified IP addresses.
  In-cluster names such as `registry.ns.svc.cluster.local` are fetched over
  HTTPS and stay eligible.
- The SpinApp names at most one image pull Secret, and exactly one when the
  executor sets `spin.kubeswift.io/runtime-image-pull-secret`; otherwise
  KubeSwift would fetch the application with the runtime credential
  (`PullSecrets`).
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
placement reports `Prepared` for its current generation. It trusts only a
placement whose labels, key annotation, name and key fields match the
SpinApp's key; anything else at that name is never trusted or modified
(`PlacementBlocked`).

A placement no SpinApp wants any more is kept for
`localApplication.retention` and then deleted. At the per-namespace limit,
retained placements are deleted first, but a placement created within the
last hour is kept for that hour, so a namespace creates at most
`maxPlacementsPerNamespace` (or `maxNewPerHour`, if smaller) placements per
hour. SpinApps that already have a placement keep it; a SpinApp that starts
wanting one at the limit reports `PlacementLimit` and starts from the
registry.

A placement that is still missing two minutes after the controller first
expected it makes the SpinApp report `PlacementBlocked` with a Warning
Event. The cache manager logs why (an API server refusal, such as a quota,
or another object at the placement's name).

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
the replica's console says that. When the reason changes, the controller
records `LocalApplicationReady` (Normal, `Attached`),
`LocalApplicationFailed` (Warning: `PlacementFailed`, `PlacementBlocked`,
`DeliveryFailed`) or `LocalApplicationSkipped` (Normal: the other reasons
except `Placing`, which has no Event). Metrics are listed in
[observability.md](observability.md).

## Failure hold

A replica that carried the artifact and failed before KubeSwift reported it
`WorkloadReady` (other than for its runtime image), or that KubeSwift
failed for an artifact reason, starts a 30-minute hold for its SpinApp: new replicas get no artifact. The hold is
kept in the controller's memory. It ends on its own or when the controller
restarts; after a restart, a failed replica that carried the artifact and
still exists starts it again.

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
  unknown `spin.kubeswift.io/` executor annotations. They also keep the
  `LocalApplication` condition as it was, and leave application placements
  in place; delete them with:

  ```bash
  kubectl delete swiftartifactplacements -A -l spin.kubeswift.io/component=application-placement
  ```

- A runtime image older than v0.3.0 ignores the artifact and starts from
  the registry.
- Setting `localApplication.enabled=false`, removing the namespace label or
  the executor annotation stops new replicas from carrying the artifact.
  With the namespace label or the executor annotation removed, the cache
  manager deletes the unwanted placements after
  `localApplication.retention`. With `localApplication.enabled=false` the
  controller deletes every application placement at once, including those
  that v0.3.0 left behind when the feature was turned off; placements of
  compiled caches are not affected. Running replicas keep their mount.

## Limits of this revision

- Only warm replicas gain. A pool replica that boots cold because no slot
  is free keeps the artifact and uses it when its node holds it, but the
  cold boot dominates: on the amd64 lab (hello-http, a one-slot pool, 2026-10-25),
  replicas that booted cold were ready 12 to 14 seconds after creation from
  the registry and 11 to 16 seconds with the local application (six each,
  measured from KubeSwift's `WorkloadReady` condition, one-second
  resolution), against 1 to 2 seconds for the checked-out replicas.
- A pool with `minWarm: 0` gets no local application: KubeSwift prepares a
  placement only on nodes that run one of the pool's pods, so the
  placement stays `NoTargetNodes` and its SpinApps report `Placing`
  ([upstream/kubeswift-placement-target-nodes.md](upstream/kubeswift-placement-target-nodes.md)).
- The local path starts Spin's hidden `spin trigger http` subcommand, which
  has no documented stability. kubeswift-spin pins the exact Spin build the
  converter supports. A supported way to run a pre-resolved application is
  proposed upstream as
  [spinframework/spin#3753](https://github.com/spinframework/spin/issues/3753).
- Applications with non-HTTP triggers, component dependencies or selected
  components start from the registry.
