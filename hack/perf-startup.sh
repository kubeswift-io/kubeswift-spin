#!/usr/bin/env bash
# Startup benchmark on a KubeSwift cluster with KVM. Runs startupbench as a
# Job on one node, first cold and then from a warm pool, and summarizes the
# results. See test/perf/startupbench/README.md.
#
# The benchmark pod and every sandbox run on PERF_NODE, so every timestamp
# comes from that node's clock. It creates, in PERF_NAMESPACE: a
# ServiceAccount, Role and RoleBinding named startupbench (no Secret access),
# a ConfigMap with the SpinApp manifest, the SpinAppExecutors
# startupbench-cold and startupbench-warm, the SwiftSandboxPool
# startupbench-warm, and one Job per mode. It deletes them afterwards unless
# PERF_KEEP=1.
#
# Environment:
#   PERF_NODE           node for the benchmark pod and the sandboxes (required)
#   PERF_IMAGE          startupbench image the node can pull, by digest
#                       (name@sha256:...); or
#   PERF_REGISTRY       registry to build and push startupbench to (for
#                       example ttl.sh) when PERF_IMAGE is not set
#   PERF_RUNTIME_IMAGE  runtime image for the warm pool (default: the
#                       controller's --runtime-image argument)
#   PERF_NAMESPACE      namespace with a Ready SwiftKernel "sandbox"
#                       (default kubeswift-spin-e2e)
#   PERF_RUNS           runs per mode (default 10)
#   PERF_MODES          modes, in order (default "cold warm")
#   PERF_MANIFEST       SpinApp manifest (default examples/hello-http/spinapp.yaml)
#   PERF_APP_IMAGE      override spec.image, for example with a digest
#   PERF_PATH           request path (default /hello)
#   PERF_EXPECT         substring the response must contain (default the
#                       hello-http greeting)
#   PERF_POOL_CPU       warm pool vCPUs; must match the manifest (default 1)
#   PERF_POOL_MEMORY    warm pool memory; must match the manifest (default 256Mi)
#   PERF_BASELINE       directory of earlier results (its *.jsonl files) to
#                       compare p50 and p95 with
#   PERF_OUT            output directory (default bin/perf/<UTC time>)
#   PERF_LAUNCHER_LOGS=1  also record KubeSwift launcher stages from the pod
#                       logs (grants the benchmark pods/log in the namespace)
#   PERF_KEEP=1         keep the objects afterwards
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

NODE="${PERF_NODE:-}"
NS="${PERF_NAMESPACE:-kubeswift-spin-e2e}"
RUNS="${PERF_RUNS:-10}"
MODES="${PERF_MODES:-cold warm}"
MANIFEST="${PERF_MANIFEST:-examples/hello-http/spinapp.yaml}"
APP_IMAGE="${PERF_APP_IMAGE:-}"
REQ_PATH="${PERF_PATH:-/hello}"
EXPECT="${PERF_EXPECT:-Hello from Spin on KubeSwift}"
POOL_CPU="${PERF_POOL_CPU:-1}"
POOL_MEMORY="${PERF_POOL_MEMORY:-256Mi}"
OUT="${PERF_OUT:-bin/perf/$(date -u +%Y%m%dT%H%M%SZ)}"
K=(kubectl -n "$NS")

die() { printf 'perf-startup: %s\n' "$*" >&2; exit 1; }
log() { printf '==> %s\n' "$*"; }
# sq quotes a value for a single-quoted YAML scalar.
sq() { local v="$1"; printf "'%s'" "${v//\'/\'\'}"; }
# sqarg quotes a container argument: as sq, and $ doubled so that the
# kubelet does not expand $(VAR) references.
sqarg() { local v="$1"; sq "${v//\$/\$\$}"; }

[[ -n "$NODE" ]] || die "set PERF_NODE to the node that should run the benchmark and the sandboxes"
[[ "$NODE" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || die "PERF_NODE is not a valid node name"
[[ "$NS" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "PERF_NAMESPACE is not a valid namespace name"
[[ "$RUNS" =~ ^[1-9][0-9]*$ ]] || die "PERF_RUNS must be a positive number"
[[ "$POOL_CPU" =~ ^[1-9][0-9]*$ ]] || die "PERF_POOL_CPU must be a whole number of vCPUs"
[[ "$POOL_MEMORY" =~ ^[1-9][0-9]*(Mi|Gi)$ ]] || die "PERF_POOL_MEMORY must be like 256Mi or 1Gi"
[[ "$REQ_PATH" == /* ]] || die "PERF_PATH must start with /"
[[ -f "$MANIFEST" ]] || die "$MANIFEST does not exist"
for m in $MODES; do [[ "$m" == cold || "$m" == warm ]] || die "unknown mode $m in PERF_MODES"; done
kubectl get node "$NODE" >/dev/null || die "node $NODE not found"
kubectl get namespace "$NS" >/dev/null || die "namespace $NS not found"

# The controller's runtime image, from the only kubeswift-spin Deployment.
runtime_image() {
  local n
  n="$(kubectl get deploy -A -l app.kubernetes.io/name=kubeswift-spin -o name | grep -c . || true)"
  [[ "$n" == 1 ]] || die "found $n kubeswift-spin Deployments; set PERF_RUNTIME_IMAGE"
  kubectl get deploy -A -l app.kubernetes.io/name=kubeswift-spin -o jsonpath='{.items[0].spec.template.spec.containers[0].args}' \
    | tr ',' '\n' | sed -n 's/.*--runtime-image=\([^"]*\).*/\1/p'
}
RUNTIME_IMAGE="${PERF_RUNTIME_IMAGE:-$(runtime_image)}"
[[ -n "$RUNTIME_IMAGE" ]] || die "cannot find the kubeswift-spin controller's --runtime-image argument; set PERF_RUNTIME_IMAGE"

if [[ -z "${PERF_IMAGE:-}" ]]; then
  [[ -n "${PERF_REGISTRY:-}" ]] || die "set PERF_IMAGE, or PERF_REGISTRY to build and push startupbench"
  tag="$PERF_REGISTRY/kubeswift-spin-startupbench:$(git rev-parse --short=12 HEAD)"
  log "building $tag"
  docker build -q -f test/perf/startupbench/Dockerfile -t "$tag" . >/dev/null
  docker push -q "$tag" >/dev/null
  PERF_IMAGE="$(docker inspect --format '{{index .RepoDigests 0}}' "$tag")"
fi
[[ "$PERF_IMAGE" == *@sha256:* ]] || die "PERF_IMAGE must be pinned by digest (name@sha256:...)"

mkdir -p "$OUT"
cleanup() {
  [[ "${PERF_KEEP:-}" == "1" ]] && return 0
  "${K[@]}" delete job -l app.kubernetes.io/name=startupbench --ignore-not-found --wait=true >/dev/null 2>&1 || true
  "${K[@]}" delete spinapp -l app.kubernetes.io/created-by=startupbench --ignore-not-found --wait=true >/dev/null 2>&1 || true
  "${K[@]}" delete spinappexecutor startupbench-cold startupbench-warm --ignore-not-found >/dev/null 2>&1 || true
  "${K[@]}" delete swiftsandboxpool startupbench-warm --ignore-not-found >/dev/null 2>&1 || true
  "${K[@]}" delete configmap startupbench-manifest --ignore-not-found >/dev/null 2>&1 || true
  "${K[@]}" delete rolebinding,role,serviceaccount startupbench --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "recording the environment in $OUT/environment.txt"
{
  echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit: $(git rev-parse HEAD)$(git diff --quiet HEAD -- || echo ' (modified)')"
  echo "node: $NODE"
  kubectl get node "$NODE" -o jsonpath='kubelet: {.status.nodeInfo.kubeletVersion}{"\n"}kernel: {.status.nodeInfo.kernelVersion}{"\n"}os: {.status.nodeInfo.osImage}{"\n"}runtime: {.status.nodeInfo.containerRuntimeVersion}{"\n"}cpu: {.status.capacity.cpu}{"\n"}memory: {.status.capacity.memory}{"\n"}'
  echo "controller images:"
  kubectl get deploy -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name} {.spec.template.spec.containers[0].image}{"\n"}{end}' \
    | grep -E ' (ghcr\.io/kubeswift-io/kubeswift-spin[@:]|ghcr\.io/kubeswift-io/kubeswift/controller-manager:|[^ ]*/spin-operator:)' | sed 's/^/  /'
  echo "runtime image: $RUNTIME_IMAGE"
  echo "startupbench image: $PERF_IMAGE"
  echo "manifest: $MANIFEST (sha256 $(sha256sum "$MANIFEST" | cut -c1-64))"
  echo "app image override: ${APP_IMAGE:-none}"
  echo "runs per mode: $RUNS; modes: $MODES; pool: ${POOL_CPU} vCPU, ${POOL_MEMORY}"
  echo "launcher logs: ${PERF_LAUNCHER_LOGS:-0}"
} >"$OUT/environment.txt"

log "creating the benchmark ServiceAccount, Role, executors and pool in $NS"
log_rule=""
if [[ "${PERF_LAUNCHER_LOGS:-}" == "1" ]]; then
  log_rule='  - {apiGroups: [""], resources: [pods/log], verbs: [get]}'
fi
"${K[@]}" create configmap startupbench-manifest --from-file=spinapp.yaml="$MANIFEST" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
cat <<YAML | "${K[@]}" apply -f - >/dev/null
apiVersion: v1
kind: ServiceAccount
metadata:
  name: startupbench
  labels: {app.kubernetes.io/name: startupbench}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: startupbench
  labels: {app.kubernetes.io/name: startupbench}
rules:
  - {apiGroups: [core.spinkube.dev], resources: [spinapps], verbs: [list, watch, create]}
  - {apiGroups: [core.spinkube.dev], resources: [spinapps], resourceNames: [startupbench-cold, startupbench-warm], verbs: [get, delete]}
  - {apiGroups: [sandbox.kubeswift.io], resources: [swiftsandboxes], verbs: [get, list, watch]}
  - {apiGroups: [""], resources: [pods, services], verbs: [get, list, watch]}
  - {apiGroups: [discovery.k8s.io], resources: [endpointslices], verbs: [list, watch]}
  - {apiGroups: [events.k8s.io], resources: [events], verbs: [list, watch]}
$log_rule
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: startupbench
  labels: {app.kubernetes.io/name: startupbench}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: startupbench}
subjects:
  - {kind: ServiceAccount, name: startupbench, namespace: $(sq "$NS")}
---
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinAppExecutor
metadata:
  name: startupbench-cold
  labels: {spin.kubeswift.io/managed-by: kubeswift-spin}
  annotations:
    spin.kubeswift.io/node-selector: $(sq "kubernetes.io/hostname=$NODE")
spec:
  createDeployment: false
---
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinAppExecutor
metadata:
  name: startupbench-warm
  labels: {spin.kubeswift.io/managed-by: kubeswift-spin}
  annotations:
    spin.kubeswift.io/node-selector: $(sq "kubernetes.io/hostname=$NODE")
    spin.kubeswift.io/sandbox-pool: startupbench-warm
spec:
  createDeployment: false
YAML


# One slot, so the slot startupbench waits for is the one KubeSwift hands
# out. Created just before the warm runs, so that its boot does not overlap
# the cold runs.
create_pool() {
  cat <<YAML | "${K[@]}" apply -f - >/dev/null
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata:
  name: startupbench-warm
spec:
  image: $(sq "$RUNTIME_IMAGE")
  cpu: $POOL_CPU
  memory: $POOL_MEMORY
  nodeSelector: {kubernetes.io/hostname: $(sq "$NODE")}
  network:
    mode: restricted
    ports: [{name: http-app, port: 3000}]
  minWarm: 1
  maxWarm: 1
YAML
}

for mode in $MODES; do
  log "$mode: $RUNS runs on $NODE"
  [[ "$mode" == warm ]] && create_pool
  args=(run -n "$NS" -f /manifest/spinapp.yaml -mode "$mode" -name "startupbench-$mode" -executor "startupbench-$mode"
    -runs "$RUNS" -path "$REQ_PATH" -expect "$EXPECT")
  [[ -n "$APP_IMAGE" ]] && args+=(-image "$APP_IMAGE")
  [[ "$mode" == warm ]] && args+=(-warm-pool startupbench-warm -start-jitter 2s)
  [[ "${PERF_LAUNCHER_LOGS:-}" == "1" ]] && args+=(-launcher-logs)
  yaml_args=""
  for a in "${args[@]}"; do yaml_args+="            - $(sqarg "$a")"$'\n'; done
  "${K[@]}" delete job "startupbench-$mode" --ignore-not-found --wait=true >/dev/null
  cat <<YAML | "${K[@]}" apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata:
  name: startupbench-$mode
  labels: {app.kubernetes.io/name: startupbench}
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 3600
  template:
    metadata:
      labels: {app.kubernetes.io/name: startupbench}
    spec:
      restartPolicy: Never
      serviceAccountName: startupbench
      nodeSelector: {kubernetes.io/hostname: $(sq "$NODE")}
      securityContext:
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: startupbench
          image: $(sq "$PERF_IMAGE")
          args:
$yaml_args          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
          resources:
            requests: {cpu: 100m, memory: 64Mi}
            limits: {memory: 256Mi}
          volumeMounts: [{name: manifest, mountPath: /manifest, readOnly: true}]
      volumes: [{name: manifest, configMap: {name: startupbench-manifest}}]
YAML
  # kubectl logs fails while the container is being created.
  for _ in $(seq 300); do
    phase="$("${K[@]}" get pods -l job-name="startupbench-$mode" -o jsonpath='{.items[0].status.phase}' 2>/dev/null || true)"
    [[ -n "$phase" && "$phase" != Pending ]] && break
    sleep 2
  done
  # Progress goes to standard error and results (JSON lines) to standard
  # output; the pod log has both.
  "${K[@]}" logs -f "job/startupbench-$mode" | tee "$OUT/$mode.log" | grep -v '^{' || true
  grep '^{' "$OUT/$mode.log" >"$OUT/$mode.jsonl" || die "the $mode Job produced no results; see $OUT/$mode.log"
  "${K[@]}" wait --for=condition=Complete --timeout=60s "job/startupbench-$mode" >/dev/null \
    || die "the $mode Job did not complete; see $OUT/$mode.log"
done

log "summary ($OUT/summary.txt, $OUT/summary.csv)"
summary_args=(-csv "$OUT/summary.csv")
if [[ -n "${PERF_BASELINE:-}" ]]; then
  for f in "$PERF_BASELINE"/*.jsonl; do
    [[ -f "$f" ]] && summary_args+=(-baseline "$f")
  done
fi
jsonl=()
for mode in $MODES; do jsonl+=("$OUT/$mode.jsonl"); done
go run ./test/perf/startupbench summarize "${summary_args[@]}" "${jsonl[@]}" | tee "$OUT/summary.txt"
