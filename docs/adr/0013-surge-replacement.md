# 0013: Replace replicas with one surge replica

Status: accepted. Amends [0009](0009-replicas-status-and-deletion.md).

## Context

ADR 0009 applied a configuration change by deleting replica `i` and
recreating it under the same name, one available replica at a time. A
single-replica SpinApp therefore had no ready endpoint from the moment the
old sandbox stopped serving until the new one passed its readiness check.
On the lab cluster, a single-replica rollout under sustained traffic lost
68 of about 105,000 requests.

SwiftSandbox specs are immutable, and replica names are part of the
documented contract (`<app>-<ordinal>`), so the replacement cannot reuse
the old name while the old sandbox still exists.

## Decision

- When a desired replica runs an outdated revision and is available, a
  rollout first creates one surge replica at ordinal `<replicas>`
  (`<app>-<replicas>`) with the new revision and the label
  `spin.kubeswift.io/surge=true`. It serves Service traffic like any
  replica.
- An outdated available replica is deleted only while the other available
  replicas, the surge replica included, still number `spec.replicas`.
  Replicas are replaced highest ordinal first, one at a time, as before.
- The surge replica is removed (as a scale-down) once every ordinal below
  `spec.replicas` runs the new revision and is available.
- A replica at ordinal `<replicas>` without the surge label is an ordinary
  scaled-down replica and is removed at once. The surge replica is created
  only once no other sandbox holds its name.
- If the spec changes again during a rollout, a surge replica of the
  earlier revision is replaced; while it is available and the desired
  replicas alone number fewer than `spec.replicas`, it is kept until they
  do. It counts toward `readyReplicas` meanwhile.
- If `spec.replicas` is raised so that the surge ordinal becomes a desired
  one, that replica is treated as outdated and replaced once, so the label
  never stays on a desired replica.
- A ready surge replica counts toward `readyReplicas`, capped at
  `spec.replicas`.
- Replica names do not change.

## Alternatives considered

- **New names per revision** (Deployment-style, `<app>-<revision>-<n>`).
  Gives surge and parallel rollouts naturally, but breaks the documented
  `<app>-<ordinal>` names that users, tooling and the `swiftctl` commands
  in status messages rely on, and needs a migration of existing replicas.
- **A configurable surge** (`maxSurge` greater than 1). SpinApp has no
  rollout strategy field, and an executor annotation for it would add API
  for a need nobody has reported. One surge replica bounds the extra
  capacity to one sandbox per SpinApp.
- **Keep delete-then-create and document the gap.** Rejected: a
  single-replica SpinApp, the most common case in examples, failed
  requests on every change.
- **Remove the old replica from the Service before deleting it.** It
  needs patching or relabeling the launcher pod, which kubeswift-spin must
  not do, or a Service and EndpointSlice of its own, which it must not
  create.

## Consequences

- No replica is taken away before its replacement is ready, so a rollout
  keeps `spec.replicas` ready replicas, including for a single replica.
- A rollout needs room for one extra sandbox. `--max-replicas` bounds
  `spec.replicas`, so a SpinApp runs at most `max-replicas + 1` sandboxes;
  the ordinal suffix reserve covers that name. Every SpinApp that rolls at
  the same time (for example after an upgrade that changes the rendered
  spec) needs that headroom. A create the API server rejects is reported as
  `SandboxRejected`. If the surge replica cannot
  start or never becomes ready, the rollout stalls with every old replica
  still serving; `Progressing` reports the failing sandbox, and the
  failed surge replica is replaced with the usual backoff.
- Old replicas leave the Service when their launcher pod starts
  terminating. kubeswift-spin relies on KubeSwift to keep a terminating
  sandbox from failing connections while kube-proxy catches up. On
  KubeSwift v0.16.2 this holds only because the guest ignores the ACPI
  power button that swiftletd presses on SIGTERM, so it keeps serving until
  the 30-second grace period ends and the launcher pod ends `Failed`. If a
  later kernel or KubeSwift version stops the guest promptly, connections
  routed during kube-proxy's next sync can fail. The KVM rollout check
  under traffic is what would catch that; the generic fix is proposed in
  [kubeswift-sandbox-termination-drain.md](../upstream/kubeswift-sandbox-termination-drain.md).
- While a deleted replica keeps serving, KubeSwift has already removed its
  NetworkPolicy, so its port is reachable from any pod in the cluster for
  up to the grace period unless the namespace has its own default-deny
  ingress policy (see
  [security-model.md](../security-model.md#network-isolation)). Rollouts
  make this window routine; it existed before for every deleted replica.
- Scaling during a rollout composes with it: a scale-down removes the
  highest ordinals (an unlabeled replica at the new surge ordinal first),
  and a scale-up replaces a former surge replica as described above.
- Downgrading to a release without this ADR during a rollout removes the
  surge replica as a scaled-down replica and finishes with
  delete-then-create.
- A controller restart resumes from the observed sandboxes and labels; the
  planner keeps no state of its own.
