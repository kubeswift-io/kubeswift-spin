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
#   4 warm pool       checkout from a matching SwiftSandboxPool
#   5 loadFromSecret  the whole runtime configuration from a Secret
#   6 private registry  imagePullSecrets for an authenticated registry
#   7 ingress-from    the executor ingress restriction
#   8 liveness        replacement of a replica whose liveness check fails
#   9 teardown        deletion leaves no SpinApp, sandbox, Service or policy
#
# Environment:
#   E2E_NAMESPACE    namespace with the kubeswift executor and a Ready
#                    SwiftKernel "sandbox" (default kubeswift-spin-e2e)
#   EXAMPLES         registry prefix of the example artifacts
#   EXAMPLES_TAG     tag of the example artifacts
#   EXECUTOR         executor name (default kubeswift)
#   TIMEOUT          seconds to wait for each step (default 300)
#   E2E_PHASES       phases to run (default "1 2 3 5 7 8 9", plus 4 and 6
#                    as below)
#   E2E_WARM_POOL=1  also run phase 4
#   E2E_SCRATCH_REGISTRY  registry for the phase 6 test runtime image, for
#                    example ttl.sh; setting it also runs phase 6
#   KEEP=1           keep the test objects afterwards
set -euo pipefail

NS="${E2E_NAMESPACE:-kubeswift-spin-e2e}"
EXAMPLES="${EXAMPLES:-ghcr.io/kubeswift-io/kubeswift-spin-examples}"
TAG="${EXAMPLES_TAG:-v0.1.0-rc3}"
EXECUTOR="${EXECUTOR:-kubeswift}"
TIMEOUT="${TIMEOUT:-300}"
SCRATCH="${E2E_SCRATCH_REGISTRY:-}"
PHASES=" ${E2E_PHASES:-1 2 3 5 7 8 9} "
if [[ -z "${E2E_PHASES:-}" ]]; then
  [[ "${E2E_WARM_POOL:-}" == "1" ]] && PHASES+="4 "
  [[ -n "$SCRATCH" ]] && PHASES+="6 "
fi
CURL_IMAGE="curlimages/curl:8.16.0@sha256:463eaf6072688fe96ac64fa623fe73e1dbe25d8ad6c34404a669ad3ce1f104b6"
GO_IMAGE="golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d"
REGISTRY_IMAGE="registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
CRANE_IMAGE="gcr.io/go-containerregistry/crane:v0.22.1@sha256:1f968817b95790bed063f71175aa6b8ff879fa17064020415f3e18bb6e6a36e1"
BUSYBOX_IMAGE="busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
K="kubectl -n $NS"
FAILED=0
WORK="$(mktemp -d)"

# Unique values for this run, so a leak can be searched for reliably. They
# are never printed.
RUN_ID="$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
LLM_TOKEN="kss-e2e-llm-$RUN_ID-token"
REG_USER="e2e"
REG_PASS="kss-e2e-registry-$RUN_ID-password"
APPS="hello-e2e info-e2e ai-e2e ai-blocked-e2e warm-e2e lfs-e2e private-e2e private-noauth-e2e ingress-e2e live-e2e"

log() { printf '\n==> %s\n' "$*"; }
ok() { printf 'ok: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAILED=1; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
want() { [[ "$PHASES" == *" $1 "* ]]; }

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
uid_of() { jp "swiftsandbox/$1" '{.metadata.uid}'; }
phase_of() { jp "swiftsandbox/$1" '{.status.phase}'; }
new_uid() { local u; u="$(uid_of "$1")"; [[ -n "$u" && "$u" != "$2" ]]; }
is_failed() { [[ "$(uid_of "$1")" == "$2" && "$(phase_of "$1")" == "Failed" ]]; }
failed_events() { $K get events.events.k8s.io --field-selector "regarding.name=$1,reason=SandboxFailed" -o name 2>/dev/null | grep -c . ; }

# A long-lived client pod in the namespace, so requests come from a normal
# workload through the Service, as real clients do.
start_client() {
  $K delete pod e2e-client --ignore-not-found --wait=true >/dev/null
  $K run e2e-client --restart=Never --image="$CURL_IMAGE" --command -- sleep 3600 >/dev/null
  $K wait --for=condition=Ready pod/e2e-client --timeout=120s >/dev/null
}
http() { $K exec e2e-client -- curl -sS --max-time 10 "$@"; }
http_code() { $K exec e2e-client -- curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$@"; }
code_from() {
  local pod="$1" t="$2"
  shift 2
  $K exec "$pod" -- curl -s -o /dev/null -w '%{http_code}' --max-time "$t" "$@" || true
}

apply_app() {
  cat | $K apply -f - >/dev/null
}

controller_ns() { kubectl get deploy -A -l app.kubernetes.io/name=kubeswift-spin -o jsonpath='{.items[0].metadata.namespace}'; }
runtime_image() {
  local img
  img="$(jp "spinappexecutor/$EXECUTOR" '{.metadata.annotations.spin\.kubeswift\.io/runtime-image}')"
  [[ -z "$img" ]] && img="$(kubectl get deploy -A -l app.kubernetes.io/name=kubeswift-spin -o jsonpath='{.items[0].spec.template.spec.containers[0].args}' | tr ',' '\n' | sed -n 's/.*--runtime-image=\([^"]*\).*/\1/p')"
  printf '%s\n' "$img"
}

# secret_from_value creates a generic Secret with one key from a value,
# through a 0600 file, so the value never appears in a command line.
secret_from_value() {
  local f="$WORK/secret-value"
  (umask 077 && printf '%s' "$3" >"$f")
  $K create secret generic "$1" --from-file="$2=$f" --dry-run=client -o yaml | $K apply -f - >/dev/null
  rm -f "$f"
}

# The OpenAI-compatible mock reads its token from the Secret e2e-llm-token,
# so the value appears in no Pod spec.
ensure_upstream() {
  $K get pod upstream >/dev/null 2>&1 && return 0
  $K create configmap upstream-src --from-file=main.go="$ROOT/examples/tools/upstream/main.go" --dry-run=client -o yaml | $K apply -f - >/dev/null
  secret_from_value e2e-llm-token token "$LLM_TOKEN"
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
      command: [go, run, /src/main.go, --listen, "0.0.0.0:8090"]
      env:
        - {name: GOCACHE, value: /tmp/gocache}
        - {name: GOFLAGS, value: -mod=mod}
        - name: UPSTREAM_REQUIRE_TOKEN
          valueFrom: {secretKeyRef: {name: e2e-llm-token, key: token}}
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
  if $K wait --for=condition=Ready pod/upstream --timeout=180s >/dev/null; then
    ok "in-cluster OpenAI-compatible mock ready"
  else
    fail "mock not ready"
  fi
}

# search_forms prints the value and the base64 forms it can take at each
# byte alignment inside a longer base64 string (the edges, which depend on
# the neighbouring bytes, are cut off).
search_forms() {
  local v="$1" p b
  printf '%s\n' "$v"
  for p in "" "x" "xx"; do
    b="$(printf '%s%s' "$p" "$v" | base64 -w0)"
    b="${b:4}"
    printf '%s\n' "${b:0:${#b}-4}"
  done
}

# leak_scan fails if any value appears outside a Secret: in any namespaced
# object of the test namespace (SpinApps, SwiftSandboxes, Pods, ConfigMaps,
# Events, policies, ...), in Events of any namespace, or in the logs of the
# kubeswift-spin controller, the KubeSwift system pods and the pods of the
# test namespace. Values are never printed.
leak_scan() {
  local what="$1"
  shift
  local corpus="$WORK/corpus" r p ksns
  : >"$corpus"
  for r in $(kubectl api-resources --verbs=list --namespaced -o name); do
    [[ "$r" == "secrets" ]] && continue
    $K get "$r" -o yaml >>"$corpus" 2>/dev/null || true
  done
  kubectl get events -A -o yaml >>"$corpus" 2>/dev/null || true
  kubectl get events.events.k8s.io -A -o yaml >>"$corpus" 2>/dev/null || true
  kubectl -n "$(controller_ns)" logs -l app.kubernetes.io/name=kubeswift-spin --all-containers --tail=-1 >>"$corpus" 2>/dev/null || true
  ksns="${KUBESWIFT_NAMESPACE:-kubeswift-system}"
  for p in $(kubectl -n "$ksns" get pods -o name 2>/dev/null); do
    kubectl -n "$ksns" logs "$p" --all-containers --since=3h >>"$corpus" 2>/dev/null || true
  done
  for p in $($K get pods -o name 2>/dev/null); do
    $K logs "$p" --all-containers >>"$corpus" 2>/dev/null || true
  done
  local found=0 v f
  for v in "$@"; do
    while IFS= read -r f; do
      [[ -n "$f" ]] && grep -qF -- "$f" "$corpus" && found=1
    done < <(search_forms "$v")
  done
  if [[ $found -eq 0 ]]; then
    ok "$what: in no object outside Secrets, no Event and no log ($(wc -c <"$corpus") bytes searched)"
  else
    fail "$what: found outside a Secret"
  fi
}

cleanup() {
  if [[ $FAILED -ne 0 ]]; then
    $K get spinapp,swiftsandbox,svc -o wide 2>/dev/null || true
    for a in $APPS; do
      $K get spinapp "$a" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || true
    done
  fi
  rm -rf "$WORK"
  [[ "${KEEP:-}" == "1" ]] && return
  # shellcheck disable=SC2086
  $K delete spinapp $APPS --ignore-not-found --wait=false >/dev/null 2>&1 || true
  $K delete pod e2e-client e2e-client-allowed upstream e2e-registry e2e-live-target --ignore-not-found --wait=false >/dev/null 2>&1 || true
  $K delete job e2e-registry-seed --ignore-not-found --wait=false >/dev/null 2>&1 || true
  $K delete svc upstream e2e-registry e2e-live-target --ignore-not-found >/dev/null 2>&1 || true
  $K delete configmap upstream-src --ignore-not-found >/dev/null 2>&1 || true
  $K delete secret e2e-greeting e2e-llm-token e2e-runtime-config e2e-registry-files e2e-registry-auth --ignore-not-found >/dev/null 2>&1 || true
  $K delete spinappexecutor kubeswift-e2e-egress kubeswift-e2e-warm kubeswift-e2e-private kubeswift-e2e-ingress kubeswift-e2e-live --ignore-not-found --wait=false >/dev/null 2>&1 || true
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
if want 6; then
  [[ -n "$SCRATCH" ]] || die "phase 6 needs E2E_SCRATCH_REGISTRY (a registry the cluster can pull from without credentials, for example ttl.sh)"
  for tool in docker openssl python3; do command -v "$tool" >/dev/null || die "phase 6 needs $tool"; done
  python3 -c 'import bcrypt' 2>/dev/null || die "phase 6 needs the Python bcrypt module"
fi
ok "cluster prepared"
echo "   phases:$PHASES"
start_client
ok "client pod running"

if want 1; then
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

$K patch spinapp hello-e2e --type=merge -p '{"spec":{"replicas":3}}' >/dev/null
until_true "scaled to 3 ready replicas" ready_replicas hello-e2e 3
until_true "three ready Service endpoints" has_endpoints hello-e2e 3
$K patch spinapp hello-e2e --type=merge -p '{"spec":{"replicas":2}}' >/dev/null
until_true "scaled down to 2 sandboxes" has_count hello-e2e 2
until_true "two ready Service endpoints" has_endpoints hello-e2e 2

# Rolling update under traffic: a new variable changes the sandbox spec.
old_rev="$(jp swiftsandbox/hello-e2e-0 '{.metadata.labels.spin\.kubeswift\.io/revision}')"
$K exec e2e-client -- sh -c 'rm -f /tmp/stop /tmp/result; end=$(($(date +%s)+240)); ok=0; bad=0; while [ $(date +%s) -lt $end ]; do if curl -s -o /dev/null -w "%{http_code}" --max-time 2 http://hello-e2e/hello | grep -q 200; then ok=$((ok+1)); else bad=$((bad+1)); fi; [ -f /tmp/stop ] && break; sleep 0.2; done; echo "$ok $bad" > /tmp/result' >/dev/null 2>&1 &
traffic=$!
sleep 5
$K patch spinapp hello-e2e --type=merge -p '{"spec":{"variables":[{"name":"rollout","value":"2"}]}}' >/dev/null
start=$SECONDS
rolled() {
  local r0 r1
  r0="$(jp swiftsandbox/hello-e2e-0 '{.metadata.labels.spin\.kubeswift\.io/revision}')"
  r1="$(jp swiftsandbox/hello-e2e-1 '{.metadata.labels.spin\.kubeswift\.io/revision}')"
  [[ -n "$r0" && "$r0" != "$old_rev" && "$r0" == "$r1" ]] && ready_replicas hello-e2e 2 && has_reason hello-e2e Available ApplicationReady
}
until_true "both replicas replaced at the new revision" rolled
echo "   rollout time: $((SECONDS - start))s"
sleep 5
$K exec e2e-client -- touch /tmp/stop >/dev/null
wait "$traffic" || true
read -r good bad < <($K exec e2e-client -- cat /tmp/result)
if [[ "$bad" -eq 0 && "$good" -gt 0 ]]; then ok "no failed request during the rollout ($good requests)"; else fail "$bad of $((good + bad)) requests failed during the rollout"; fi

$K patch spinapp hello-e2e --type=merge -p '{"spec":{"replicas":1}}' >/dev/null
until_true "scaled down to one sandbox" has_count hello-e2e 1
until_true "one Service endpoint" has_endpoints hello-e2e 1
$K delete spinapp hello-e2e --wait=true >/dev/null
until_true "hello-e2e sandboxes removed" has_count hello-e2e 0
fi

if want 2; then
log "phase 2: request-info with a Secret-backed variable"
greeting="hello from a Secret $RUN_ID"
secret_from_value e2e-greeting greeting "$greeting"
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
[[ "$body" == *"\"greeting\":\"$greeting\""* ]] && ok "Secret value reached Spin" || fail "secret variable not delivered"
[[ "$body" == *'"app_version":"e2e"'* ]] && ok "literal variable" || fail "literal variable missing from the response"
[[ "$body" != *"do-not-echo"* ]] && ok "authorization header not echoed" || fail "authorization echoed"
leak_scan "Secret-backed variable" "$greeting"
$K delete spinapp info-e2e --wait=false >/dev/null
fi

if want 3; then
log "phase 3: serverless-ai, egress allowlist and a Secret-backed LLM token"
ensure_upstream
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
leak_scan "Secret-backed runtime-config option" "$LLM_TOKEN"
$K delete spinapp ai-e2e ai-blocked-e2e --wait=false >/dev/null
fi

if want 4; then
  log "phase 4: warm pool checkout"
  cat <<YAML | $K apply -f - >/dev/null
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata:
  name: spin-e2e-warm
spec:
  image: $(runtime_image)
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
  $K delete spinapp warm-e2e --wait=false >/dev/null
fi

if want 5; then
log "phase 5: runtimeConfig.loadFromSecret"
ensure_upstream
cat >"$WORK/runtime-config.toml" <<TOML
[llm_compute]
type = "remote_http"
url = "http://upstream.$NS.svc.cluster.local:8090"
api_type = "open_ai"
auth_token = "$LLM_TOKEN"
TOML
$K create secret generic e2e-runtime-config --from-file=runtime-config.toml="$WORK/runtime-config.toml" --dry-run=client -o yaml | $K apply -f - >/dev/null
rm -f "$WORK/runtime-config.toml"
apply_app <<YAML
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: lfs-e2e
spec:
  image: $EXAMPLES/serverless-ai:$TAG
  executor: kubeswift-e2e-egress
  replicas: 1
  runtimeConfig:
    loadFromSecret: e2e-runtime-config
YAML
until_true "lfs-e2e ready" ready_replicas lfs-e2e 1
until_true "lfs-e2e endpoint ready" has_endpoints lfs-e2e 1
files="$(jp swiftsandbox/lfs-e2e-0 '{range .spec.secretFiles[*]}{.secretName}:{.items[0].key}>{.items[0].path}{end}')"
[[ "$files" == "e2e-runtime-config:runtime-config.toml>/run/kubeswift-spin/runtime-config.toml" ]] \
  && ok "runtime configuration projected as a KubeSwift secret file" || fail "secretFiles: $files"
body="$(http -X POST --data-binary 'hello from loadFromSecret' http://lfs-e2e/ask)"
[[ "$body" == *'mock completion from model'* ]] \
  && ok "the token in the Secret runtime configuration reached Spin (the mock requires it)" || fail "inference: $body"
leak_scan "loadFromSecret runtime configuration" "$LLM_TOKEN"
$K delete spinapp lfs-e2e --wait=false >/dev/null
fi

if want 6; then
log "phase 6: private application registry through imagePullSecrets"
reg_host="e2e-registry.$NS.svc.cluster.local:5000"
# A throwaway CA for an in-cluster TLS registry with htpasswd. Spin speaks
# only HTTPS to registries and kubeswift-spin has no CA option, so a test
# runtime image that trusts this CA is built from the configured runtime
# image and pushed to E2E_SCRATCH_REGISTRY. The CA key is deleted with the
# work directory when the script exits.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=kubeswift-spin-e2e-ca \
  -keyout "$WORK/ca.key" -out "$WORK/ca.crt" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=e2e-registry \
  -keyout "$WORK/tls.key" -out "$WORK/tls.csr" 2>/dev/null
printf 'subjectAltName=DNS:e2e-registry.%s.svc.cluster.local,DNS:e2e-registry.%s.svc\nextendedKeyUsage=serverAuth\n' "$NS" "$NS" >"$WORK/ext"
openssl x509 -req -in "$WORK/tls.csr" -CA "$WORK/ca.crt" -CAkey "$WORK/ca.key" -CAcreateserial -days 1 \
  -extfile "$WORK/ext" -out "$WORK/tls.crt" 2>/dev/null
REG_PASS="$REG_PASS" python3 -c 'import bcrypt, os; print("e2e:" + bcrypt.hashpw(os.environ["REG_PASS"].encode(), bcrypt.gensalt()).decode())' >"$WORK/htpasswd"
$K create secret generic e2e-registry-files --from-file="$WORK/tls.crt" --from-file="$WORK/tls.key" \
  --from-file="$WORK/ca.crt" --from-file="$WORK/htpasswd" --dry-run=client -o yaml | $K apply -f - >/dev/null
(umask 077 && printf '{"auths":{"%s":{"auth":"%s"}}}' "$reg_host" "$(printf '%s:%s' "$REG_USER" "$REG_PASS" | base64 -w0)" >"$WORK/config.json")
$K create secret generic e2e-registry-auth --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson="$WORK/config.json" --dry-run=client -o yaml | $K apply -f - >/dev/null
rm -f "$WORK/config.json"
rm -f "$WORK/tls.key" "$WORK/htpasswd" "$WORK/ca.key"
cat <<YAML | $K apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e-registry
  labels: {app: e2e-registry}
spec:
  containers:
    - name: registry
      image: $REGISTRY_IMAGE
      env:
        - {name: REGISTRY_HTTP_TLS_CERTIFICATE, value: /certs/tls.crt}
        - {name: REGISTRY_HTTP_TLS_KEY, value: /certs/tls.key}
        - {name: REGISTRY_AUTH, value: htpasswd}
        - {name: REGISTRY_AUTH_HTPASSWD_REALM, value: e2e}
        - {name: REGISTRY_AUTH_HTPASSWD_PATH, value: /certs/htpasswd}
      ports: [{containerPort: 5000}]
      readinessProbe: {tcpSocket: {port: 5000}, periodSeconds: 2}
      volumeMounts: [{name: files, mountPath: /certs, readOnly: true}]
  volumes: [{name: files, secret: {secretName: e2e-registry-files}}]
---
apiVersion: v1
kind: Service
metadata:
  name: e2e-registry
spec:
  selector: {app: e2e-registry}
  ports: [{port: 5000, targetPort: 5000}]
YAML
$K wait --for=condition=Ready pod/e2e-registry --timeout=180s >/dev/null && ok "authenticated TLS registry ready" || fail "registry not ready"
# Copy the published artifact into the private registry from inside the
# cluster, with the same Docker config Secret the SpinApp uses.
cat <<YAML | $K apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata:
  name: e2e-registry-seed
spec:
  backoffLimit: 2
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: crane
          image: $CRANE_IMAGE
          args: [copy, "$EXAMPLES/hello-http:$TAG", "$reg_host/private/hello-http:e2e"]
          env:
            - {name: SSL_CERT_FILE, value: /certs/ca.crt}
            - {name: DOCKER_CONFIG, value: /docker}
          volumeMounts:
            - {name: files, mountPath: /certs/ca.crt, subPath: ca.crt, readOnly: true}
            - {name: auth, mountPath: /docker, readOnly: true}
      volumes:
        - {name: files, secret: {secretName: e2e-registry-files}}
        - name: auth
          secret: {secretName: e2e-registry-auth, items: [{key: .dockerconfigjson, path: config.json}]}
YAML
$K wait --for=condition=Complete job/e2e-registry-seed --timeout=180s >/dev/null \
  && ok "hello-http copied into the private registry" || fail "seeding the private registry"
code="$(code_from e2e-client 10 -k "https://$reg_host/v2/private/hello-http/manifests/e2e")"
[[ "$code" == "401" ]] && ok "the registry refuses anonymous pulls (HTTP 401)" || fail "anonymous manifest request returned HTTP $code"

base="$(runtime_image)"
docker pull -q "$base" >/dev/null
cid="$(docker create "$base")"
docker cp "$cid:/etc/ssl/certs/ca-certificates.crt" "$WORK/bundle.crt" >/dev/null
chmod 0644 "$WORK/bundle.crt"
docker rm "$cid" >/dev/null
cat "$WORK/ca.crt" >>"$WORK/bundle.crt"
printf 'FROM %s\nCOPY bundle.crt /etc/ssl/certs/ca-certificates.crt\n' "$base" >"$WORK/Dockerfile"
test_image="$SCRATCH/kubeswift-spin-e2e-runtime-$RUN_ID:2h"
docker build -q -t "$test_image" "$WORK" >/dev/null
docker push -q "$test_image" >/dev/null
test_image="$(docker inspect --format '{{index .RepoDigests 0}}' "$test_image")"
docker rmi "$test_image" >/dev/null 2>&1 || true
ok "test runtime image trusting the test CA: $test_image"
cat <<YAML | $K apply -f - >/dev/null
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinAppExecutor
metadata:
  name: kubeswift-e2e-private
  labels: {spin.kubeswift.io/managed-by: kubeswift-spin}
  annotations:
    spin.kubeswift.io/runtime-image: "$test_image"
    spin.kubeswift.io/egress-allow: '[{"service":{"name":"e2e-registry"},"ports":[{"port":5000}]}]'
spec:
  createDeployment: false
YAML
for app in private-e2e:e2e-registry-auth private-noauth-e2e:; do
  name="${app%%:*}"
  secret="${app##*:}"
  {
    cat <<YAML
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: $name
spec:
  image: $reg_host/private/hello-http:e2e
  executor: kubeswift-e2e-private
  replicas: 1
  checks:
    readiness:
      httpGet: {path: /healthz, httpHeaders: []}
      periodSeconds: 2
YAML
    if [[ -n "$secret" ]]; then printf '  imagePullSecrets:\n    - name: %s\n' "$secret"; fi
  } | apply_app
done
start=$SECONDS
until_true "private-e2e Available" has_reason private-e2e Available ApplicationReady
echo "   time to Available: $((SECONDS - start))s"
until_true "private-e2e endpoint ready" has_endpoints private-e2e 1
[[ "$(http http://private-e2e/hello)" == "Hello from Spin on KubeSwift" ]] \
  && ok "application pulled by Spin from the authenticated registry serves HTTP" || fail "HTTP to private-e2e"
files="$(jp swiftsandbox/private-e2e-0 '{range .spec.secretFiles[*]}{.secretName}:{.items[0].key}>{.items[0].path}{end}')"
[[ "$files" == "e2e-registry-auth:.dockerconfigjson>/run/kubeswift-spin/registry-auth/0.json" ]] \
  && ok "credentials projected as a KubeSwift secret file" || fail "secretFiles: $files"
until_true "private-noauth-e2e sandbox fails without credentials" bash -c "[[ \$($K get events.events.k8s.io --field-selector regarding.name=private-noauth-e2e,reason=SandboxFailed -o name | wc -l) -ge 1 ]]"
[[ "$(jp spinapp/private-noauth-e2e '{.status.readyReplicas}')" == "0" ]] \
  && ok "private-noauth-e2e never became ready" || fail "private-noauth-e2e became ready without credentials"
leak_scan "registry credentials" "$REG_PASS" "$(printf '%s:%s' "$REG_USER" "$REG_PASS" | base64 -w0)"
$K delete spinapp private-e2e private-noauth-e2e --wait=false >/dev/null
fi

if want 7; then
log "phase 7: ingress-from"
cat <<YAML | $K apply -f - >/dev/null
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinAppExecutor
metadata:
  name: kubeswift-e2e-ingress
  labels: {spin.kubeswift.io/managed-by: kubeswift-spin}
  annotations:
    spin.kubeswift.io/ingress-from: '[{"podSelector":{"matchLabels":{"e2e-role":"allowed"}}}]'
spec:
  createDeployment: false
---
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: ingress-e2e
spec:
  image: $EXAMPLES/hello-http:$TAG
  executor: kubeswift-e2e-ingress
  replicas: 1
  checks:
    readiness:
      httpGet: {path: /healthz, httpHeaders: []}
      periodSeconds: 2
YAML
$K delete pod e2e-client-allowed --ignore-not-found --wait=true >/dev/null
$K run e2e-client-allowed --restart=Never --labels=e2e-role=allowed --image="$CURL_IMAGE" --command -- sleep 3600 >/dev/null
$K wait --for=condition=Ready pod/e2e-client-allowed --timeout=120s >/dev/null
until_true "ingress-e2e Available (readiness is not blocked by the restriction)" has_reason ingress-e2e Available ApplicationReady
until_true "ingress-e2e endpoint ready" has_endpoints ingress-e2e 1
peer="$(jp networkpolicy/ingress-e2e-0-restricted '{.spec.ingress[0].from[0].podSelector.matchLabels.e2e-role}')"
[[ "$peer" == "allowed" ]] && ok "KubeSwift NetworkPolicy ingress-e2e-0-restricted admits only pods labelled e2e-role=allowed" \
  || fail "NetworkPolicy peer: [$peer]"
code="$(code_from e2e-client-allowed 10 http://ingress-e2e/hello)"
[[ "$code" == "200" ]] && ok "allowed source -> Service: HTTP 200" || fail "allowed source got HTTP $code"
code="$(code_from e2e-client 5 http://ingress-e2e/hello)"
[[ "$code" == "000" ]] && ok "disallowed source -> Service: no connection" || fail "disallowed source got HTTP $code"
# The same pod becomes allowed when it gets the label, so the decision is
# the NetworkPolicy peer match and not the application or the client.
$K label pod e2e-client e2e-role=allowed >/dev/null
until_true "disallowed source admitted after it gets the label" bash -c "[[ \$($K exec e2e-client -- curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://ingress-e2e/hello) == 200 ]]"
$K label pod e2e-client e2e-role- >/dev/null
until_true "and blocked again after the label is removed" bash -c "[[ \$($K exec e2e-client -- curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://ingress-e2e/hello || true) == 000 ]]"
$K delete spinapp ingress-e2e --wait=false >/dev/null
$K delete pod e2e-client-allowed --wait=false >/dev/null
fi

if want 8; then
log "phase 8: liveness failure and replacement"
# outbound-http /fetch returns 200 while its target answers 200 and 502
# otherwise, so removing the target file makes the liveness check fail
# without touching the sandbox.
cat <<YAML | $K apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e-live-target
  labels: {app: e2e-live-target}
spec:
  containers:
    - name: httpd
      image: $BUSYBOX_IMAGE
      command: [sh, -c, 'mkdir -p /www && echo ok > /www/alive && exec httpd -f -p 8080 -h /www']
      ports: [{containerPort: 8080}]
      readinessProbe: {tcpSocket: {port: 8080}, periodSeconds: 2}
---
apiVersion: v1
kind: Service
metadata:
  name: e2e-live-target
spec:
  selector: {app: e2e-live-target}
  ports: [{port: 8080, targetPort: 8080}]
---
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinAppExecutor
metadata:
  name: kubeswift-e2e-live
  labels: {spin.kubeswift.io/managed-by: kubeswift-spin}
  annotations:
    spin.kubeswift.io/egress-allow: '[{"service":{"name":"e2e-live-target"},"ports":[{"port":8080}]}]'
spec:
  createDeployment: false
YAML
$K wait --for=condition=Ready pod/e2e-live-target --timeout=120s >/dev/null
target="http://e2e-live-target.$NS.svc.cluster.local:8080"
apply_app <<YAML
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: live-e2e
spec:
  image: $EXAMPLES/outbound-http:$TAG
  executor: kubeswift-e2e-live
  replicas: 1
  variables:
    - {name: target_url, value: "$target/alive"}
    - {name: target_origin, value: "$target"}
  checks:
    readiness:
      httpGet: {path: /healthz, httpHeaders: []}
      periodSeconds: 2
    liveness:
      httpGet: {path: /fetch, httpHeaders: []}
      initialDelaySeconds: 5
      periodSeconds: 3
      timeoutSeconds: 2
      failureThreshold: 2
YAML
until_true "live-e2e Available" has_reason live-e2e Available ApplicationReady
until_true "live-e2e endpoint ready" has_endpoints live-e2e 1
[[ "$(http_code http://live-e2e/fetch)" == "200" ]] && ok "requests succeed while healthy" || fail "/fetch while healthy"
probe="$(jp swiftsandbox/live-e2e-0 '{.spec.livenessProbe.httpGet.path}:{.spec.livenessProbe.periodSeconds}:{.spec.livenessProbe.failureThreshold}')"
[[ "$probe" == "/fetch:3:2" ]] && ok "liveness check passed to KubeSwift as spec.livenessProbe" || fail "livenessProbe: $probe"
since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
uid1="$(uid_of live-e2e-0)"

$K exec e2e-live-target -- rm /www/alive
t0=$SECONDS
until_true "liveness failure detected: sandbox Failed" is_failed live-e2e-0 "$uid1"
echo "   failure to Failed: $((SECONDS - t0))s; sandbox status: $(jp swiftsandbox/live-e2e-0 '{.status.message}{" ["}{range .status.conditions[*]}{.type}={.reason} {end}{"]"}')"
until_true "SpinApp reports 0 ready replicas" ready_replicas live-e2e 0
echo "   SpinApp Available: $(reason live-e2e Available); Progressing: $(reason live-e2e Progressing)"
t1=$SECONDS
until_true "failed sandbox replaced (new UID)" new_uid live-e2e-0 "$uid1"
echo "   Failed to replacement created: $((SECONDS - t1))s"
uid2="$(uid_of live-e2e-0)"
# The target is still broken, so the replacement fails as well; the second
# replacement waits longer (backoff).
until_true "replacement fails while the target is still broken" is_failed live-e2e-0 "$uid2"
$K exec e2e-live-target -- sh -c 'echo ok > /www/alive'
t2=$SECONDS
until_true "second replacement created after backoff" new_uid live-e2e-0 "$uid2"
echo "   Failed to second replacement created: $((SECONDS - t2))s"
uid3="$(uid_of live-e2e-0)"
until_true "healthy replacement Available" has_reason live-e2e Available ApplicationReady
echo "   health restored to Available: $((SECONDS - t2))s"
until_true "one ready endpoint" has_endpoints live-e2e 1
[[ "$(http_code http://live-e2e/fetch)" == "200" ]] && ok "requests succeed after replacement" || fail "/fetch after replacement"
has_count live-e2e 1 && ok "no stale sandbox remains" || fail "$(count live-e2e) sandboxes for live-e2e"
nps="$($K get networkpolicy -o name | grep -c 'live-e2e' || true)"
[[ "$nps" -eq 1 ]] && ok "one sandbox NetworkPolicy" || fail "$nps NetworkPolicies for live-e2e"
before="$(failed_events live-e2e)"
sleep 90
if [[ "$(uid_of live-e2e-0)" == "$uid3" && "$(phase_of live-e2e-0)" == "Running" ]] && has_reason live-e2e Available ApplicationReady \
  && [[ "$(failed_events live-e2e)" -eq "$before" ]]; then
  ok "stable for 90s after recovery: same sandbox, no further failures"
else
  fail "not stable after recovery"
fi
errors="$(kubectl -n "$(controller_ns)" logs -l app.kubernetes.io/name=kubeswift-spin --since-time="$since" --tail=-1 | grep -c '"level":"error"' || true)"
[[ "$errors" -eq 0 ]] && ok "no controller errors during failure and replacement" || fail "$errors controller error lines during phase 8"
$K delete spinapp live-e2e --wait=false >/dev/null
fi

if want 9; then
log "phase 9: teardown"
# shellcheck disable=SC2086
$K delete spinapp $APPS --ignore-not-found --wait=false >/dev/null
none_left() {
  local a
  for a in $APPS; do
    $K get spinapp "$a" >/dev/null 2>&1 && return 1
    [[ "$(count "$a")" -eq 0 ]] || return 1
    $K get service "$a" >/dev/null 2>&1 && return 1
    $K get networkpolicy -o name | grep -q "/$a-[0-9]*-restricted" && return 1
  done
  return 0
}
until_true "no SpinApp, sandbox, Service or sandbox NetworkPolicy left" none_left
fi

if [[ $FAILED -ne 0 ]]; then
  echo "KVM e2e FAILED" >&2
  exit 1
fi
log "KVM e2e passed"
