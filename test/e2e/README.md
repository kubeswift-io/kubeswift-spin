# KVM end-to-end test

`kvm-e2e.sh` runs the hello-http example in real KubeSwift microVMs. It is
the only test in this repository that exercises execution; the envtest and
kind tests stand in for KubeSwift.

It has not been run against KubeSwift v0.15.1 for the current release.

## Phases

| Phase | Checks | Status |
|---|---|---|
| execution | SpinApp creates SwiftSandbox; sandbox reaches `Running`; Spin prints `Serving http://0.0.0.0:3000` on the guest console; scale to 2 and back to 1; delete removes all sandboxes | runnable |
| network exposure | Service routes to Spin; HTTP response is correct; SpinApp becomes `Available` | gated: SwiftSandbox has no inbound port exposure in KubeSwift v0.15.1; the script prints SKIP and checks that the SpinApp reports `NetworkUnavailable` instead of Ready |

## Requirements

- A Linux Kubernetes cluster with at least one worker that has `/dev/kvm`
  and the label `kubeswift.io/kernel-node=true`
- KubeSwift v0.15.1 or later, including the sandbox CRDs
- cert-manager and Spin Operator v0.6.1
- kubeswift-spin installed from `charts/kubeswift-spin` with an executor
  named `kubeswift` in the test namespace
- a Ready SwiftKernel named `sandbox` in the test namespace
- `swiftctl` on `PATH`, to read the guest console
- the hello-http artifact pushed to a registry the sandbox can reach without
  credentials (`make example-push EXAMPLE=hello-http ...`)

## Run

```bash
E2E_NAMESPACE=kubeswift-spin-e2e APP_IMAGE=<registry>/hello-http:<tag> make e2e
```

The script uses the current kubeconfig context. It deletes the SpinApp it
created when it exits.

## CI

`.github/workflows/kvm-e2e.yaml` runs this script on a self-hosted runner
labelled `kvm`, on manual dispatch only. Public GitHub runners do not
provide KubeSwift-capable KVM nodes, so pull requests do not depend on it.
The workflow has not been run.
