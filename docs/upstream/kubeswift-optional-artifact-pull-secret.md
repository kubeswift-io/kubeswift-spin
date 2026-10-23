# KubeSwift requirement: an optional artifact never fails its sandbox

Status: proposal, not filed. Found while implementing the compiled Wasmtime
cache ([ADR 0014](../adr/0014-compiled-wasmtime-cache.md)) on KubeSwift
v0.19.1.

## User problem

`optional: true` on a SwiftSandbox artifact promises that the workload
starts without the artifact when it cannot be resolved, fetched or
verified in time. A platform uses that for content that only speeds a
workload up, such as a cache, and relies on it never stopping the
workload.

In v0.19.1 one case breaks the promise. Before resolving any artifact, the
controller reads the pull Secret of every artifact (`artifactRequests`,
`pullSecretAuth`). When that Secret is missing, has no
`.dockerconfigjson`, or cannot be read, the error is a refusal, and both
the cold path and the warm checkout fail the sandbox with
`ArtifactResolveFailed`. The per-artifact optional handling is never
reached. A deleted or mistyped Secret, or a namespace set up before its
Secret exists, therefore stops every new sandbox that names it, although
the artifact is optional.

## Proposed behavior

- For an optional artifact, a pull Secret that cannot be read skips the
  artifact: `status.artifacts[].state: Skipped` with a reason such as
  `PullSecretInvalid` and a message naming the Secret, not its content.
  The workload starts without it, as for any other optional failure.
- A required artifact keeps failing the sandbox, as today.
- The `kubeswift_sandbox_artifacts_total` metric counts the skip under
  its reason.

## Why this belongs in KubeSwift

The Secret read happens in KubeSwift, with KubeSwift's credentials. A
client cannot check the Secret itself without reading Secrets, which a
platform controller should not need to do. The need is generic: any
optional artifact read with its own credential.

## kubeswift-spin until then

The controller stops adding the cache to a SpinApp's new replicas for 30
minutes after one of them failed because of it, and reports
`CompiledCache=False` with reason `DeliveryFailed`
([compiled-cache.md](../compiled-cache.md#fallback)). One replica start
is lost each time.
