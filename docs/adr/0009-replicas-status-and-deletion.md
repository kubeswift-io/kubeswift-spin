# 0009: StatefulSet-like replicas, owned status fields, no finalizer

Status: accepted

## Context

SwiftSandbox specs are immutable, launcher pods never restart, and
SpinApp status is written by nobody else for external executors.

## Decision

- Replica `i` is the sandbox `<app>-<i>`. Changes are applied by deleting
  and recreating one available replica at a time; failed replicas are
  replaced with exponential backoff.
- A controller owner reference (by UID) is the only proof of ownership;
  labels are for selection. Foreign objects are never adopted.
- kubeswift-spin owns `activeScheduler`, `readyReplicas` and the
  `Available` and `Progressing` conditions, written with a merge patch that
  carries the resource version; other conditions are preserved.
- No finalizer: owner references and garbage collection clean up, and
  sandboxes are deleted with foreground propagation.

## Consequences

- Deterministic names and idempotent reconciliation; tested with envtest
  (including a controller restart) and kind.
- A single-replica app has a short gap during replacement.
- Deletion can never be blocked by kubeswift-spin.
