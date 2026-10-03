#!/usr/bin/env bash
# Controller integration test on kind.
#
# Scope: API-level reconciliation against a real Kubernetes control plane
# (API server, garbage collector, admission) with the pinned SpinKube and
# KubeSwift CRDs, kubeswift-spin installed from its Helm chart. There is no
# KubeSwift controller and no KVM: the test writes SwiftSandbox status itself
# to stand in for KubeSwift. This is NOT an execution end-to-end test; see
# test/e2e for that.
#
# With WITH_SPIN_OPERATOR=1 the test also installs cert-manager and Spin
# Operator v0.6.1 and checks that they coexist: Spin Operator creates the
# SpinApp Service and leaves the status to kubeswift-spin.
#
# Environment:
#   IMAGE              controller image to load (required)
#   KIND_K8S_IMAGE     kind node image
#   CLUSTER            kind cluster name (default kubeswift-spin-it)
#   KEEP_CLUSTER=1     do not delete the cluster afterwards
#   WITH_SPIN_OPERATOR=1
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
IMAGE="${IMAGE:?set IMAGE to the controller image}"
KIND_K8S_IMAGE="${KIND_K8S_IMAGE:-kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a}"
CLUSTER="${CLUSTER:-kubeswift-spin-it}"
NS=kss-it
SYS=kubeswift-spin-system
CERT_MANAGER_VERSION=v1.21.2
CERT_MANAGER_SHA256=e03b668ec8675214af6b0a671699d088f2601fa3878e0dbe1b41d3feafd1879f
SPIN_OPERATOR_VERSION=v0.6.1
SPIN_OPERATOR_CHART_SHA256=640fcbf0da182ab0482228aa33872a06adec1d3849bea234ae8f725d1de60ff5
CTX="kind-$CLUSTER"
K="kubectl --context $CTX"
FAILED=0

log() { printf '\n==> %s\n' "$*"; }
ok() { printf 'ok: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAILED=1; }

cleanup() {
  if [[ $FAILED -ne 0 ]]; then
    log "diagnostics"
    $K -n "$SYS" logs deploy/kubeswift-spin --tail=100 2>/dev/null || true
    $K -n "$NS" get spinapp,swiftsandbox -o wide 2>/dev/null || true
    $K -n "$NS" get events.events.k8s.io --sort-by=.metadata.creationTimestamp 2>/dev/null | tail -30 || true
  fi
  if [[ "${KEEP_CLUSTER:-}" != "1" ]]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

# until <timeout-seconds> <description> <command...>
until_true() {
  local timeout="$1" what="$2"
  shift 2
  local deadline=$((SECONDS + timeout))
  while ((SECONDS < deadline)); do
    if "$@" >/dev/null 2>&1; then ok "$what"; return 0; fi
    sleep 1
  done
  fail "timed out: $what"
  return 1
}

jp() { $K -n "$NS" get "$1" -o jsonpath="$2" 2>/dev/null; }
cond() { jp "spinapp/$1" "{.status.conditions[?(@.type==\"$2\")].reason}"; }
# Names of live (not deleting) sandboxes of one SpinApp, sorted.
sandboxes() {
  $K -n "$NS" get swiftsandbox -l "spin.kubeswift.io/app=$1" -o json 2>/dev/null \
    | python3 -c 'import json,sys; print(" ".join(sorted(i["metadata"]["name"] for i in json.load(sys.stdin)["items"] if not i["metadata"].get("deletionTimestamp"))))'
}
has_sandboxes() { [[ "$(sandboxes "$1")" == "$2" ]]; }
has_reason() { [[ "$(cond "$1" "$2")" == "$3" ]]; }

set_running() {
  $K -n "$NS" patch swiftsandbox "$1" --subresource=status --type=merge \
    -p '{"status":{"phase":"Running"}}' >/dev/null
}

spinkube_crds="$(go -C "$ROOT" list -m -f '{{.Dir}}' github.com/spinkube/spin-operator)/config/crd/bases"
kubeswift_crds="$(go -C "$ROOT" list -m -f '{{.Dir}}' github.com/kubeswift-io/kubeswift)/config/crd/bases"

log "creating kind cluster $CLUSTER"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --image "$KIND_K8S_IMAGE" --wait 120s
kind load docker-image "$IMAGE" --name "$CLUSTER"

log "startup check: the controller refuses to start without the KubeSwift CRDs"
$K apply --server-side -f "$spinkube_crds" >/dev/null
go -C "$ROOT" build -o "$ROOT/bin/manager" ./cmd/manager
kubeconfig="$(mktemp)"
kind get kubeconfig --name "$CLUSTER" >"$kubeconfig"
set +e
out="$(timeout 60 "$ROOT/bin/manager" --kubeconfig "$kubeconfig" --metrics-bind-address=0 \
  --health-probe-bind-address=0 --runtime-image=ghcr.io/kubeswift-io/kubeswift-spin-runtime:v0.0.0-it 2>&1)"
code=$?
set -e
rm -f "$kubeconfig"
if [[ $code -ne 0 && "$out" == *"sandbox.kubeswift.io/v1alpha1"* && "$out" == *"install KubeSwift"* ]]; then
  ok "startup fails with an actionable error (exit $code)"
else
  fail "unexpected startup behavior (exit $code): $out"
fi

log "installing KubeSwift sandbox CRDs (no KubeSwift controller)"
$K apply --server-side -f "$kubeswift_crds/sandbox.kubeswift.io_swiftsandboxes.yaml" \
  -f "$kubeswift_crds/sandbox.kubeswift.io_swiftsandboxpools.yaml" >/dev/null
$K wait --for=condition=Established crd --all --timeout=60s >/dev/null
$K create namespace "$NS" >/dev/null

if [[ "${WITH_SPIN_OPERATOR:-}" == "1" ]]; then
  log "installing cert-manager $CERT_MANAGER_VERSION and Spin Operator $SPIN_OPERATOR_VERSION"
  dl="$(mktemp -d)"
  curl -fsSL --proto '=https' -o "$dl/cert-manager.yaml" \
    "https://github.com/cert-manager/cert-manager/releases/download/$CERT_MANAGER_VERSION/cert-manager.yaml"
  curl -fsSL --proto '=https' -o "$dl/spin-operator.tgz" \
    "https://github.com/spinframework/spin-operator/releases/download/$SPIN_OPERATOR_VERSION/spin-operator-${SPIN_OPERATOR_VERSION#v}.tgz"
  printf '%s  %s\n%s  %s\n' "$CERT_MANAGER_SHA256" "$dl/cert-manager.yaml" "$SPIN_OPERATOR_CHART_SHA256" "$dl/spin-operator.tgz" | sha256sum -c - >/dev/null
  $K apply -f "$dl/cert-manager.yaml" >/dev/null
  $K -n cert-manager wait --for=condition=Available deploy --all --timeout=180s >/dev/null
  helm --kube-context "$CTX" install spin-operator "$dl/spin-operator.tgz" --namespace spin-operator --create-namespace \
    --wait --timeout 300s >/dev/null
  rm -rf "$dl"
  ok "Spin Operator installed"
fi

log "installing kubeswift-spin from the Helm chart"
repo="${IMAGE%:*}"
tag="${IMAGE##*:}"
helm --kube-context "$CTX" install kubeswift-spin "$ROOT/charts/kubeswift-spin" --namespace "$SYS" --create-namespace \
  --set image.repository="$repo" --set image.tag="$tag" --set image.pullPolicy=Never \
  --set runtimeImage.tag=v0.0.0-it \
  --set "executors[0].name=kubeswift" --set "executors[0].namespaces={$NS}" \
  --set "executors[1].name=kubeswift-open" --set "executors[1].namespaces={$NS}" --set "executors[1].networkMode=open" \
  --set controller.maxReplicas=5 --wait --timeout 180s >/dev/null
ok "controller Deployment is ready"

log "create: SpinApp with 2 replicas"
cat <<YAML | $K apply -n "$NS" -f - >/dev/null
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: hello
spec:
  image: ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http:v0.1.0
  executor: kubeswift
  replicas: 2
  variables:
    - name: greeting
      value: hi
YAML
until_true 60 "sandboxes hello-0 and hello-1 created" has_sandboxes hello "hello-0 hello-1"
owner="$(jp swiftsandbox/hello-0 '{.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}/{.metadata.ownerReferences[0].controller}')"
[[ "$owner" == "SpinApp/hello/true" ]] && ok "sandbox controlled by the SpinApp" || fail "owner reference: $owner"
args="$(jp swiftsandbox/hello-0 '{.spec.args}')"
[[ "$args" == *'--from=ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http:v0.1.0'* ]] && ok "spin up arguments" || fail "args: $args"
[[ "$(jp swiftsandbox/hello-0 '{.spec.image}')" == "ghcr.io/kubeswift-io/kubeswift-spin-runtime:v0.0.0-it" ]] && ok "runtime image from Helm values" || fail "runtime image"
until_true 30 "Progressing=SandboxCreating" has_reason hello Progressing SandboxCreating

log "status: running sandboxes are not reported ready"
set_running hello-0
set_running hello-1
until_true 30 "Available=NetworkUnavailable" has_reason hello Available NetworkUnavailable
[[ "$(jp spinapp/hello '{.status.readyReplicas}')" == "0" ]] && ok "readyReplicas is 0" || fail "readyReplicas"
[[ "$(jp spinapp/hello '{.status.activeScheduler}')" == "kubeswift" ]] && ok "activeScheduler" || fail "activeScheduler"

if [[ "${WITH_SPIN_OPERATOR:-}" == "1" ]]; then
  log "coexistence with Spin Operator"
  until_true 30 "Spin Operator created the hello Service" $K -n "$NS" get service hello
  sel="$(jp service/hello '{.spec.selector}')"
  [[ "$sel" == *'core.spinkube.dev/app.hello.status'* ]] && ok "Service selector is Spin Operator's ($sel)" || fail "selector $sel"
  ! $K -n "$NS" get deploy hello >/dev/null 2>&1 && ok "no Deployment created" || fail "a Deployment exists"
  sleep 5
  has_reason hello Available NetworkUnavailable && ok "status still owned by kubeswift-spin" || fail "status overwritten"
fi

log "scale up to 3, then down to 1"
$K -n "$NS" patch spinapp hello --type=merge -p '{"spec":{"replicas":3}}' >/dev/null
until_true 60 "three sandboxes" has_sandboxes hello "hello-0 hello-1 hello-2"
$K -n "$NS" patch spinapp hello --type=merge -p '{"spec":{"replicas":1}}' >/dev/null
until_true 90 "scaled down to hello-0 (garbage collector finished foreground deletion)" \
  bash -c "[[ \$($K -n $NS get swiftsandbox -l spin.kubeswift.io/app=hello -o name | wc -l) -eq 1 ]]"

log "rolling replacement on image change"
uid="$(jp swiftsandbox/hello-0 '{.metadata.uid}')"
$K -n "$NS" patch spinapp hello --type=merge -p '{"spec":{"image":"ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http:v0.1.1"}}' >/dev/null
until_true 90 "hello-0 replaced at the new revision" \
  bash -c "[[ \$($K -n $NS get swiftsandbox hello-0 -o jsonpath='{.metadata.uid}' 2>/dev/null) != '' && \$($K -n $NS get swiftsandbox hello-0 -o jsonpath='{.metadata.uid}') != '$uid' ]]"

log "unsupported configuration is reported, not ignored"
cat <<YAML | $K apply -n "$NS" -f - >/dev/null
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: needs-secret
spec:
  image: ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http:v0.1.0
  executor: kubeswift
  replicas: 1
  variables:
    - name: database_url
      valueFrom:
        secretKeyRef: {name: db, key: url}
YAML
until_true 30 "Progressing=UnsupportedConfiguration" has_reason needs-secret Progressing UnsupportedConfiguration
msg="$(jp spinapp/needs-secret '{.status.conditions[?(@.type=="Progressing")].message}')"
[[ "$msg" == *'variable "database_url" uses secretKeyRef'* ]] && ok "actionable message" || fail "message: $msg"
[[ -z "$(sandboxes needs-secret)" ]] && ok "no sandbox created" || fail "sandbox created for unsupported config"
$K -n "$NS" get events.events.k8s.io --field-selector reason=UnsupportedConfiguration -o name | grep -q . \
  && ok "UnsupportedConfiguration event" || fail "no event"

log "replica limit"
cat <<YAML | $K apply -n "$NS" -f - >/dev/null
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: too-big
spec:
  image: ghcr.io/kubeswift-io/kubeswift-spin-examples/hello-http:v0.1.0
  executor: kubeswift
  replicas: 50
YAML
until_true 30 "replica limit enforced" has_reason too-big Progressing UnsupportedConfiguration

log "second executor profile"
cat <<YAML | $K apply -n "$NS" -f - >/dev/null
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: outbound
spec:
  image: ghcr.io/kubeswift-io/kubeswift-spin-examples/outbound-http:v0.1.0
  executor: kubeswift-open
  replicas: 1
YAML
until_true 60 "outbound-0 created" has_sandboxes outbound "outbound-0"
[[ "$(jp swiftsandbox/outbound-0 '{.spec.network.mode}')" == "open" ]] && ok "open network profile applied" || fail "network mode"

log "metrics"
pod="$($K -n "$SYS" get pod -l app.kubernetes.io/name=kubeswift-spin -o jsonpath='{.items[0].metadata.name}')"
$K -n "$SYS" port-forward "pod/$pod" 18080:8080 >/dev/null 2>&1 &
pf=$!
sleep 3
metrics="$(curl -fsS http://127.0.0.1:18080/metrics || true)"
kill $pf 2>/dev/null || true
for m in kubeswift_spin_reconciliations_total kubeswift_spin_apps kubeswift_spin_sandbox_creations_total kubeswift_spin_unsupported_configuration_total; do
  [[ "$metrics" == *"$m"* ]] && ok "metric $m" || fail "metric $m missing"
done
if grep -E '^kubeswift_spin_[a-z_]+\{[^}]*namespace=' <<<"$metrics" >/dev/null; then fail "metrics carry namespace labels"; else ok "no namespace labels on kubeswift_spin metrics"; fi

log "delete: owner references clean up sandboxes"
$K -n "$NS" delete spinapp hello outbound --wait=true >/dev/null
until_true 90 "all hello and outbound sandboxes deleted" \
  bash -c "[[ -z \$($K -n $NS get swiftsandbox -o name) ]]"

log "controller security context"
sc="$($K -n "$SYS" get pod "$pod" -o jsonpath='{.spec.containers[0].securityContext.readOnlyRootFilesystem}/{.spec.containers[0].securityContext.allowPrivilegeEscalation}/{.spec.securityContext.runAsNonRoot}')"
[[ "$sc" == "true/false/true" ]] && ok "hardened controller pod" || fail "security context $sc"

if [[ $FAILED -ne 0 ]]; then
  echo "kind integration test FAILED" >&2
  exit 1
fi
log "kind integration test passed"
