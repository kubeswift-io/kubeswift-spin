#!/usr/bin/env bash
# Test the kubeswift-spin runtime image with Docker, without Kubernetes or
# KVM. The container is started as root with the same command, arguments and
# environment the controller puts in a SwiftSandbox spec, which is what the
# KubeSwift guest agent runs.
#
# This exercises the image and the entrypoint contract. It does not exercise
# KubeSwift: there is no microVM, no sandbox networking and no rootfs
# materialization. See test/e2e for the KVM path.
#
# Usage: hack/test-runtime-image.sh <runtime-image>
set -euo pipefail

IMAGE="${1:?usage: $0 <runtime-image>}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SPIN="${SPIN:-$ROOT/bin/spin}"
SPIN_VERSION="${SPIN_VERSION:-4.2.1}"
REGISTRY_IMAGE="registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
REG_PORT="${REG_PORT:-5555}"
TMP="$(mktemp -d)"
CONTAINERS=()
FAILED=0

cleanup() {
  for c in "${CONTAINERS[@]}"; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  [[ -n "${UPSTREAM_PID:-}" ]] && kill "$UPSTREAM_PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

log() { printf '==> %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAILED=1; }
ok() { printf '==> ok: %s\n' "$*"; }

wait_http() {
  for _ in $(seq 1 120); do
    curl -fsS -o /dev/null "$1" 2>/dev/null && return 0
    sleep 0.25
  done
  return 1
}

# run_sandbox <name> <port> <app-ref> [extra args] -- [env...]
# Mirrors the SwiftSandbox the controller builds: entrypoint + "up" args.
run_sandbox() {
  local name="$1" port="$2" ref="$3"
  shift 3
  local args=() envs=()
  while [[ $# -gt 0 ]]; do
    if [[ "$1" == "--" ]]; then shift; envs=("$@"); break; fi
    args+=("$1"); shift
  done
  local eflags=()
  for e in "${envs[@]}"; do eflags+=(-e "$e"); done
  docker run -d --name "$name" --network host --user 0 "${eflags[@]}" \
    --entrypoint /usr/local/bin/kubeswift-spin-entrypoint "$IMAGE" \
    up "--from=$ref" --insecure "--listen=127.0.0.1:$port" "${args[@]}" >/dev/null
  CONTAINERS+=("$name")
  if ! wait_http "http://127.0.0.1:$port/healthz"; then
    docker logs "$name" >&2 || true
    fail "$name did not become healthy"
    return 1
  fi
}

log "image: $IMAGE"

# 1. Static checks on the image filesystem and metadata.
cid="$(docker create "$IMAGE")"
CONTAINERS+=("$cid")
docker export "$cid" | tar -t >"$TMP/files"
for f in usr/local/bin/spin usr/local/bin/kubeswift-spin-entrypoint etc/ssl/certs/ca-certificates.crt etc/passwd usr/share/doc/spin/LICENSE; do
  grep -qx "$f" "$TMP/files" && ok "image contains /$f" || fail "image is missing /$f"
done
for f in bin/sh bin/bash usr/bin/apt usr/bin/apt-get usr/bin/dpkg sbin/apk usr/bin/curl usr/bin/wget; do
  grep -qx "$f" "$TMP/files" && fail "image contains /$f" || ok "image has no /$f"
done
grep -qx 'var/lib/kubeswift-spin/' "$TMP/files" && ok "image has /var/lib/kubeswift-spin" || fail "image lacks /var/lib/kubeswift-spin"
user="$(docker image inspect "$IMAGE" --format '{{.Config.User}}')"
[[ "$user" == "65532:65532" ]] && ok "image user is 65532:65532" || fail "image user is [$user]"
if docker image inspect "$IMAGE" --format '{{json .Config.Env}}' | grep -Eiq 'token|password|secret|credential'; then
  fail "image environment contains credential-like variables"
else
  ok "image environment has no credential-like variables"
fi
if docker history --no-trunc "$IMAGE" | grep -Eiq 'GITHUB_TOKEN|password=|secret='; then
  fail "image history references credentials"
else
  ok "image history has no credentials"
fi

# 2. Version.
ver="$(docker run --rm "$IMAGE")"
[[ "$ver" == "spin $SPIN_VERSION "* ]] && ok "spin version: $ver" || fail "unexpected version [$ver]"
docker run --rm --entrypoint /usr/local/bin/kubeswift-spin-entrypoint "$IMAGE" build >/dev/null 2>&1 \
  && fail "entrypoint accepted a command other than up" || ok "entrypoint rejects commands other than up"

# 3. Application artifacts from a local registry.
docker run -d --name kss-test-registry -p "127.0.0.1:$REG_PORT:5000" "$REGISTRY_IMAGE" >/dev/null
CONTAINERS+=(kss-test-registry)
wait_http "http://127.0.0.1:$REG_PORT/v2/" || { fail "registry did not start"; exit 1; }
(cd "$ROOT/examples" && cargo build --locked --quiet --target wasm32-wasip2 --release)
for app in hello-http key-value serverless-ai; do
  (cd "$ROOT/examples/$app" && "$SPIN" registry push --insecure "localhost:$REG_PORT/$app:test" >/dev/null)
done
ok "pushed example artifacts"

# 4. hello-http, started as root like the KubeSwift guest agent does.
run_sandbox kss-test-hello 3201 "localhost:$REG_PORT/hello-http:test"
[[ "$(curl -fsS http://127.0.0.1:3201/hello)" == "Hello from Spin on KubeSwift" ]] && ok "hello-http responds" || fail "hello-http response"
uids="$(docker top kss-test-hello -o uid,pid | tail -n +2 | awk '{print $1}' | sort -u | tr '\n' ' ')"
[[ "$uids" == "65532 " ]] && ok "spin runs as uid 65532 after dropping root" || fail "processes run as uids [$uids]"
start=$(date +%s%N)
docker stop -t 10 kss-test-hello >/dev/null
elapsed_ms=$(( ($(date +%s%N) - start) / 1000000 ))
code="$(docker inspect kss-test-hello --format '{{.State.ExitCode}}')"
if [[ "$code" == "0" && $elapsed_ms -lt 5000 ]]; then ok "SIGTERM: exit 0 after ${elapsed_ms}ms"; else fail "SIGTERM: exit $code after ${elapsed_ms}ms"; fi

# 5. Runtime configuration delivered the way the controller delivers it.
kv_config=$'[key_value_store.default]\ntype = "spin"\npath = "/var/lib/kubeswift-spin/state/kv.db"\n'
run_sandbox kss-test-kv 3202 "localhost:$REG_PORT/key-value:test" \
  --runtime-config-file=/var/lib/kubeswift-spin/runtime-config.toml \
  -- "KUBESWIFT_SPIN_RUNTIME_CONFIG_B64=$(printf '%s' "$kv_config" | base64 -w0)"
curl -fsS -X PUT --data-binary v1 http://127.0.0.1:3202/kv/k >/dev/null
[[ "$(curl -fsS http://127.0.0.1:3202/kv/k)" == "v1" ]] && ok "key-value store from runtime config" || fail "key-value runtime config"

# The default store without runtime config is in memory.
run_sandbox kss-test-kv-mem 3203 "localhost:$REG_PORT/key-value:test"
curl -fsS -X PUT --data-binary v2 http://127.0.0.1:3203/kv/k >/dev/null
[[ "$(curl -fsS http://127.0.0.1:3203/kv/k)" == "v2" ]] && ok "default key-value store without runtime config" || fail "default key-value store"

# 6. LLM runtime config against the local OpenAI-compatible fixture.
(cd "$ROOT" && go build -o "$TMP/upstream" ./examples/tools/upstream)
"$TMP/upstream" --listen 127.0.0.1:8090 >"$TMP/upstream.log" 2>&1 &
UPSTREAM_PID=$!
wait_http http://127.0.0.1:8090/ || fail "upstream did not start"
llm_config=$'[llm_compute]\ntype = "remote_http"\nurl = "http://127.0.0.1:8090"\napi_type = "open_ai"\nauth_token = ""\n'
run_sandbox kss-test-ai 3204 "localhost:$REG_PORT/serverless-ai:test" \
  --runtime-config-file=/var/lib/kubeswift-spin/runtime-config.toml \
  -- "KUBESWIFT_SPIN_RUNTIME_CONFIG_B64=$(printf '%s' "$llm_config" | base64 -w0)" SPIN_VARIABLE_LLM_MODEL=default-model
body="$(curl -sS -X POST --data-binary 'two words' http://127.0.0.1:3204/ask)"
[[ "$body" == *'mock completion from model \"default-model\"'* ]] && ok "LLM runtime config" || fail "LLM runtime config: $body"

# 7. A contract violation fails fast with a clear message.
if docker run --rm --user 0 --entrypoint /usr/local/bin/kubeswift-spin-entrypoint \
  -e KUBESWIFT_SPIN_RUNTIME_CONFIG_B64=eA== "$IMAGE" up --from=localhost/x:1 >"$TMP/out" 2>&1; then
  fail "entrypoint accepted runtime config without the flag"
elif grep -q "runtime-config-file" "$TMP/out"; then
  ok "entrypoint rejects a runtime config without the flag"
else
  fail "unexpected entrypoint error: $(cat "$TMP/out")"
fi

if [[ $FAILED -ne 0 ]]; then
  echo "runtime image tests FAILED" >&2
  exit 1
fi
log "all runtime image tests passed"
