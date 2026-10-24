#!/usr/bin/env bash
# Compiled-cache end-to-end test (ADR 0014): compiles, signs, places and
# mounts trusted Wasmtime caches for the example applications in real
# KubeSwift microVMs, and checks the security boundaries with the released
# KubeSwift export, verification and placement features. Requires a cluster
# prepared as described in test/e2e/README.md ("Compiled caches").
#
# Phases:
#   1 functional   each example on a warm pool with a compiled cache: the
#                  record becomes Ready, a checkout mounts the cache from the
#                  prepared node, and the application answers through its
#                  Service
#   2 boundaries   RBAC of a tenant; forged records (unsigned export, another
#                  application's artifact, a tenant repository, a wrong-key
#                  signature) are never mounted and never stop a replica;
#                  no key or credential reaches a compiler or a replica
#   3 restart      restarting the cache manager and the signer during a
#                  compilation neither duplicates nor trusts it early
#   5 fallback     a replica whose cache KubeSwift skips (not on its node,
#                  a pull Secret it cannot use, a credential the registry
#                  refuses) starts and becomes ready without it, is never
#                  replaced, and starts no hold; a replica that boots cold
#                  mounts the cache its node already holds; a placement
#                  the API server refuses leaves the SpinApp Available; a
#                  controller restart leaves running replicas alone
#   4 teardown     placements go with their records' consumers (runs last)
#
# Environment:
#   E2E_CONTEXT    the kubectl context to run in; must equal the current
#                  context, so the script never runs against another
#                  cluster by accident (required)
#   E2E_NAMESPACE  enabled namespace (label spin.kubeswift.io/compiled-cache=
#                  enabled) with a Ready SwiftKernel "sandbox" whose kernel
#                  supports outputs and warm mounts (default kubeswift-spin-e2e)
#   CC_IMAGES      file with one "<example> <repository@sha256:...>" line per
#                  example: hello-http, multi-component, request-info,
#                  outbound-http, key-value, serverless-ai, mcp-tools; and
#                  "request-info-b" (the request-info artifact under another
#                  repository) for phases 2 and 3, and "hello-http-b" (the
#                  hello-http artifact under another repository, never placed
#                  before) for phase 5 (required)
#   CC_WRONG_KEY_JOB  phase 2 also signs an export with a key no policy
#                  names, in a Job in this namespace that can push to the
#                  cache repository with this docker-registry Secret
#                  (optional). It is a push credential for the cache
#                  repository in the tenant test namespace: create it for the
#                  run only and delete it afterwards.
#   CC_PRIVATE_REGISTRY_DIR  run against the registry behind a private CA
#                  that test/e2e/private-registry.sh started (its state
#                  directory): the pool, executor, compilers and replicas
#                  reach it (egress), pull with its docker-registry Secret
#                  e2e-registry-auth, and phase 5 tests the cache pull
#                  Secret. KubeSwift and the signer must trust its CA, and
#                  CC_IMAGES and CC_RUNTIME_IMAGE must point into it
#                  (optional)
#   CC_RUNTIME_IMAGE  runtime image of the executor and pool, if not the
#                  chart's (repo@sha256:...; it must be in
#                  compiledCache.extraRuntimes)
#   CC_CACHE_PULL_SECRET  the chart's compiledCache.pullSecretName, a Secret
#                  in this namespace; phase 5 breaks and restores it (needs
#                  CC_PRIVATE_REGISTRY_DIR, which recreates it)
#   E2E_PHASES     phases to run (default "1 2 3 5 4")
#   TIMEOUT        seconds to wait for each step (default 600)
#   KEEP=1         keep the test objects afterwards
set -euo pipefail

NS="${E2E_NAMESPACE:-kubeswift-spin-e2e}"
TIMEOUT="${TIMEOUT:-600}"
PHASES=" ${E2E_PHASES:-1 2 3 5 4} "
CURL_IMAGE="curlimages/curl:8.16.0@sha256:463eaf6072688fe96ac64fa623fe73e1dbe25d8ad6c34404a669ad3ce1f104b6"
GO_IMAGE="golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
K="kubectl -n $NS"
EXEC=kss-cc-e2e
POOL=kss-cc-e2e
FAILED=0
WORK="$(mktemp -d)"
RUN_ID="$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
LLM_TOKEN="kss-cc-e2e-llm-$RUN_ID-token"
TENANT="system:serviceaccount:$NS:kss-cc-tenant"

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
artifact_state() { jp "swiftsandbox/$1" '{.status.artifacts[?(@.name=="wasmtime-cache")].state}/{.status.artifacts[?(@.name=="wasmtime-cache")].reason}'; }
artifact_is() { [[ "$(artifact_state "$1")" == "$2"* ]]; }
artifact_decided() { case "$(artifact_state "$1")" in Mounted/* | Skipped/* | Failed/*) return 0 ;; esac; return 1; }
endpoints() { $K get endpointslices -l "kubernetes.io/service-name=$1" -o jsonpath='{range .items[*].endpoints[?(@.conditions.ready==true)]}{.addresses[0]}{"\n"}{end}' 2>/dev/null | grep -c . ; }
has_endpoints() { [[ "$(endpoints "$1")" -ge "$2" ]]; }
http() { $K exec e2e-client -- curl -sS --max-time 20 --retry 3 --retry-delay 1 --retry-connrefused "$@"; }
image_of() { awk -v n="$1" '$1 == n {print $2}' "$CC_IMAGES"; }
# pod_ip prints the IP of the launcher pod of sandbox $1 (a warm slot's pod
# is named after its pool).
pod_ip() { jp "pod/$(jp "swiftsandbox/$1" '{.status.podRef}')" '{.status.podIP}'; }

[[ -n "${E2E_CONTEXT:-}" ]] || die "set E2E_CONTEXT to the kubectl context of the test cluster"
[[ "$(kubectl config current-context)" == "$E2E_CONTEXT" ]] || die "the current kubectl context is not E2E_CONTEXT ($E2E_CONTEXT)"
[[ -n "${CC_IMAGES:-}" && -f "$CC_IMAGES" ]] || die "set CC_IMAGES to a file of example image references"
CTL_NS="$(kubectl get deploy -A -l app.kubernetes.io/name=kubeswift-spin -o jsonpath='{.items[0].metadata.namespace}')"
[[ -n "$CTL_NS" ]] || die "kubeswift-spin is not installed"
CM="$(kubectl -n "$CTL_NS" get deploy -l app.kubernetes.io/component=cache-manager -o name | head -1)"
SIGNER="$(kubectl -n "$CTL_NS" get deploy -l app.kubernetes.io/component=cache-signer -o name | head -1)"
[[ -n "$CM" && -n "$SIGNER" ]] || die "compiled caches are not enabled (chart value compiledCache.enabled)"
[[ "$(kubectl get ns "$NS" -o jsonpath='{.metadata.labels.spin\.kubeswift\.io/compiled-cache}')" == enabled ]] \
  || die "namespace $NS lacks the label spin.kubeswift.io/compiled-cache=enabled"
RUNTIME="${CC_RUNTIME_IMAGE:-$(kubectl -n "$CTL_NS" get "$CM" -o jsonpath='{.spec.template.spec.containers[0].args}' | tr ',' '\n' | sed -n 's/.*--runtime-image=\([^"]*\).*/\1/p')}"
[[ "$RUNTIME" == *@sha256:* ]] || die "the runtime image must be pinned by digest"
# Private mode: everything is pulled from the registry behind a private CA
# with the Secret private-registry.sh created.
PRIV="${CC_PRIVATE_REGISTRY_DIR:-}"
PULL=""
EGRESS='{"service":{"name":"upstream"},"ports":[{"port":8090}]}'
if [[ -n "$PRIV" ]]; then
  [[ -f "$PRIV/host" ]] || die "CC_PRIVATE_REGISTRY_DIR: no $PRIV/host (run test/e2e/private-registry.sh up)"
  PULL=e2e-registry-auth
  EGRESS+=',{"service":{"name":"e2e-registry"},"ports":[{"port":5000}]}'
  [[ "$RUNTIME" == "$(cat "$PRIV/host")/"* ]] || die "in private mode CC_RUNTIME_IMAGE must be in the private registry"
fi
CACHE_PULL="${CC_CACHE_PULL_SECRET:-}"

# pool_prepared reports whether record $1's placement on the test pool is
# prepared on every node.
pool_prepared() { [[ "$(rec "$1" "{.status.placements[?(@.pool==\"$POOL\")].prepared}")" == true ]]; }

# record_of prints the record a SpinApp's CompiledCache condition names.
record_of() { reason_msg="$(jp "spinapp/$1" '{.status.conditions[?(@.type=="CompiledCache")].message}')"; grep -o 'spincc-[0-9a-f]\{32\}' <<<"$reason_msg" | head -1; }
rec() { kubectl -n "$CTL_NS" get spincompiledcache "$1" -o jsonpath="$2" 2>/dev/null; }

SIGNER_REPLICAS="$(kubectl -n "$CTL_NS" get "$SIGNER" -o jsonpath='{.spec.replicas}')"
CM_REPLICAS="$(kubectl -n "$CTL_NS" get "$CM" -o jsonpath='{.spec.replicas}')"
sec_record=""
pull_broken=""

# Even with KEEP=1, the platform is left as found: the signer runs again and
# a record whose status was forged is removed.
cleanup() {
  kubectl -n "$CTL_NS" scale "$SIGNER" --replicas="${SIGNER_REPLICAS:-1}" >/dev/null 2>&1 || true
  kubectl -n "$CTL_NS" scale "$CM" --replicas="${CM_REPLICAS:-1}" >/dev/null 2>&1 || true
  $K delete resourcequota kss-cc-e2e-no-placements --ignore-not-found >/dev/null 2>&1 || true
  if [[ -n "$pull_broken" ]]; then
    $K delete secret "$CACHE_PULL" --ignore-not-found >/dev/null 2>&1 || true
    PRIVATE_REGISTRY_DIR="$PRIV" "$ROOT/test/e2e/private-registry.sh" secret "$NS" "$CACHE_PULL" >/dev/null 2>&1 || true
  fi
  if [[ -n "$sec_record" ]]; then
    kubectl -n "$CTL_NS" delete spincompiledcache "$sec_record" --ignore-not-found >/dev/null 2>&1 || true
  fi
  [[ "${KEEP:-}" == "1" ]] && return 0
  $K delete spinapp -l app.kubernetes.io/created-by=kss-cc-e2e --ignore-not-found --wait=true >/dev/null 2>&1 || true
  $K delete pod e2e-client upstream --ignore-not-found --wait=false >/dev/null 2>&1 || true
  $K delete svc upstream --ignore-not-found >/dev/null 2>&1 || true
  $K delete configmap upstream-src --ignore-not-found >/dev/null 2>&1 || true
  $K delete secret kss-cc-e2e-llm-token --ignore-not-found >/dev/null 2>&1 || true
  $K delete job kss-cc-e2e-wrong-key --ignore-not-found >/dev/null 2>&1 || true
  $K delete spinappexecutor "$EXEC" --ignore-not-found >/dev/null 2>&1 || true
  $K delete swiftsandboxpool "$POOL" --ignore-not-found >/dev/null 2>&1 || true
  $K delete rolebinding,role,serviceaccount kss-cc-tenant --ignore-not-found >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# app writes a SpinApp of example $2 named $1 with $3 replicas; the rest of
# the spec comes from standard input.
app() {
  local name="$1" example="$2" replicas="$3" image
  image="$(image_of "$example")"
  [[ -n "$image" ]] || { fail "no image for $example in $CC_IMAGES"; return 1; }
  { cat <<YAML
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinApp
metadata:
  name: $name
  labels: {app.kubernetes.io/created-by: kss-cc-e2e}
spec:
  image: $image
  executor: $EXEC
  replicas: $replicas
${PULL:+  imagePullSecrets: [{name: $PULL\}]}
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
$K create secret generic kss-cc-e2e-llm-token --from-file=token="$WORK/token" --dry-run=client -o yaml | $K apply -f - >/dev/null
rm -f "$WORK/token"
# The mock reads the token when it starts: a mock left from an earlier run
# would hold another one.
$K delete pod upstream --ignore-not-found --wait=true >/dev/null
cat <<YAML | $K apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: upstream
  labels: {app: kss-cc-e2e-upstream}
spec:
  containers:
    - name: upstream
      image: $GO_IMAGE
      command: [go, run, /src/main.go, --listen, "0.0.0.0:8090"]
      env:
        - {name: GOCACHE, value: /tmp/gocache}
        - {name: GOFLAGS, value: -mod=mod}
        - name: UPSTREAM_REQUIRE_TOKEN
          valueFrom: {secretKeyRef: {name: kss-cc-e2e-llm-token, key: token}}
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
  selector: {app: kss-cc-e2e-upstream}
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
    spin.kubeswift.io/compiled-cache: enabled
    spin.kubeswift.io/runtime-image: '$RUNTIME'
    spin.kubeswift.io/egress-allow: '[$EGRESS]'
${PULL:+    spin.kubeswift.io/runtime-image-pull-secret: $PULL}
spec:
  createDeployment: false
YAML
$K delete pod e2e-client --ignore-not-found --wait=true >/dev/null
$K run e2e-client --restart=Never --image="$CURL_IMAGE" --command -- sleep 7200 >/dev/null
$K wait --for=condition=Ready pod/e2e-client pod/upstream --timeout=300s >/dev/null || die "client or upstream not ready"
until_true "warm pool $POOL has 3 warm slots" bash -c "[[ \$($K get swiftsandboxpool $POOL -o jsonpath='{.status.warmReplicas}') -ge 3 ]]"

# cached_checkout deploys example $2 as SpinApp $1 (spec tail on standard
# input), waits for its record to be Ready (compiled, signed, placed on the
# pool's nodes), and scales to 2: the second replica must mount the cache
# from its prepared node. The first replica may have raced the placement.
cached_checkout() {
  local name="$1" example="$2" spec
  spec="$(cat)"
  app "$name" "$example" 1 <<<"$spec"
  until_true "$name: Available without waiting for the cache" ready_replicas "$name" 1
  until_true "$name: CompiledCache=True/Ready (compiled, signed, placed)" has_reason "$name" CompiledCache Ready
  local r
  r="$(record_of "$name")"
  [[ -n "$r" ]] && printf '   record %s: compile %s -> %s, signed %s, artifact %s bytes\n' "$r" \
    "$(rec "$r" '{.status.compile.startedAt}')" "$(rec "$r" '{.status.compile.finishedAt}')" \
    "$(rec "$r" '{.status.signature.signedAt}')" "$(rec "$r" '{.status.compile.size}')"
  # Ready can be left from before this pool was a consumer's (no pool needs
  # a placement); new warm replicas get the cache once the pool's placement
  # is prepared.
  until_true "$name: the cache is prepared on the pool's nodes" pool_prepared "$r"
  scale "$name" 2
  until_true "$name: second replica decided its cache" artifact_decided "$name-1"
  if artifact_is "$name-1" Mounted/Cached; then
    ok "$name: the second replica mounted the signed cache from its node ($(artifact_state "$name-1"))"
  else
    fail "$name: the second replica did not mount the cache: $(artifact_state "$name-1")"
  fi
  until_true "$name: 2 replicas ready" ready_replicas "$name" 2
  until_true "$name: endpoints ready" has_endpoints "$name" 2
}

if want 1; then
log "phase 1: examples with compiled caches on a warm pool"
cached_checkout cc-hello hello-http </dev/null
body="$(http "http://cc-hello/hello")"
[[ "$body" == *"Hello from Spin on KubeSwift"* ]] && ok "hello-http answers" || fail "hello-http: $body"
$K delete spinapp cc-hello --wait=false >/dev/null

cached_checkout cc-multi multi-component <<'YAML'
  variables:
    - {name: app_version, value: "2.0.0"}
  runtimeConfig:
    keyValueStores:
      - name: default
        type: spin
        options: [{name: path, value: /var/lib/kubeswift-spin/state/kv.db}]
YAML
body="$(http "http://cc-multi/hello")"
[[ "$body" == *"Hello from Spin on KubeSwift"* ]] && ok "multi-component: hello component" || fail "multi-component hello: $body"
body="$(http "http://cc-multi/info/x?y=1")"
[[ "$body" == *'"app_version":"2.0.0"'* ]] && ok "multi-component: info component with a variable" || fail "multi-component info: $body"
# Each replica has its own store: write and read through one pod IP.
ip="$(pod_ip cc-multi-1)"
http -X PUT --data-binary v1 "http://$ip:3000/kv/a" >/dev/null
body="$(http "http://$ip:3000/kv/a")"
[[ "$body" == v1 ]] && ok "multi-component: key-value component" || fail "multi-component kv: $body"
$K delete spinapp cc-multi --wait=false >/dev/null

cached_checkout cc-info request-info <<'YAML'
  variables:
    - {name: app_version, value: "1.2.3"}
YAML
body="$(http -H 'Authorization: Bearer do-not-echo' "http://cc-info/info?x=1")"
[[ "$body" == *'"app_version":"1.2.3"'* && "$body" != *do-not-echo* ]] && ok "request-info answers" || fail "request-info: $body"
$K delete spinapp cc-info --wait=false >/dev/null

cached_checkout cc-outbound outbound-http <<YAML
  variables:
    - {name: target_url, value: "http://upstream.$NS.svc.cluster.local:8090/"}
    - {name: target_origin, value: "http://upstream.$NS.svc.cluster.local:8090"}
YAML
body="$(http "http://cc-outbound/fetch")"
[[ "$body" == *'"ok":true'* ]] && ok "outbound-http reaches the allowed upstream" || fail "outbound-http: $body"
$K delete spinapp cc-outbound --wait=false >/dev/null

cached_checkout cc-kv key-value <<'YAML'
  runtimeConfig:
    keyValueStores:
      - name: default
        type: spin
        options: [{name: path, value: /var/lib/kubeswift-spin/state/kv.db}]
YAML
ip="$(pod_ip cc-kv-1)"
http -X PUT --data-binary v1 "http://$ip:3000/kv/greeting" >/dev/null
body="$(http "http://$ip:3000/kv/greeting")"
[[ "$body" == v1 ]] && ok "key-value stores and reads" || fail "key-value: $body"
$K delete spinapp cc-kv --wait=false >/dev/null

cached_checkout cc-ai serverless-ai <<YAML
  runtimeConfig:
    llmCompute:
      type: remote_http
      options:
        - {name: url, value: "http://upstream.$NS.svc.cluster.local:8090"}
        - {name: api_type, value: open_ai}
        - name: auth_token
          valueFrom: {secretKeyRef: {name: kss-cc-e2e-llm-token, key: token}}
YAML
body="$(http -X POST --data-binary 'hello from the cache test' "http://cc-ai/ask")"
[[ "$body" == *'mock completion'* ]] && ok "serverless-ai answers through the LLM mock" || fail "serverless-ai: $body"
$K delete spinapp cc-ai --wait=false >/dev/null

cached_checkout cc-mcp mcp-tools </dev/null
body="$(http -H 'content-type: application/json' "http://cc-mcp/mcp" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"word_count","arguments":{"text":"a b c"}}}')"
[[ "$body" == *'"text":"3"'* ]] && ok "mcp tool call" || fail "mcp: $body"
$K delete spinapp cc-mcp --wait=false >/dev/null

$K delete spinapp -l app.kubernetes.io/created-by=kss-cc-e2e --wait=true >/dev/null
fi

# forge patches a record's status as a compromised status writer could:
# kubeswift-spin then attaches the named artifact, and KubeSwift's platform
# verification is what must refuse it.
forge() {
  local r="$1" artifact="$2"
  kubectl -n "$CTL_NS" patch spincompiledcache "$r" --subresource=status --type merge -p "$(printf \
    '{"status":{"compile":{"attempt":1,"artifact":"%s"},"signature":{"artifact":"%s","keyID":"forged","signedAt":"%s","rejected":false}}}' \
    "$artifact" "$artifact" "$(date -u +%Y-%m-%dT%H:%M:%SZ)")" >/dev/null
}

# cache_attached reports whether a SpinApp's new replicas carry a cache
# (CompiledCache=True: Ready, Placing or PlacementFailed).
cache_attached() { [[ "$(jp "spinapp/$1" '{.status.conditions[?(@.type=="CompiledCache")].status}')" == True ]]; }

# placement_refused reports whether KubeSwift refused to prepare record $1's
# artifact: its platform verification failed, a claim did not match, or a
# policy denied it.
placement_refused() {
  case "$(rec "$1" '{.status.placements[0].reason}')" in
    VerificationFailed | ClaimMismatch | PolicyDenied | ArtifactRefused | PrepareFailed) return 0 ;;
  esac
  return 1
}

# refused checks that KubeSwift refused the forged artifact $3 of record $2
# for SpinApp $1: the placement fails KubeSwift's platform verification, so
# the pool is never prepared and a new replica gets no cache at all (warm
# replicas get the cache only from a prepared pool), and the SpinApp stays
# Available.
refused() {
  local name="$1" record="$2" artifact="$3" what="$4" sb="$1-1"
  until_true "$what: attached to the record" cache_attached "$name"
  until_true "$what: the placement is refused by KubeSwift" placement_refused "$record"
  printf '   placement: %s: %s\n' "$(rec "$record" '{.status.placements[0].reason}')" "$(rec "$record" '{.status.placements[0].message}' | cut -c1-200)"
  [[ "$(rec "$record" '{.status.placements[0].name}')" != "" ]] || fail "$what: no placement"
  scale "$name" 2
  until_true "$what: the SpinApp stays Available with 2 replicas" ready_replicas "$name" 2
  local carried
  carried="$(jp "swiftsandbox/$sb" '{.spec.artifacts[*].ref}')"
  if [[ -z "$carried" ]]; then
    ok "$what: a new replica got no cache from the unprepared pool"
  elif [[ "$carried" == "$artifact" && "$(artifact_state "$sb")" == Skipped/* ]]; then
    ok "$what: a new replica carried it and started without it ($(artifact_state "$sb"))"
  else
    fail "$what: replica carried [$carried], state [$(artifact_state "$sb")]"
  fi
  scale "$name" 1
  until_true "$what: back to one replica" bash -c "! $K get swiftsandbox $sb >/dev/null 2>&1"
}

if want 2; then
log "phase 2: trust boundaries"
# A tenant: may manage SpinApps and read sandboxes in its namespace.
cat <<YAML | $K apply -f - >/dev/null
apiVersion: v1
kind: ServiceAccount
metadata: {name: kss-cc-tenant}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: kss-cc-tenant}
rules:
  - {apiGroups: [core.spinkube.dev], resources: [spinapps], verbs: [get, list, watch, create, update, patch, delete]}
  - {apiGroups: [sandbox.kubeswift.io], resources: [swiftsandboxes], verbs: [get, list, watch]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: kss-cc-tenant}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: kss-cc-tenant}
subjects: [{kind: ServiceAccount, name: kss-cc-tenant, namespace: $NS}]
YAML
for check in "create spincompiledcaches -n $CTL_NS" "patch spincompiledcaches/status -n $CTL_NS" "create spincompiledcaches -n $NS" \
  "create swiftartifactplacements -n $NS" "patch swiftsandboxes/status -n $NS" "get secrets -n $CTL_NS"; do
  # shellcheck disable=SC2086
  if [[ "$(kubectl auth can-i --as="$TENANT" $check 2>/dev/null)" == no ]]; then
    ok "tenant cannot $check"
  else
    fail "tenant can $check"
  fi
done
f="$WORK/forged-record.yaml"
cat >"$f" <<YAML
apiVersion: spin.kubeswift.io/v1alpha1
kind: SpinCompiledCache
metadata: {name: spincc-00000000000000000000000000000000, namespace: $CTL_NS}
spec: {namespace: $NS, source: "$(image_of request-info)", runtime: "$RUNTIME", architecture: amd64, repository: "x.example/c/$NS/wasmtime-cache"}
YAML
if kubectl --as="$TENANT" create -f "$f" >/dev/null 2>&1; then fail "tenant created a record"; else ok "tenant's create of a record is refused"; fi

# Trusted artifacts to forge with: hello-http's, from phase 1 or now.
app cc-sec-ref hello-http 1 </dev/null
until_true "reference record Ready" has_reason cc-sec-ref CompiledCache Ready
ref_record="$(record_of cc-sec-ref)"
ref_artifact="$(rec "$ref_record" '{.status.compile.artifact}')"
# Values read from records are checked before they reach a Job's arguments.
pinned_ref() { [[ "$1" =~ ^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$ ]]; }
pinned_ref "$ref_artifact" || die "reference artifact is not a pinned reference"

# With the signer stopped, request-info-b's export stays unsigned.
kubectl -n "$CTL_NS" scale "$SIGNER" --replicas=0 >/dev/null
kubectl -n "$CTL_NS" rollout status "$SIGNER" --timeout=120s >/dev/null
app cc-sec request-info-b 1 </dev/null
until_true "cc-sec: compiled and exported, not signed" has_reason cc-sec CompiledCache Signing
until_true "cc-sec: Available while the signer is stopped" ready_replicas cc-sec 1
sec_record="$(record_of cc-sec)"
[[ -z "$sec_record" ]] && sec_record="$(kubectl -n "$CTL_NS" get spincompiledcache -o jsonpath="{range .items[?(@.spec.source==\"$(image_of request-info-b)\")]}{.metadata.name}{end}")"
unsigned="$(rec "$sec_record" '{.status.compile.artifact}')"
pinned_ref "$unsigned" || die "the exported artifact is not a pinned reference"
[[ -n "$unsigned" ]] || fail "no exported artifact for cc-sec"

# Spec inspection: no key or push credential in a compiler or a replica.
specs="$($K get swiftsandbox -o json)"
for name in kubeswift-spin-cache-e2e-key cosign.key kubeswift-spin-cache-e2e-registry kubeswift-spin-cache-e2e-export; do
  grep -q "$name" <<<"$specs" && fail "a sandbox spec names $name" || ok "no sandbox spec names $name"
done
sec_names="$($K get swiftsandbox -o jsonpath='{range .items[*]}{.metadata.name}:{range .spec.secretFiles[*]}{.secretName},{end}{range .spec.env[*]}{.valueFrom.secretKeyRef.name},{end}{"\n"}{end}')"
# A compiler may reference only the application's pull Secret (it pulls the
# application as a replica does): never a key or a push credential.
extra="$(grep 'spin-compile-' <<<"$sec_names" | sed 's/^[^:]*://' | tr ',' '\n' | grep -v -x -e '' ${PULL:+-e "$PULL"} || true)"
if [[ -n "$extra" ]]; then fail "a compiler sandbox references Secrets: $(tr '\n' ' ' <<<"$extra")"; else ok "compiler sandboxes reference no Secret${PULL:+ but the application pull Secret $PULL}"; fi
if [[ -n "$PRIV" ]] && grep -qF "$(cat "$PRIV/password")" <<<"$specs$($K get events -o json)"; then
  fail "the registry password appears in a sandbox spec or an Event"
elif [[ -n "$PRIV" ]]; then
  ok "the registry password is in no sandbox spec or Event"
fi

forge "$sec_record" "$unsigned"
refused cc-sec "$sec_record" "$unsigned" "unsigned export"

# Another application's signed artifact: claims name its source.
forge "$sec_record" "$ref_artifact"
until_true "the placement names the new artifact" bash -c "kubectl -n $CTL_NS get spincompiledcache $sec_record -o jsonpath='{.status.placements[0].reason}' | grep -qv VerificationFailed"
refused cc-sec "$sec_record" "$ref_artifact" "another application's signed artifact"

# An artifact outside the namespace's cache repository is never attached.
foreign="$(image_of hello-http)"
forge "$sec_record" "$foreign"
sleep 5
scale cc-sec 2
until_true "cc-sec-1 created" bash -c "$K get swiftsandbox cc-sec-1 >/dev/null 2>&1"
carried="$(jp swiftsandbox/cc-sec-1 '{.spec.artifacts[*].name}')"
[[ -z "$carried" ]] && ok "an artifact outside the cache repository is not attached" || fail "foreign repository artifact attached: $carried"
scale cc-sec 1

if [[ -n "${CC_WRONG_KEY_JOB:-}" ]]; then
  log "signing the unsigned export with a key no policy names"
  signer_image="$(kubectl -n "$CTL_NS" get "$SIGNER" -o jsonpath='{.spec.template.spec.containers[0].image}')"
  cat <<YAML | $K apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: kss-cc-e2e-wrong-key}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: sign
          image: $signer_image
          command: [/usr/local/bin/cosign]
          args: [sign, --yes, --key, /tmp/cosign.key, --signing-config, /etc/kubeswift-spin-signer/signing-config.json, "$unsigned"]
          env:
            - {name: COSIGN_PASSWORD, value: ""}
            - {name: DOCKER_CONFIG, value: /work/docker}
            - {name: HOME, value: /tmp}
${PRIV:+            - {name: SSL_CERT_DIR, value: /work/ca\}}
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
          volumeMounts:
            - {name: tmp, mountPath: /tmp}
            - {name: docker, mountPath: /work/docker, readOnly: true}
${PRIV:+            - {name: ca, mountPath: /work/ca/ca.crt, subPath: ca.crt, readOnly: true\}}
      initContainers:
        - name: key
          image: $signer_image
          command: [/usr/local/bin/cosign, generate-key-pair, --output-key-prefix, /tmp/cosign]
          env: [{name: COSIGN_PASSWORD, value: ""}, {name: HOME, value: /tmp}]
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
          volumeMounts: [{name: tmp, mountPath: /tmp}]
      volumes:
        - {name: tmp, emptyDir: {}}
        - name: docker
          secret: {secretName: $CC_WRONG_KEY_JOB, items: [{key: .dockerconfigjson, path: config.json}]}
${PRIV:+        - {name: ca, secret: {secretName: e2e-registry-files, items: [{key: ca.crt, path: ca.crt\}]\}\}}
YAML
  if $K wait --for=condition=complete job/kss-cc-e2e-wrong-key --timeout=300s >/dev/null 2>&1; then
    forge "$sec_record" "$unsigned"
    refused cc-sec "$sec_record" "$unsigned" "an export signed with a key no policy names"
  else
    fail "the wrong-key signing Job failed: $($K logs job/kss-cc-e2e-wrong-key --all-containers 2>&1 | tail -3)"
  fi
fi

kubectl -n "$CTL_NS" scale "$SIGNER" --replicas="${SIGNER_REPLICAS:-1}" >/dev/null
kubectl -n "$CTL_NS" rollout status "$SIGNER" --timeout=180s >/dev/null
$K delete spinapp cc-sec cc-sec-ref --wait=true >/dev/null
# The forged record is reset by deleting it; the cache manager recreates it
# for a new consumer.
kubectl -n "$CTL_NS" delete spincompiledcache "$sec_record" --wait=true >/dev/null
sec_record=""
fi

if want 3; then
log "phase 3: restarts during a compilation"
# request-info-b's record was deleted at the end of phase 2, so it compiles
# afresh.
app cc-restart request-info-b 1 </dev/null
until_true "cc-restart: compiling" has_reason cc-restart CompiledCache Compiling
r="$(kubectl -n "$CTL_NS" get spincompiledcache -o jsonpath="{range .items[?(@.spec.source==\"$(image_of request-info-b)\")]}{.metadata.name}{end}")"
until_true "a compiler sandbox runs" bash -c "[[ -n \"\$(kubectl -n $CTL_NS get spincompiledcache $r -o jsonpath='{.status.compile.sandbox.name}')\" ]]"
first="$(rec "$r" '{.status.compile.sandbox.uid}')"
kubectl -n "$CTL_NS" delete pod -l app.kubernetes.io/component=cache-manager --wait=false >/dev/null
kubectl -n "$CTL_NS" delete pod -l app.kubernetes.io/component=cache-signer --wait=false >/dev/null
[[ -z "$(rec "$r" '{.status.signature.signedAt}')" ]] && ok "not trusted while compiling" || fail "trusted before the compiler finished"
until_true "cc-restart: Ready after the restarts" has_reason cc-restart CompiledCache Ready
[[ "$(rec "$r" '{.status.compile.sandbox.uid}')" == "$first" && "$(rec "$r" '{.status.compile.attempt}')" == 1 ]] \
  && ok "the restarted cache manager kept the one compiler (attempt 1)" || fail "compile restarted: $(rec "$r" '{.status.compile}')"
n="$($K get swiftsandbox -l spin.kubeswift.io/component=compiler -o name | grep -c . || true)"
[[ "$n" -eq 0 ]] && ok "no compiler sandbox left" || fail "$n compiler sandboxes left"
$K delete spinapp cc-restart --wait=true >/dev/null
fi

if want 5; then
log "phase 5: fallback"
guest() { jp "swiftsandbox/$1" '{.status.conditions[?(@.type=="GuestRunning")].reason}'; }
cond() { jp "swiftsandbox/$1" "{.status.conditions[?(@.type==\"$2\")].status}"; }
uid() { jp "swiftsandbox/$1" '{.metadata.uid}'; }
workload_ready() { [[ "$(cond "$1" WorkloadReady)" == True ]]; }
warm_slots() { [[ "$($K get swiftsandboxpool "$POOL" -o jsonpath='{.status.warmReplicas}')" -eq "$1" ]]; }
min_warm() {
  $K patch swiftsandboxpool "$POOL" --type merge -p "{\"spec\":{\"minWarm\":$1}}" >/dev/null
  until_true "pool $POOL has $1 warm slots" warm_slots "$1"
}
not_held() { [[ "$(reason "$1" CompiledCache)" != DeliveryFailed ]]; }
# skipped_ready checks that KubeSwift skipped replica $1's cache and the
# replica became ready without it, with an accurate status.
skipped_ready() {
  local sb="$1" what="$2"
  until_true "$what: KubeSwift decided the cache" artifact_decided "$sb"
  if artifact_is "$sb" Skipped/; then ok "$what: the cache was skipped ($(artifact_state "$sb"))"; else fail "$what: $(artifact_state "$sb")"; fi
  until_true "$what: the replica became ready without it" workload_ready "$sb"
}
# kept checks that replica $1 is still the same sandbox after the window the
# v0.2.0 breaker waited, and that its SpinApp $2 started no hold.
kept() {
  local sb="$1" app="$2" u
  u="$(uid "$sb")"
  sleep 75
  [[ "$(uid "$sb")" == "$u" ]] && ok "$sb was not replaced" || fail "$sb was replaced"
  not_held "$app" && ok "$app: no DeliveryFailed hold ($(reason "$app" CompiledCache))" || fail "$app: DeliveryFailed after a skipped cache"
}

# A replica that boots cold (no warm slot) mounts the cache its node holds.
app cc-cold hello-http 1 </dev/null
until_true "cc-cold: CompiledCache=True/Ready" has_reason cc-cold CompiledCache Ready
r="$(record_of cc-cold)"
until_true "cc-cold: the cache is prepared on the pool's nodes" pool_prepared "$r"
until_true "cc-cold: Available" ready_replicas cc-cold 1
min_warm 0
scale cc-cold 2
until_true "cc-cold-1: KubeSwift decided the cache" artifact_decided cc-cold-1
until_true "cc-cold-1: ready" workload_ready cc-cold-1
[[ "$(guest cc-cold-1)" != CheckedOut ]] && ok "cc-cold-1 booted cold ($(guest cc-cold-1))" || fail "cc-cold-1 came from a warm slot"
if artifact_is cc-cold-1 Mounted/Cached; then ok "cc-cold-1: a cold replica mounted the cache from its node with fetchTimeout 0s (Mounted/Cached)"
else fail "cc-cold-1: $(artifact_state cc-cold-1)"; fi
[[ "$(cond cc-cold-1 Resolved)" == True ]] && ok "cc-cold-1: Resolved=True" || fail "cc-cold-1: Resolved=$(cond cc-cold-1 Resolved)"
until_true "cc-cold: 2 replicas ready" ready_replicas cc-cold 2
body="$(http "http://$(pod_ip cc-cold-1):3000/hello")"
[[ "$body" == *"Hello from Spin on KubeSwift"* ]] && ok "cc-cold-1 answers" || fail "cc-cold-1: $body"
min_warm 3

# A controller restart leaves running replicas alone.
u0="$(uid cc-cold-0)" u1="$(uid cc-cold-1)"
kubectl -n "$CTL_NS" delete pod -l app.kubernetes.io/name=kubeswift-spin --wait=false >/dev/null
kubectl -n "$CTL_NS" rollout status deploy -l app.kubernetes.io/name=kubeswift-spin --timeout=180s >/dev/null 2>&1 || true
sleep 20
[[ "$(uid cc-cold-0)" == "$u0" && "$(uid cc-cold-1)" == "$u1" ]] && ok "the controller restart replaced no replica" || fail "replicas changed after the controller restart"
until_true "cc-cold: Available after the controller restart" ready_replicas cc-cold 2

# A placement the API server refuses: the SpinApp stays Available. Its
# record is then made to claim a prepared pool (the cache manager stopped,
# as a stale status could), so replicas carry a cache no node holds.
$K create quota kss-cc-e2e-no-placements --hard=count/swiftartifactplacements.sandbox.kubeswift.io=0 >/dev/null
app cc-miss hello-http-b 1 </dev/null
until_true "cc-miss: Available without its cache" ready_replicas cc-miss 1
until_true "cc-miss: CompiledCache=True/PlacementFailed (placement refused)" has_reason cc-miss CompiledCache PlacementFailed
r="$(record_of cc-miss)"
kubectl -n "$CTL_NS" scale "$CM" --replicas=0 >/dev/null
kubectl -n "$CTL_NS" rollout status "$CM" --timeout=120s >/dev/null
kubectl -n "$CTL_NS" patch spincompiledcache "$r" --subresource=status --type merge \
  -p "{\"status\":{\"placements\":[{\"pool\":\"$POOL\",\"name\":\"forged\",\"prepared\":true,\"preparedNodes\":2,\"targetNodes\":2}]}}" >/dev/null
scale cc-miss 2
until_true "cc-miss-1 carries the cache" bash -c "[[ -n \"\$($K get swiftsandbox cc-miss-1 -o jsonpath='{.spec.artifacts[*].ref}')\" ]]"
skipped_ready cc-miss-1 "warm replica, cache not on its node"
[[ "$(guest cc-miss-1)" == CheckedOut ]] && ok "cc-miss-1 came from a warm slot" || fail "cc-miss-1: $(guest cc-miss-1)"
until_true "cc-miss: 2 replicas ready" ready_replicas cc-miss 2
# The first checkout may skip the cache before the claim while KubeSwift
# resolves the new digest (ResolvePending). Once resolved, a checkout claims
# the slot, finds the cache not on the node (NotOnNode) and dispatches the
# workload again without it: the path KubeSwift v0.19.1 left not ready.
scale cc-miss 3
until_true "cc-miss-2 carries the cache" bash -c "[[ -n \"\$($K get swiftsandbox cc-miss-2 -o jsonpath='{.spec.artifacts[*].ref}')\" ]]"
skipped_ready cc-miss-2 "warm replica after the digest resolved, cache not on its node"
artifact_is cc-miss-2 Skipped/NotOnNode && ok "cc-miss-2: Skipped/NotOnNode after the claim" || fail "cc-miss-2: $(artifact_state cc-miss-2), want Skipped/NotOnNode"
[[ "$(guest cc-miss-2)" == CheckedOut ]] && ok "cc-miss-2 came from a warm slot" || fail "cc-miss-2: $(guest cc-miss-2)"
until_true "cc-miss: 3 replicas ready" ready_replicas cc-miss 3
kept cc-miss-1 cc-miss
kept cc-miss-2 cc-miss
min_warm 0
scale cc-miss 4
skipped_ready cc-miss-3 "cold replica, cache not on its node"
[[ "$(guest cc-miss-3)" != CheckedOut ]] && ok "cc-miss-3 booted cold" || fail "cc-miss-3 came from a warm slot"
[[ "$(cond cc-miss-3 Resolved)" == True ]] && ok "cc-miss-3: Resolved=True" || fail "cc-miss-3: Resolved=$(cond cc-miss-3 Resolved)"
until_true "cc-miss: 4 replicas ready" ready_replicas cc-miss 4
min_warm 3

# The cache pull Secret: missing (PullSecretInvalid), then a credential the
# registry refuses. Either way the cache is skipped, never pulled
# anonymously, and the replica becomes ready.
if [[ -n "$CACHE_PULL" && -n "$PRIV" ]]; then
  pull_broken=1
  $K delete secret "$CACHE_PULL" >/dev/null
  scale cc-cold 3
  skipped_ready cc-cold-2 "cache pull Secret missing"
  artifact_is cc-cold-2 Skipped/PullSecretInvalid && ok "cc-cold-2: PullSecretInvalid" || fail "cc-cold-2: $(artifact_state cc-cold-2)"
  $K create secret docker-registry "$CACHE_PULL" --docker-server="$(cat "$PRIV/host")" --docker-username=e2e \
    --docker-password="kss-cc-e2e-wrong-$RUN_ID" >/dev/null
  scale cc-cold 4
  skipped_ready cc-cold-3 "a cache credential the registry refuses"
  until_true "cc-cold: 4 replicas ready" ready_replicas cc-cold 4
  kept cc-cold-3 cc-cold
  $K delete secret "$CACHE_PULL" >/dev/null
  PRIVATE_REGISTRY_DIR="$PRIV" "$ROOT/test/e2e/private-registry.sh" secret "$NS" "$CACHE_PULL"
  pull_broken=""
fi

kubectl -n "$CTL_NS" scale "$CM" --replicas="${CM_REPLICAS:-1}" >/dev/null
kubectl -n "$CTL_NS" rollout status "$CM" --timeout=180s >/dev/null
$K delete resourcequota kss-cc-e2e-no-placements >/dev/null
$K delete spinapp cc-cold cc-miss --wait=true >/dev/null
fi

if want 4; then
log "phase 4: placements go with their consumers"
until_true "no placement of the test pool left" bash -c "[[ -z \"\$($K get swiftartifactplacements -o jsonpath='{range .items[?(@.spec.poolRef.name==\"$POOL\")]}{.metadata.name}{end}')\" ]]"
fi

if [[ $FAILED -ne 0 ]]; then
  echo "compiled-cache e2e FAILED" >&2
  exit 1
fi
log "compiled-cache e2e passed"
