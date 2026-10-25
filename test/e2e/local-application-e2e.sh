#!/usr/bin/env bash
# Local application end-to-end test (ADR 0015): warm replicas start Spin
# from a KubeSwift-mounted, verified copy of their own application artifact
# in real KubeSwift microVMs. Requires a cluster prepared as described in
# test/e2e/README.md ("Local applications").
#
# Phases:
#   1 functional   each example on a warm pool: the placement is prepared,
#                  a new replica mounts the application, the entrypoint
#                  reports "local application used" in the guest console,
#                  and the application answers through its Service
#   2 offline      (private registry) with the application placed, the
#                  registry's Service is pointed at no pod, so nothing can
#                  reach the registry; a fresh warm replica still starts
#                  from the local copy and serves; then it is restored
#   3 binding      (private registry) the same digest under another
#                  repository the SpinApp has no credentials for is never
#                  prepared or mounted, although the node holds the digest
#   4 credentials  (private registry) with an invalid pull Secret, and then
#                  with the Secret removed, new replicas do not mount the
#                  application; the Secret is restored
#   5 fallback     a tag reference and a component selection are not
#                  eligible: the condition says why and the replica serves
#                  from the registry; a registry KubeSwift would reach over
#                  plain HTTP is not eligible and is never placed
#
# Environment:
#   E2E_CONTEXT    the kubectl context to run in; must equal the current
#                  context (required)
#   E2E_NAMESPACE  namespace with the label spin.kubeswift.io/local-application=
#                  enabled and a Ready SwiftKernel "sandbox" with warm mounts
#                  and a console (default kubeswift-spin-e2e)
#   LA_IMAGES      file with one "<example> <repository@sha256:...>" line
#                  per example: hello-http, multi-component, request-info,
#                  outbound-http, key-value, serverless-ai, mcp-tools, and
#                  "hello-http-tag" (the hello-http artifact by tag) (required)
#   LA_PRIVATE_REGISTRY_DIR  state directory of test/e2e/private-registry.sh
#                  up; enables phases 2 to 4. KubeSwift must trust its CA
#                  (sandboxMaterialize.registryCABundle). LA_IMAGES must then
#                  also list "private-hello-http" (hello-http copied into
#                  that registry) and "private-hello-http-b" (the same
#                  artifact under another repository there)
#   LA_RUNTIME_IMAGE  runtime image of the executor and pool, if not the
#                  chart's (repo@sha256:...; it must be allowed, chart value
#                  localApplication.extraRuntimes); in private mode one that
#                  trusts the registry's CA (private-registry.sh runtime)
#   E2E_PHASES     phases to run (default "1 2 3 4 5")
#   TIMEOUT        seconds to wait for each step (default 600)
#   KEEP=1         keep the test objects afterwards
set -euo pipefail

NS="${E2E_NAMESPACE:-kubeswift-spin-e2e}"
TIMEOUT="${TIMEOUT:-600}"
PHASES=" ${E2E_PHASES:-1 2 3 4 5} "
CURL_IMAGE="curlimages/curl:8.16.0@sha256:463eaf6072688fe96ac64fa623fe73e1dbe25d8ad6c34404a669ad3ce1f104b6"
GO_IMAGE="golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
K="kubectl -n $NS"
EXEC=kss-la-e2e
POOL=kss-la-e2e
FAILED=0
WORK="$(mktemp -d)"
RUN_ID="$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
LLM_TOKEN="kss-la-e2e-llm-$RUN_ID-token"

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
reason() { jp "spinapp/$1" '{.status.conditions[?(@.type=="LocalApplication")].reason}'; }
has_reason() { [[ "$(reason "$1")" == "$2" ]]; }
ready_replicas() { [[ "$(jp "spinapp/$1" '{.status.readyReplicas}')" == "$2" ]]; }
artifact_state() { jp "swiftsandbox/$1" '{.status.artifacts[?(@.name=="application")].state}/{.status.artifacts[?(@.name=="application")].reason}'; }
carries() { [[ -n "$(jp "swiftsandbox/$1" '{.spec.artifacts[?(@.name=="application")].ref}')" ]]; }
endpoints() { $K get endpointslices -l "kubernetes.io/service-name=$1" -o jsonpath='{range .items[*].endpoints[?(@.conditions.ready==true)]}{.addresses[0]}{"\n"}{end}' 2>/dev/null | grep -c . ; }
has_endpoints() { [[ "$(endpoints "$1")" -ge "$2" ]]; }
http() { $K exec e2e-client -- curl -sS --max-time 20 --retry 3 --retry-delay 1 --retry-connrefused "$@"; }
image_of() { awk -v n="$1" '$1 == n {print $2}' "$LA_IMAGES"; }
# console prints the guest console of sandbox $1 (KubeSwift v0.20 console
# container of its launcher pod; reading logs only).
console() { $K logs "$(jp "swiftsandbox/$1" '{.status.podRef}')" -c console 2>/dev/null; }
used_local() { console "$1" | grep -q "local application used from /opt/kubeswift-spin-app"; }

[[ -n "${E2E_CONTEXT:-}" ]] || die "set E2E_CONTEXT to the kubectl context of the test cluster"
[[ "$(kubectl config current-context)" == "$E2E_CONTEXT" ]] || die "the current kubectl context is not E2E_CONTEXT ($E2E_CONTEXT)"
[[ -n "${LA_IMAGES:-}" && -f "$LA_IMAGES" ]] || die "set LA_IMAGES to a file of example image references"
CTL_NS="$(kubectl get deploy -A -l app.kubernetes.io/name=kubeswift-spin -o jsonpath='{.items[0].metadata.namespace}')"
[[ -n "$CTL_NS" ]] || die "kubeswift-spin is not installed"
CM="$(kubectl -n "$CTL_NS" get deploy -l app.kubernetes.io/component=cache-manager -o name | head -1)"
[[ -n "$CM" ]] || die "local applications are not enabled (chart value localApplication.enabled)"
kubectl -n "$CTL_NS" get "$CM" -o jsonpath='{.spec.template.spec.containers[0].args}' | grep -q -- '--local-application"' \
  || die "the cache manager does not run local applications (chart value localApplication.enabled)"
[[ "$(kubectl get ns "$NS" -o jsonpath='{.metadata.labels.spin\.kubeswift\.io/local-application}')" == enabled ]] \
  || die "namespace $NS lacks the label spin.kubeswift.io/local-application=enabled"
RUNTIME="${LA_RUNTIME_IMAGE:-$(kubectl -n "$CTL_NS" get "$CM" -o jsonpath='{.spec.template.spec.containers[0].args}' | tr ',' '\n' | sed -n 's/.*--runtime-image=\([^"]*\).*/\1/p')}"
[[ "$RUNTIME" == *@sha256:* ]] || die "the runtime image must be pinned by digest"
PRIV="${LA_PRIVATE_REGISTRY_DIR:-}"
PULL=""
EGRESS='{"service":{"name":"upstream"},"ports":[{"port":8090}]}'
if [[ -n "$PRIV" ]]; then
  [[ -f "$PRIV/host" ]] || die "LA_PRIVATE_REGISTRY_DIR: no $PRIV/host (run test/e2e/private-registry.sh up)"
  PULL=e2e-registry-auth
  EGRESS+=',{"service":{"name":"e2e-registry"},"ports":[{"port":5000}]}'
else
  PHASES="${PHASES// 2 / }"; PHASES="${PHASES// 3 / }"; PHASES="${PHASES// 4 / }"
  log "no LA_PRIVATE_REGISTRY_DIR: phases 2, 3 and 4 are skipped"
fi
registry_scaled=""
secret_broken=""

# drop_placements deletes the test pool's application placements. Once
# their SpinApps are gone they are retained, and a placement younger than an
# hour counts against the namespace's placement limits (ADR 0015), so each
# test removes its own.
drop_placements() {
  $K get swiftartifactplacement -l spin.kubeswift.io/component=application-placement \
    -o jsonpath="{range .items[?(@.spec.poolRef.name==\"$POOL\")]}{.metadata.name}{'\\n'}{end}" 2>/dev/null \
    | xargs -r $K delete swiftartifactplacement --ignore-not-found >/dev/null 2>&1 || true
}
# done_app deletes the SpinApps of a finished test and the test pool's
# placements.
done_app() {
  $K delete spinapp "$@" --wait=true >/dev/null
  drop_placements
}

cleanup() {
  if [[ -n "$registry_scaled" ]]; then
    $K patch svc e2e-registry --type merge -p '{"spec":{"selector":{"app":"e2e-registry"}}}' >/dev/null 2>&1 || true
  fi
  if [[ -n "$secret_broken" ]]; then
    $K delete secret "$PULL" --ignore-not-found >/dev/null 2>&1 || true
    PRIVATE_REGISTRY_DIR="$PRIV" "$ROOT/test/e2e/private-registry.sh" secret "$NS" "$PULL" >/dev/null 2>&1 || true
  fi
  [[ "${KEEP:-}" == "1" ]] && return 0
  $K delete spinapp -l app.kubernetes.io/created-by=kss-la-e2e --ignore-not-found --wait=true >/dev/null 2>&1 || true
  drop_placements
  $K delete pod e2e-client upstream --ignore-not-found --wait=false >/dev/null 2>&1 || true
  $K delete svc upstream --ignore-not-found >/dev/null 2>&1 || true
  $K delete configmap upstream-src --ignore-not-found >/dev/null 2>&1 || true
  $K delete secret kss-la-e2e-llm-token --ignore-not-found >/dev/null 2>&1 || true
  $K delete spinappexecutor "$EXEC" --ignore-not-found >/dev/null 2>&1 || true
  $K delete swiftsandboxpool "$POOL" --ignore-not-found >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# app writes SpinApp $1 with image $2, $3 replicas and optionally a pull
# Secret ($4: "pull" or ""); the rest of the spec comes from standard input.
app() {
  local name="$1" image="$2" replicas="$3" pull="${4:-}"
  [[ -n "$image" ]] || { fail "no image for $name"; return 1; }
  { cat <<YAML
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: $name
  labels: {app.kubernetes.io/created-by: kss-la-e2e}
spec:
  image: $image
  executor: $EXEC
  replicas: $replicas
${pull:+  imagePullSecrets: [{name: $PULL\}]}
  checks:
    readiness:
      httpGet: {path: /healthz, httpHeaders: []}
      initialDelaySeconds: 1
      periodSeconds: 2
YAML
    cat; } | $K apply -f - >/dev/null
}
scale() { $K patch spinapp "$1" --type merge -p "{\"spec\":{\"replicas\":$2}}" >/dev/null; }

log "setup: upstream mock, client, warm pool and executor in $NS"
$K create configmap upstream-src --from-file=main.go="$ROOT/examples/tools/upstream/main.go" --dry-run=client -o yaml | $K apply -f - >/dev/null
(umask 077 && printf '%s' "$LLM_TOKEN" >"$WORK/token")
$K create secret generic kss-la-e2e-llm-token --from-file=token="$WORK/token" --dry-run=client -o yaml | $K apply -f - >/dev/null
rm -f "$WORK/token"
$K delete pod upstream --ignore-not-found --wait=true >/dev/null
cat <<YAML | $K apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: upstream
  labels: {app: kss-la-e2e-upstream}
spec:
  containers:
    - name: upstream
      image: $GO_IMAGE
      command: [go, run, /src/main.go, --listen, "0.0.0.0:8090"]
      env:
        - {name: GOCACHE, value: /tmp/gocache}
        - {name: GOFLAGS, value: -mod=mod}
        - name: UPSTREAM_REQUIRE_TOKEN
          valueFrom: {secretKeyRef: {name: kss-la-e2e-llm-token, key: token}}
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
  selector: {app: kss-la-e2e-upstream}
  ports: [{port: 8090, targetPort: 8090}]
---
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata:
  name: $POOL
spec:
  image: $RUNTIME
${PULL:+  imagePullSecret: $PULL}
  cpu: 1
  memory: 512Mi
  network:
    mode: restricted
    ports: [{name: http-app, port: 3000}]
    egress:
      allow: [$EGRESS]
  minWarm: 3
---
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinAppExecutor
metadata:
  name: $EXEC
  labels: {spin.kubeswift.io/managed-by: kubeswift-spin}
  annotations:
    spin.kubeswift.io/sandbox-pool: $POOL
    spin.kubeswift.io/local-application: enabled
    spin.kubeswift.io/runtime-image: '$RUNTIME'
    spin.kubeswift.io/egress-allow: '[$EGRESS]'
spec:
  createDeployment: false
YAML
$K delete pod e2e-client --ignore-not-found --wait=true >/dev/null
$K run e2e-client --restart=Never --image="$CURL_IMAGE" --command -- sleep 7200 >/dev/null
$K wait --for=condition=Ready pod/e2e-client pod/upstream --timeout=300s >/dev/null || die "client or upstream not ready"
# warm waits until the pool has all its warm slots again, so that the next
# replicas are checked out rather than booted cold.
warm() { until_true "warm pool $POOL has 3 warm slots" bash -c "[[ \$($K get swiftsandboxpool $POOL -o jsonpath='{.status.warmReplicas}') -ge 3 ]]"; }

# local_replica deploys SpinApp $1 (image $2, pull "$3"; spec tail on
# standard input), waits until its placement is prepared (Attached), and
# scales to 2: the new replica must carry and mount the application, and
# the entrypoint must report using it. Each test deletes its SpinApp when
# done, so that the pool's warm slots suffice.
local_replica() {
  local name="$1"
  warm
  app "$name" "$2" 1 "${3:-}"
  until_true "$name: LocalApplication Attached" has_reason "$name" Attached || return 1
  scale "$name" 2
  until_true "$name: 2 replicas ready" ready_replicas "$name" 2 || return 1
  carries "$name-1" && ok "$name-1 carries the application" || { fail "$name-1 carries no application"; return 1; }
  [[ "$(artifact_state "$name-1")" == Mounted/* ]] && ok "$name-1 mounted it ($(artifact_state "$name-1"))" \
    || fail "$name-1 artifact $(artifact_state "$name-1")"
  until_true "$name-1: the entrypoint used the local application" used_local "$name-1"
  until_true "$name: 2 Service endpoints" has_endpoints "$name" 2
}

if want 1; then
  log "phase 1: functional, every example from local content"
  local_replica la-hello "$(image_of hello-http)" </dev/null
  [[ "$(http http://la-hello/hello)" == "Hello from Spin on KubeSwift" ]] && ok "hello-http answers" || fail "hello-http"
  done_app la-hello

  local_replica la-multi "$(image_of multi-component)" <<'YAML'
  variables: [{name: app_version, value: "2.0.0"}]
YAML
  [[ "$(http 'http://la-multi/info/x?y=1')" == *'"app_version":"2.0.0"'* ]] && ok "multi-component answers with its variable" || fail "multi-component"
  done_app la-multi

  local_replica la-info "$(image_of request-info)" <<'YAML'
  variables: [{name: app_version, value: "0.1.0"}, {name: greeting, value: hi}]
YAML
  b="$(http -H 'x-request-id: req-42' 'http://la-info/info?x=1')"
  [[ "$b" == *'"query":"x=1"'* && "$b" == *'"greeting":"hi"'* ]] && ok "request-info answers" || fail "request-info: $b"
  done_app la-info

  local_replica la-outbound "$(image_of outbound-http)" <<YAML
  variables:
    - {name: target_url, value: "http://upstream.$NS.svc.cluster.local:8090/"}
    - {name: target_origin, value: "http://upstream.$NS.svc.cluster.local:8090"}
YAML
  [[ "$(http http://la-outbound/fetch)" == *'"ok":true'* ]] && ok "outbound-http reaches its allowed target" || fail "outbound-http"
  done_app la-outbound

  local_replica la-kv "$(image_of key-value)" </dev/null
  [[ "$($K exec e2e-client -- curl -s -o /dev/null -w '%{http_code}' -X PUT --data-binary v1 http://la-kv/kv/a)" == 204 && "$(http http://la-kv/kv/a)" == v1 ]] \
    && ok "key-value stores and reads" || fail "key-value"
  done_app la-kv

  local_replica la-ai "$(image_of serverless-ai)" <<YAML
  variables: [{name: llm_model, value: default-model}]
  runtimeConfig:
    llmCompute:
      type: remote_http
      options:
        - {name: url, value: "http://upstream.$NS.svc.cluster.local:8090"}
        - {name: api_type, value: open_ai}
        - name: auth_token
          valueFrom: {secretKeyRef: {name: kss-la-e2e-llm-token, key: token}}
YAML
  [[ "$(http -X POST --data-binary 'hello from the e2e test' http://la-ai/ask)" == *'mock completion from model \"default-model\"'* ]] \
    && ok "serverless-ai answers with a Secret-backed token" || fail "serverless-ai"
  done_app la-ai

  local_replica la-mcp "$(image_of mcp-tools)" </dev/null
  [[ "$(http http://la-mcp/mcp -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"word_count","arguments":{"text":"a b  c"}}}')" == *'"text":"3"'* ]] \
    && ok "mcp answers a tool call" || fail "mcp"
  done_app la-mcp
fi

if want 2; then
  log "phase 2: offline, the registry unreachable after placement"
  local_replica la-offline "$(image_of private-hello-http)" pull </dev/null
  $K patch svc e2e-registry --type merge -p '{"spec":{"selector":{"app":"kss-la-e2e-nothing"}}}' >/dev/null
  registry_scaled=1
  until_true "registry unreachable" bash -c "! $K exec e2e-client -- curl -sk --max-time 5 https://e2e-registry:5000/v2/ >/dev/null"
  # A fresh replica within KubeSwift's authorization TTL (default 5
  # minutes): its checkout uses the cached authorization, the guest needs
  # no registry.
  scale la-offline 3
  until_true "la-offline-2 ready with the registry down" bash -c "[[ \$($K get spinapp la-offline -o jsonpath='{.status.readyReplicas}') == 3 ]]"
  [[ "$(artifact_state la-offline-2)" == Mounted/* ]] && ok "la-offline-2 mounted the application" || fail "la-offline-2 artifact $(artifact_state la-offline-2)"
  until_true "la-offline-2: the entrypoint used the local application" used_local la-offline-2
  [[ "$(http http://la-offline/hello)" == "Hello from Spin on KubeSwift" ]] && ok "served with the registry down" || fail "no answer with the registry down"
  $K patch svc e2e-registry --type merge -p '{"spec":{"selector":{"app":"e2e-registry"}}}' >/dev/null
  registry_scaled=""
  until_true "registry reachable again" bash -c "$K exec e2e-client -- curl -sk --max-time 5 -o /dev/null https://e2e-registry:5000/v2/"
  done_app la-offline
fi

if want 3; then
  log "phase 3: repository binding, the same digest under a repository without credentials"
  # hello-http's digest is on the pool's nodes from phase 1 or 2. The same
  # digest under private-hello-http-b, without a pull Secret: KubeSwift must
  # not prepare or mount it.
  warm
  app la-binding "$(image_of private-hello-http-b)" 1 </dev/null
  until_true "la-binding: placement fails (PlacementFailed)" has_reason la-binding PlacementFailed
  carries la-binding-0 && fail "la-binding-0 carries the application" || ok "la-binding-0 carries no application"
  done_app la-binding
fi

if want 4; then
  log "phase 4: credentials, an invalid pull Secret"
  local_replica la-creds "$(image_of private-hello-http)" pull </dev/null
  # The pool pulls the runtime image with the same Secret, so it cannot
  # refill once the Secret is broken: fill it for both replicas below now.
  warm
  $K create secret docker-registry "$PULL" --docker-server="$(cat "$PRIV/host")" --docker-username=nobody \
    --docker-password="invalid-$RUN_ID" --dry-run=client -o yaml | $K apply -f - >/dev/null
  secret_broken=1
  scale la-creds 3
  until_true "la-creds-2 created" bash -c "$K get swiftsandbox la-creds-2 >/dev/null"
  until_true "la-creds-2: the application decided" bash -c "[[ -n \"\$($K get swiftsandbox la-creds-2 -o jsonpath='{.status.artifacts[?(@.name==\"application\")].state}' 2>/dev/null)\" ]] || ! $K get swiftsandbox la-creds-2 -o jsonpath='{.spec.artifacts[*].name}' | grep -q application"
  [[ "$(artifact_state la-creds-2)" != Mounted/* ]] && ok "la-creds-2 did not mount the application with invalid credentials ($(artifact_state la-creds-2))" \
    || fail "la-creds-2 mounted the application with invalid credentials"
  # The pull Secret removed: new replicas do not mount the application.
  $K delete secret "$PULL" >/dev/null
  scale la-creds 4
  until_true "la-creds-3 created" bash -c "$K get swiftsandbox la-creds-3 >/dev/null"
  until_true "la-creds-3: the application decided" bash -c "[[ -n \"\$($K get swiftsandbox la-creds-3 -o jsonpath='{.status.artifacts[?(@.name==\"application\")].state}' 2>/dev/null)\" ]] || ! $K get swiftsandbox la-creds-3 -o jsonpath='{.spec.artifacts[*].name}' | grep -q application"
  [[ "$(artifact_state la-creds-3)" != Mounted/* ]] && ok "la-creds-3 did not mount the application without its pull Secret ($(artifact_state la-creds-3))" \
    || fail "la-creds-3 mounted the application without its pull Secret"
  PRIVATE_REGISTRY_DIR="$PRIV" "$ROOT/test/e2e/private-registry.sh" secret "$NS" "$PULL" >/dev/null
  secret_broken=""
  done_app la-creds
fi

if want 5; then
  log "phase 5: fallback for SpinApps that are not eligible"
  warm
  app la-tag "$(image_of hello-http-tag)" 1 </dev/null
  until_true "la-tag: ImageNotPinned" has_reason la-tag ImageNotPinned
  until_true "la-tag: ready from the registry" ready_replicas la-tag 1
  carries la-tag-0 && fail "la-tag-0 carries the application" || ok "la-tag-0 carries no application"
  app la-comp "$(image_of multi-component)" 1 <<'YAML'
  components: [hello]
YAML
  until_true "la-comp: ComponentsSelected" has_reason la-comp ComponentsSelected
  until_true "la-comp: ready from the registry" ready_replicas la-comp 1
  done_app la-tag la-comp
  # A registry KubeSwift would reach over plain HTTP (a private-range IP
  # literal) is never eligible; nothing listens there, so the replica only
  # has to be created.
  hello="$(image_of hello-http)"
  app la-plain "10.255.255.1:5000/apps/hello-http@${hello##*@}" 1 </dev/null
  until_true "la-plain: RegistryNotEligible" has_reason la-plain RegistryNotEligible
  until_true "la-plain-0 created" bash -c "$K get swiftsandbox la-plain-0 >/dev/null"
  carries la-plain-0 && fail "la-plain-0 carries the application" || ok "la-plain-0 carries no application"
  [[ -z "$($K get swiftartifactplacement -o jsonpath='{range .items[*]}{.spec.artifacts[*].ref}{"\n"}{end}' | grep '^10\.255\.255\.1:')" ]] \
    && ok "no placement names the plain-HTTP registry" || fail "a placement names the plain-HTTP registry"
  done_app la-plain
fi

[[ $FAILED -eq 0 ]] && log "local application e2e: all checks passed" || { log "local application e2e: FAILED"; exit 1; }
