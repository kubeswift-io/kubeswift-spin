#!/usr/bin/env bash
# KVM end-to-end test: runs the example applications in real KubeSwift
# microVMs and reaches them through the SpinApp Service. Requires a cluster
# prepared as described in test/e2e/README.md (KubeSwift v0.16.0 or later).
#
# Phases:
#   1 hello-http      readiness, Service, scale up and down, rolling update
#                     under continuous traffic, delete
#   2 request-info    Secret-backed variable, two replicas behind the Service
#   3 serverless-ai   egress allowlist to an in-cluster service and a
#                     Secret-backed LLM token in the runtime configuration
#   4 warm pool       checkout from a matching SwiftSandboxPool (E2E_WARM_POOL=1)
#
# Environment:
#   E2E_NAMESPACE    namespace with the kubeswift executor and a Ready
#                    SwiftKernel "sandbox" (default kubeswift-spin-e2e)
#   EXAMPLES         registry prefix of the example artifacts
#   EXAMPLES_TAG     tag of the example artifacts
#   EXECUTOR         executor name (default kubeswift)
#   TIMEOUT          seconds to wait for each step (default 300)
#   E2E_WARM_POOL=1  also run phase 4
#   KEEP=1           keep the test objects afterwards
set -euo pipefail

NS="${E2E_NAMESPACE:-kubeswift-spin-e2e}"
EXAMPLES="${EXAMPLES:-ghcr.io/kubeswift-io/kubeswift-spin-examples}"
TAG="${EXAMPLES_TAG:-v0.1.0}"
EXECUTOR="${EXECUTOR:-kubeswift}"
TIMEOUT="${TIMEOUT:-300}"
CURL_IMAGE="curlimages/curl:8.16.0@sha256:463eaf6072688fe96ac64fa623fe73e1dbe25d8ad6c34404a669ad3ce1f104b6"
GO_IMAGE="golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
K="kubectl -n $NS"
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

jp() { $K get "$1" -o jsonpath="$2" 2>/dev/null; }
reason() { jp "spinapp/$1" "{.status.conditions[?(@.type==\"$2\")].reason}"; }
has_reason() { [[ "$(reason "$1" "$2")" == "$3" ]]; }
ready_replicas() { [[ "$(jp "spinapp/$1" '{.status.readyReplicas}')" == "$2" ]]; }
count() { $K get swiftsandbox -l "spin.kubeswift.io/app=$1" -o name 2>/dev/null | wc -l; }
has_count() { [[ "$(count "$1")" -eq "$2" ]]; }
endpoints() { $K get endpointslices -l "kubernetes.io/service-name=$1" -o jsonpath='{range .items[*].endpoints[?(@.conditions.ready==true)]}{.addresses[0]}{"\n"}{end}' 2>/dev/null | grep -c . ; }
has_endpoints() { [[ "$(endpoints "$1")" -eq "$2" ]]; }

# A long-lived client pod in the namespace, so requests come from a normal
# workload through the Service, as real clients do.
start_client() {
  $K delete pod e2e-client --ignore-not-found --wait=true >/dev/null
  $K run e2e-client --restart=Never --image="$CURL_IMAGE" --command -- sleep 3600 >/dev/null
  $K wait --for=condition=Ready pod/e2e-client --timeout=120s >/dev/null
}
http() { $K exec e2e-client -- curl -sS --max-time 10 "$@"; }
http_code() { $K exec e2e-client -- curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$@"; }

apply_app() {
  cat | $K apply -f - >/dev/null
}

cleanup() {
  if [[ $FAILED -ne 0 ]]; then
    $K get spinapp,swiftsandbox,svc -o wide 2>/dev/null || true
    for a in hello-e2e info-e2e ai-e2e ai-blocked-e2e warm-e2e; do
      $K get spinapp "$a" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || true
    done
  fi
  [[ "${KEEP:-}" == "1" ]] && return
  $K delete spinapp hello-e2e info-e2e ai-e2e ai-blocked-e2e warm-e2e --ignore-not-found --wait=false >/dev/null 2>&1 || true
  $K delete pod e2e-client upstream --ignore-not-found --wait=false >/dev/null 2>&1 || true
  $K delete svc upstream --ignore-not-found >/dev/null 2>&1 || true
  $K delete configmap upstream-src --ignore-not-found >/dev/null 2>&1 || true
  $K delete secret e2e-greeting e2e-llm-token --ignore-not-found >/dev/null 2>&1 || true
  $K delete spinappexecutor kubeswift-e2e-egress kubeswift-e2e-warm --ignore-not-found --wait=false >/dev/null 2>&1 || true
  $K delete swiftsandboxpool spin-e2e-warm --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "preflight"
for crd in spinapps.core.spinkube.dev spinappexecutors.core.spinkube.dev swiftsandboxes.sandbox.kubeswift.io swiftkernels.kernel.kubeswift.io; do
  kubectl get crd "$crd" >/dev/null 2>&1 || die "CRD $crd is not installed"
done
kubectl get --raw /openapi/v3/apis/sandbox.kubeswift.io/v1alpha1 | grep -q '"podMetadata"' \
  || die "the installed KubeSwift has no sandbox port exposure; this test needs KubeSwift v0.16.0 or later"
ok "KubeSwift sandbox exposure available"
kubectl get nodes -l kubeswift.io/kernel-node=true -o name | grep -q . || die "no node labelled kubeswift.io/kernel-node=true"
[[ "$(jp swiftkernel/sandbox '{.status.phase}')" == "Ready" ]] || die "SwiftKernel sandbox is not Ready in namespace $NS"
[[ "$(jp "spinappexecutor/$EXECUTOR" '{.metadata.labels.spin\.kubeswift\.io/managed-by}')" == "kubeswift-spin" ]] \
  || die "SpinAppExecutor $NS/$EXECUTOR is missing or not managed by kubeswift-spin"
kubectl get deploy -A -l app.kubernetes.io/name=kubeswift-spin -o jsonpath='{.items[0].status.availableReplicas}' | grep -q '^[1-9]' \
  || die "kubeswift-spin controller is not available"
ok "cluster prepared"
start_client
ok "client pod running"

log "phase 1: hello-http"
apply_app <<YAML
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: hello-e2e
spec:
  image: $EXAMPLES/hello-http:$TAG
  executor: $EXECUTOR
  replicas: 1
  checks:
    readiness:
      httpGet: {path: /healthz, httpHeaders: []}
      periodSeconds: 2
YAML
start=$SECONDS
until_true "hello-e2e-0 Running and the SpinApp Available" has_reason hello-e2e Available ApplicationReady
echo "   time to Available: $((SECONDS - start))s"
ready_replicas hello-e2e 1 && ok "readyReplicas 1" || fail "readyReplicas"
# Readiness is reported from the sandbox; the Service routes once the
# EndpointSlice lists the launcher pod, which can lag by a moment.
until_true "ready Service endpoint" has_endpoints hello-e2e 1
body="$(http http://hello-e2e/hello || true)"
[[ "$body" == "Hello from Spin on KubeSwift" ]] && ok "HTTP through the SpinApp Service" || fail "HTTP response: $body"
[[ "$(http_code http://hello-e2e/nope)" == "404" ]] && ok "routing inside the guest" || fail "404 route"

$K patch spinapp hello-e2e --type=merge -p '{"spec":{"replicas":2}}' >/dev/null
until_true "scaled to 2 ready replicas" ready_replicas hello-e2e 2
until_true "two ready Service endpoints" has_endpoints hello-e2e 2

# Rolling update under traffic: a new variable changes the sandbox spec.
old_rev="$(jp swiftsandbox/hello-e2e-0 '{.metadata.labels.spin\.kubeswift\.io/revision}')"
$K exec e2e-client -- sh -c 'end=$(($(date +%s)+240)); ok=0; bad=0; while [ $(date +%s) -lt $end ]; do if curl -s -o /dev/null -w "%{http_code}" --max-time 2 http://hello-e2e/hello | grep -q 200; then ok=$((ok+1)); else bad=$((bad+1)); fi; [ -f /tmp/stop ] && break; sleep 0.2; done; echo "$ok $bad" > /tmp/result' >/dev/null 2>&1 &
traffic=$!
$K patch spinapp hello-e2e --type=merge -p '{"spec":{"variables":[{"name":"rollout","value":"2"}]}}' >/dev/null
rolled() {
  local r0 r1
  r0="$(jp swiftsandbox/hello-e2e-0 '{.metadata.labels.spin\.kubeswift\.io/revision}')"
  r1="$(jp swiftsandbox/hello-e2e-1 '{.metadata.labels.spin\.kubeswift\.io/revision}')"
  [[ -n "$r0" && "$r0" != "$old_rev" && "$r0" == "$r1" ]] && ready_replicas hello-e2e 2 && has_reason hello-e2e Available ApplicationReady
}
until_true "both replicas replaced at the new revision" rolled
$K exec e2e-client -- touch /tmp/stop >/dev/null
wait "$traffic" || true
read -r good bad < <($K exec e2e-client -- cat /tmp/result)
if [[ "$bad" -eq 0 && "$good" -gt 0 ]]; then ok "no failed request during the rollout ($good requests)"; else fail "$bad of $((good + bad)) requests failed during the rollout"; fi

$K patch spinapp hello-e2e --type=merge -p '{"spec":{"replicas":1}}' >/dev/null
until_true "scaled down to one sandbox" has_count hello-e2e 1
until_true "one Service endpoint" has_endpoints hello-e2e 1
$K delete spinapp hello-e2e --wait=true >/dev/null
until_true "hello-e2e sandboxes removed" has_count hello-e2e 0

log "phase 2: request-info with a Secret-backed variable"
$K create secret generic e2e-greeting --from-literal=greeting='hello from a Secret' --dry-run=client -o yaml | $K apply -f - >/dev/null
apply_app <<YAML
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: info-e2e
spec:
  image: $EXAMPLES/request-info:$TAG
  executor: $EXECUTOR
  replicas: 2
  variables:
    - name: app_version
      value: e2e
    - name: greeting
      valueFrom:
        secretKeyRef: {name: e2e-greeting, key: greeting}
YAML
until_true "info-e2e 2 replicas ready" ready_replicas info-e2e 2
until_true "info-e2e endpoints ready" has_endpoints info-e2e 2
body="$(http -H 'Authorization: Bearer do-not-echo' 'http://info-e2e/info?x=1')"
[[ "$body" == *'"greeting":"hello from a Secret"'* ]] && ok "Secret value reached Spin" || fail "secret variable: $body"
[[ "$body" == *'"app_version":"e2e"'* ]] && ok "literal variable" || fail "literal variable: $body"
[[ "$body" != *"do-not-echo"* ]] && ok "authorization header not echoed" || fail "authorization echoed"
if $K get swiftsandbox info-e2e-0 -o yaml | grep -q "hello from a Secret" || \
   $K get configmap -o yaml | grep -q "hello from a Secret"; then
  fail "the Secret value is stored in a SwiftSandbox or ConfigMap"
else
  ok "the Secret value is in no SwiftSandbox or ConfigMap"
fi
$K delete spinapp info-e2e --wait=false >/dev/null

log "phase 3: serverless-ai, egress allowlist and a Secret-backed LLM token"
$K create configmap upstream-src --from-file=main.go="$ROOT/examples/tools/upstream/main.go" --dry-run=client -o yaml | $K apply -f - >/dev/null
$K create secret generic e2e-llm-token --from-literal=token=e2e-token --dry-run=client -o yaml | $K apply -f - >/dev/null
cat <<YAML | $K apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: upstream
  labels: {app: e2e-upstream}
spec:
  containers:
    - name: upstream
      image: $GO_IMAGE
      command: [go, run, /src/main.go, --listen, "0.0.0.0:8090", --require-token, e2e-token]
      env: [{name: GOCACHE, value: /tmp/gocache}, {name: GOFLAGS, value: -mod=mod}]
      ports: [{containerPort: 8090}]
      readinessProbe: {httpGet: {path: /, port: 8090}, periodSeconds: 2}
      volumeMounts: [{name: src, mountPath: /src}]
  volumes: [{name: src, configMap: {name: upstream-src}}]
---
apiVersion: v1
kind: Service
metadata:
  name: upstream
spec:
  selector: {app: e2e-upstream}
  ports: [{port: 8090, targetPort: 8090}]
---
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinAppExecutor
metadata:
  name: kubeswift-e2e-egress
  labels: {spin.kubeswift.io/managed-by: kubeswift-spin}
  annotations:
    spin.kubeswift.io/egress-allow: '[{"service":{"name":"upstream"},"ports":[{"port":8090}]}]'
spec:
  createDeployment: false
YAML
$K wait --for=condition=Ready pod/upstream --timeout=180s >/dev/null && ok "in-cluster OpenAI-compatible mock ready" || fail "mock not ready"
for app in ai-e2e:kubeswift-e2e-egress ai-blocked-e2e:$EXECUTOR; do
  apply_app <<YAML
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: ${app%%:*}
spec:
  image: $EXAMPLES/serverless-ai:$TAG
  executor: ${app##*:}
  replicas: 1
  runtimeConfig:
    llmCompute:
      type: remote_http
      options:
        - name: url
          value: http://upstream.$NS.svc.cluster.local:8090
        - name: api_type
          value: open_ai
        - name: auth_token
          valueFrom:
            secretKeyRef: {name: e2e-llm-token, key: token}
YAML
done
until_true "ai-e2e ready" ready_replicas ai-e2e 1
until_true "ai-blocked-e2e ready" ready_replicas ai-blocked-e2e 1
until_true "ai endpoints ready" bash -c "[[ \$($K get endpointslices -l kubernetes.io/service-name=ai-e2e -o jsonpath='{.items[*].endpoints[?(@.conditions.ready==true)].addresses[0]}' | wc -w) -ge 1 && \$($K get endpointslices -l kubernetes.io/service-name=ai-blocked-e2e -o jsonpath='{.items[*].endpoints[?(@.conditions.ready==true)].addresses[0]}' | wc -w) -ge 1 ]]"
body="$(http -X POST --data-binary 'hello from the e2e test' http://ai-e2e/ask)"
[[ "$body" == *'mock completion from model \"default-model\"'* ]] \
  && ok "inference through the egress allowlist with a Secret-backed token" || fail "inference: $body"
# Restricted egress drops the packets, so Spin waits for a TCP connect
# timeout: the request either times out here or fails with 502. Either way it
# must not succeed.
code="$($K exec e2e-client -- curl -s -o /dev/null -w '%{http_code}' --max-time 20 -X POST --data-binary 'x' http://ai-blocked-e2e/ask || true)"
case "$code" in
  200) fail "restricted sandbox without the allowlist reached the cluster service" ;;
  502 | 000) ok "restricted sandbox without the allowlist cannot reach the cluster service (HTTP $code)" ;;
  *) fail "unexpected HTTP $code from ai-blocked-e2e" ;;
esac
$K delete spinapp ai-e2e ai-blocked-e2e --wait=false >/dev/null

if [[ "${E2E_WARM_POOL:-}" == "1" ]]; then
  log "phase 4: warm pool checkout"
  runtime_image="$(jp "spinappexecutor/$EXECUTOR" '{.metadata.annotations.spin\.kubeswift\.io/runtime-image}')"
  [[ -z "$runtime_image" ]] && runtime_image="$(kubectl get deploy -A -l app.kubernetes.io/name=kubeswift-spin -o jsonpath='{.items[0].spec.template.spec.containers[0].args}' | tr ',' '\n' | sed -n 's/.*--runtime-image=\([^"]*\).*/\1/p')"
  cat <<YAML | $K apply -f - >/dev/null
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata:
  name: spin-e2e-warm
spec:
  image: $runtime_image
  cpu: 1
  memory: 512Mi
  network:
    mode: restricted
    ports: [{name: http-app, port: 3000}]
  minWarm: 1
---
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinAppExecutor
metadata:
  name: kubeswift-e2e-warm
  labels: {spin.kubeswift.io/managed-by: kubeswift-spin}
  annotations:
    spin.kubeswift.io/sandbox-pool: spin-e2e-warm
spec:
  createDeployment: false
YAML
  until_true "pool has a warm slot" bash -c "[[ \$($K get swiftsandboxpool spin-e2e-warm -o jsonpath='{.status.warmReplicas}') -ge 1 ]]"
  apply_app <<YAML
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: warm-e2e
spec:
  image: $EXAMPLES/hello-http:$TAG
  executor: kubeswift-e2e-warm
  replicas: 1
YAML
  start=$SECONDS
  until_true "warm-e2e Available" has_reason warm-e2e Available ApplicationReady
  echo "   time to Available from a warm slot: $((SECONDS - start))s"
  until_true "warm-e2e endpoint ready" has_endpoints warm-e2e 1
  if $K get events.events.k8s.io --field-selector regarding.name=warm-e2e-0,reason=CheckedOut -o name | grep -q .; then
    ok "checked out a warm slot"
  else
    fail "no CheckedOut event: the sandbox booted cold"
  fi
  [[ "$(http http://warm-e2e/hello)" == "Hello from Spin on KubeSwift" ]] && ok "HTTP to the checked-out sandbox" || fail "HTTP to warm sandbox"
fi

if [[ $FAILED -ne 0 ]]; then
  echo "KVM e2e FAILED" >&2
  exit 1
fi
log "KVM e2e passed"
