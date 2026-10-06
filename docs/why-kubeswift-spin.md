# Why kubeswift-spin

The obvious question: why put a microVM around a WebAssembly application?
Wasm is already a sandbox, and Spin is designed to start applications in
milliseconds at a small memory cost.

Spin's sandbox is a good isolation boundary. kubeswift-spin adds a second,
different kind of boundary for the cases where one software boundary is not
enough. It is not intended to make every Spin workload faster or lighter: it
makes each replica slower to start and more expensive to run than standard
SpinKube execution.

## When standard SpinKube is the better choice

Use the standard SpinKube executors (containerd-shim-spin with runwasi, or
the Spin runtime in a container) when:

- you trust the applications, for example your own services built from
  your own source;
- the isolation of Wasmtime plus a Linux container on a shared node kernel
  is sufficient for your threat model;
- replica startup time and density matter most: a standard executor starts
  Spin without booting a virtual machine, while a kubeswift-spin replica
  boots a microVM first, or takes one from a warm pool. On the lab cluster
  a new replica returned its first direct response (to the pod IP) 9.9
  seconds after SpinApp creation when it booted a microVM and 2.8 seconds
  from a warm pool, and its first response through the Service after 13.0
  and 5.2 seconds (medians, see [performance.md](performance.md));
- a microVM adds no meaningful protection for what you run.

## Startup time is not request latency

The startup times above are paid when a replica is created: on deployment,
scale-out, a rolling update or the replacement of a failed replica. A
running replica keeps its microVM and its Spin process, and serves every
request from them; no microVM is booted per request.

kubeswift-spin is built for strong isolation first. The project's goal is
a first direct response from a warm pool in under one second; on the lab
cluster it is 2.81 seconds (p50) today. The measured stages, and which of them are
fixed waits rather than work, are in [performance.md](performance.md).

## When kubeswift-spin is useful

kubeswift-spin is for running Wasm that you do not fully trust, or that a
policy says must not share a kernel with the node:

- third-party Wasm components and plugins;
- tenant-provided code on a shared platform;
- AI-generated workloads and code produced by agents;
- MCP and other tool servers that act on behalf of a model;
- platforms whose policy requires VM-backed workload isolation;
- workloads that must be separated from the Kubernetes node: Spin and the
  application run on the guest kernel, not the node kernel;
- workloads that benefit from KubeSwift's per-sandbox network policy
  (restricted egress with an allowlist, ingress restricted to chosen
  peers) and compute shape (CPU, memory, kernel, warm pools).

The user API stays the standard `SpinApp`, so moving an application between
a standard executor and a KubeSwift executor is a change of
`spec.executor`, within the limits listed in
[compatibility.md](compatibility.md). The switch causes downtime: Spin
Operator deletes the application's Deployment at once, and the application
is unreachable until the first sandbox is ready.

## Trust model

Each replica has these layers:

```
Wasm component
      |
      v
Spin / Wasmtime            (Wasm sandbox, Spin capabilities: allowed hosts,
      |                     declared variables and files only)
      v
guest userspace            (entrypoint drops root; Spin runs as UID 65532
      |                     with no_new_privs)
      v
KubeSwift microVM          (Cloud Hypervisor, guest kernel, per-sandbox
      |                     NetworkPolicy and in-pod egress rules)
      v
KVM / host                 (node kernel, launcher pod, kubelet)
```

An attacker in a Wasm component must first escape Wasmtime or abuse Spin.
That gives them the Spin process inside the guest, which holds the
SpinApp's own registry credentials and Secret-backed configuration (see
[security-model.md](security-model.md#secrets)). To reach the node or
other workloads they must then escape the guest through the hypervisor or
its virtual devices, or use the guest's network, which KubeSwift restricts.

What this does not give you:

- Immunity. A vulnerability in Wasmtime, Spin, the guest kernel, Cloud
  Hypervisor, KVM, the KubeSwift launcher or the host kernel can still be
  exploited. The layers make a full escape require more than one
  vulnerability; they do not rule it out.
- Protection of a SpinApp from itself. Everything inside one guest belongs
  to one SpinApp: a compromised Spin process can read that application's
  credentials and configuration.
- Protection against a malicious cluster administrator, node owner or
  anyone who can read the namespace's Secrets.
- Supply-chain guarantees for the application. kubeswift-spin does not
  verify application artifact signatures; pin applications by digest.

## Costs

- Startup: a cold microVM boot plus the Spin pull and start, instead of a
  container start. Warm pools reduce it but keep booted guests idle.
- Memory and CPU: each replica reserves a whole guest (1 CPU and 512Mi
  with the chart's default executor settings), much more than a Spin
  application needs in a shared runtime.
- Nodes: replicas run only on nodes with KVM that KubeSwift manages.
- Operations: KubeSwift, its kernels and its images become part of the
  platform you operate.

## See also

- [Architecture](architecture.md)
- [Security model](security-model.md)
- [Networking](networking.md)
