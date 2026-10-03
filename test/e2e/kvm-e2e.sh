#!/usr/bin/env bash
# KVM end-to-end test: runs the hello-http example in real KubeSwift
# microVMs. Requires a cluster prepared as described in test/e2e/README.md.
#
# Phases:
#   execution         SpinApp -> SwiftSandbox -> Running -> Spin serving,
#                     scale up, scale down, delete
#   network exposure  Service reachability; gated until KubeSwift supports
#                     inbound port exposure for SwiftSandbox
#
# Environment:
#   E2E_NAMESPACE   namespace with the kubeswift executor and a Ready
#                   SwiftKernel "sandbox" (default kubeswift-spin-e2e)
#   APP_IMAGE       hello-http Spin OCI artifact reachable from the sandbox
#                   without credentials
#   EXECUTOR        executor name (default kubeswift)
#   TIMEOUT         seconds to wait for each step (default 300)
set -euo pipefail

NS="${E2E_NAMESPACE:-kubeswift-spin-e2e}"
APP_IMAGE="${APP_IMAGE:-ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http:v0.1.0}"
EXECUTOR="${EXECUTOR:-kubeswift}"
TIMEOUT="${TIMEOUT:-300}"
APP=hello-e2e
FAILED=0

log() { printf '\n==> %s\n' "$*"; }
ok() { printf 'ok: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAILED=1; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

until_true() {
  local what="$1"
  shift
  local deadline=$((SECONDS + TIMEOUT))
  while ((SECONDS < deadline)); do
    if "$@" >/dev/null 2>&1; then ok "$what"; return 0; fi
    sleep 3
  done
  fail "timed out after ${TIMEOUT}s: $what"
  return 1
}

phase() { kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
is_running() { [[ "$(phase "$1")" == "Running" ]]; }
serving() { swiftctl -n "$NS" sandbox logs "$1" 2>/dev/null | grep -q "Serving http://0.0.0.0:3000"; }
reason() { kubectl -n "$NS" get spinapp "$APP" -o jsonpath="{.status.conditions[?(@.type==\"$1\")].reason}" 2>/dev/null; }
has_reason() { [[ "$(reason "$1")" == "$2" ]]; }
count() { kubectl -n "$NS" get swiftsandbox -l "spin.kubeswift.io/app=$APP" -o name 2>/dev/null | wc -l; }
has_count() { [[ "$(count)" -eq "$1" ]]; }

cleanup() {
  if [[ $FAILED -ne 0 ]]; then
    kubectl -n "$NS" get spinapp,swiftsandbox -o wide 2>/dev/null || true
    kubectl -n "$NS" describe spinapp "$APP" 2>/dev/null | tail -30 || true
  fi
  kubectl -n "$NS" delete spinapp "$APP" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "preflight"
for crd in spinapps.core.spinkube.dev spinappexecutors.core.spinkube.dev swiftsandboxes.sandbox.kubeswift.io swiftkernels.kernel.kubeswift.io; do
  kubectl get crd "$crd" >/dev/null 2>&1 || die "CRD $crd is not installed"
done
ok "required CRDs present"
command -v swiftctl >/dev/null || die "swiftctl is not on PATH (needed to read the guest console)"
kubectl get nodes -l kubeswift.io/kernel-node=true -o name | grep -q . || die "no node labelled kubeswift.io/kernel-node=true"
ok "kernel node present"
[[ "$(kubectl -n "$NS" get swiftkernel sandbox -o jsonpath='{.status.phase}' 2>/dev/null)" == "Ready" ]] \
  || die "SwiftKernel sandbox is not Ready in namespace $NS"
ok "SwiftKernel sandbox Ready"
[[ "$(kubectl -n "$NS" get spinappexecutor "$EXECUTOR" -o jsonpath='{.metadata.labels.spin\.kubeswift\.io/managed-by}' 2>/dev/null)" == "kubeswift-spin" ]] \
  || die "SpinAppExecutor $NS/$EXECUTOR is missing or not managed by kubeswift-spin"
ok "executor $EXECUTOR managed by kubeswift-spin"
kubectl get deploy -A -l app.kubernetes.io/name=kubeswift-spin -o jsonpath='{.items[0].status.availableReplicas}' | grep -q '^[1-9]' \
  || die "kubeswift-spin controller is not available"
ok "kubeswift-spin controller available"

log "execution: deploy $APP with 1 replica"
kubectl -n "$NS" delete spinapp "$APP" --ignore-not-found --wait=true >/dev/null
cat <<YAML | kubectl -n "$NS" apply -f - >/dev/null
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: $APP
spec:
  image: $APP_IMAGE
  executor: $EXECUTOR
  replicas: 1
YAML
until_true "SwiftSandbox $APP-0 created" kubectl -n "$NS" get swiftsandbox "$APP-0"
until_true "$APP-0 reached Running" is_running "$APP-0"
until_true "Spin is serving inside $APP-0 (guest console)" serving "$APP-0"
until_true "SpinApp Progressing=SandboxRunning" has_reason Progressing SandboxRunning

log "execution: scale to 2"
kubectl -n "$NS" patch spinapp "$APP" --type=merge -p '{"spec":{"replicas":2}}' >/dev/null
until_true "$APP-1 reached Running" is_running "$APP-1"
until_true "Spin is serving inside $APP-1" serving "$APP-1"

log "execution: scale to 1"
kubectl -n "$NS" patch spinapp "$APP" --type=merge -p '{"spec":{"replicas":1}}' >/dev/null
until_true "$APP-1 removed" has_count 1
is_running "$APP-0" && ok "$APP-0 kept running" || fail "$APP-0 disturbed by scale down"

log "network exposure"
if kubectl get --raw /openapi/v3/apis/sandbox.kubeswift.io/v1alpha1 2>/dev/null | grep -q '"podMetadata"'; then
  echo "SKIP: the installed KubeSwift advertises sandbox port exposure, but this kubeswift-spin release does not use it yet."
else
  echo "SKIP: SwiftSandbox has no inbound port exposure in the installed KubeSwift."
  echo "      HTTP reachability through a Service cannot be tested; see docs/upstream/kubeswift-sandbox-service-exposure.md."
fi
until_true "SpinApp Available=NetworkUnavailable (not falsely Ready)" has_reason Available NetworkUnavailable

log "execution: delete"
kubectl -n "$NS" delete spinapp "$APP" --wait=true >/dev/null
until_true "all sandboxes removed" has_count 0

if [[ $FAILED -ne 0 ]]; then
  echo "KVM e2e FAILED" >&2
  exit 1
fi
log "KVM execution e2e passed (network exposure skipped, see above)"
