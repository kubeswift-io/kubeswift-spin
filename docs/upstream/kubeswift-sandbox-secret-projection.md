# KubeSwift requirement: secure Secret projection into SwiftSandbox

Status: proposal for KubeSwift. Not implemented in KubeSwift v0.15.1.

## User problem

Most real workloads need credentials: database URLs, API tokens, registry
credentials, TLS material. A SwiftSandbox has no way to receive a Secret
without the value first being written in plain text into a Kubernetes object
that is not a Secret.

## Why this is generic KubeSwift functionality

Every sandbox workload that talks to an authenticated service needs this:
CI runners, agents, inference clients, Spin applications. The guest is only
reachable through KubeSwift's launcher and boot path, so only KubeSwift can
deliver data into it.

## Why it does not belong in kubeswift-spin

The only channels kubeswift-spin has into a sandbox are `env`, `command`
and `args`, and KubeSwift stores all three in plain text (see below).
Reading Secrets in kubeswift-spin and copying values into those fields would
leak them to anyone who can read ConfigMaps or SwiftSandboxes in the
namespace, and would require cluster-wide Secret read access for the
controller. kubeswift-spin instead rejects secret-backed SpinApp
configuration with `UnsupportedConfiguration` and has no Secret permissions.

## Current behavior (v0.15.1)

- `spec.env` is `[]corev1.EnvVar`, but the controller merges only literal
  values: `valueFrom` is silently dropped, and the variable is set to an
  empty string.
- The merged environment, command and arguments are serialized into the
  `<sandbox>-runtime-intent` ConfigMap, which the launcher mounts.
- `imagePullSecret` and `verifyKeySecretRef` are read by KubeSwift on the
  host side for rootfs pulls and verification; their contents never enter
  the guest.

## Proposed API

```yaml
spec:
  env:
    - name: DATABASE_URL
      valueFrom:
        secretKeyRef: {name: db, key: url}      # now honored
  secretFiles:                                  # optional file form
    - secretName: registry-auth
      items:
        - key: config.json
          path: /run/secrets/registry/config.json
      mode: 0400
```

Two delivery forms cover the common cases: environment variables for
single values, files for structured credentials (Docker config, TLS keys,
cloud credential files).

## Controller behavior

- Resolve `secretKeyRef` at launch on the node side, not in the controller,
  and pass values to the guest over the existing vsock channel or a
  memory-only config disk.
- Never write resolved values to the runtime-intent ConfigMap, the
  SwiftSandbox status, annotations, Events or logs. Keep only references in
  the intent.
- Mount `secretFiles` on a tmpfs in the guest with the requested mode.
- A missing Secret or key keeps the sandbox `Pending` with a condition that
  names the Secret and key, never the value.
- Apply the same on warm-pool checkout.

## Security considerations

- The launcher service account would need `get` on the referenced Secrets
  in the sandbox namespace, ideally limited by the referencing object
  rather than granting namespace-wide Secret read.
- Values exist in launcher memory and guest memory only.
- Anyone who can create a SwiftSandbox referencing a Secret can read it from
  inside the guest; this is the same trust as pod Secret references and
  should be documented as such.
- Rotation: the first version can deliver values at start only;
  replacement of the sandbox picks up new values.

## Compatibility implications

`valueFrom` currently degrades silently to an empty value; honoring it is a
behavior change that should be called out, and the webhook should reject
unsupported `valueFrom` sources (`fieldRef`, `resourceFieldRef`) instead of
dropping them. kubeswift-spin would then support SpinApp
`variables[].valueFrom.secretKeyRef`, `runtimeConfig.loadFromSecret`,
secret-backed runtime-config options, and executor `caCertSecret`.

## Tests required

- A secret-backed variable is visible to the guest workload and absent from
  the runtime-intent ConfigMap, SwiftSandbox object, Events and controller
  logs.
- `secretFiles` appear with the requested mode on a memory-backed
  filesystem.
- Missing Secret or key gives a Pending sandbox with an actionable
  condition.
- Warm-pool checkout delivers the same values.
- Unsupported `valueFrom` sources are rejected by the webhook.
