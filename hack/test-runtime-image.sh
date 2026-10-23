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
AUTH_REG_PORT="${AUTH_REG_PORT:-5556}"
TLS_REG_PORT="${TLS_REG_PORT:-5557}"
TMP="$(mktemp -d)"
CONTAINERS=()
FAILED=0

cleanup() {
  for c in "${CONTAINERS[@]}"; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  [[ -n "${UPSTREAM_PID:-}" ]] && kill "$UPSTREAM_PID" 2>/dev/null || true
  # Compile mode writes its output as root.
  docker run --rm -v "$TMP:/t" --entrypoint /bin/sh "$REGISTRY_IMAGE" -c 'rm -rf /t/out-*' >/dev/null 2>&1 || true
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
  for e in "${envs[@]}"; do
    if [[ "$e" == MOUNT:* ]]; then
      local spec="${e#MOUNT:}"
      eflags+=(-v "${spec}:ro")
    else
      eflags+=(-e "$e")
    fi
  done
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
# UPSTREAM_PORT moves the fixture off 8090 when another process holds it.
UPSTREAM_PORT="${UPSTREAM_PORT:-8090}"
[[ "$UPSTREAM_PORT" =~ ^[0-9]{1,5}$ ]] || { echo "UPSTREAM_PORT must be a port number" >&2; exit 1; }
"$TMP/upstream" --listen "127.0.0.1:$UPSTREAM_PORT" >"$TMP/upstream.log" 2>&1 &
UPSTREAM_PID=$!
wait_http "http://127.0.0.1:$UPSTREAM_PORT/" || fail "upstream did not start"
kill -0 "$UPSTREAM_PID" 2>/dev/null || fail "upstream exited; is 127.0.0.1:$UPSTREAM_PORT in use? Set UPSTREAM_PORT"
llm_config="$(printf '[llm_compute]\ntype = "remote_http"\nurl = "http://127.0.0.1:%s"\napi_type = "open_ai"\nauth_token = ""\n' "$UPSTREAM_PORT")"
run_sandbox kss-test-ai 3204 "localhost:$REG_PORT/serverless-ai:test" \
  --runtime-config-file=/var/lib/kubeswift-spin/runtime-config.toml \
  -- "KUBESWIFT_SPIN_RUNTIME_CONFIG_B64=$(printf '%s' "$llm_config" | base64 -w0)" SPIN_VARIABLE_LLM_MODEL=default-model
body="$(curl -sS -X POST --data-binary 'two words' http://127.0.0.1:3204/ask)"
[[ "$body" == *'mock completion from model \"default-model\"'* ]] && ok "LLM runtime config" || fail "LLM runtime config: $body"

# 7. A Secret-backed runtime-config option: the rendered config carries a
# placeholder and KubeSwift delivers the value as an environment variable.
"$TMP/upstream" --listen 127.0.0.1:8091 --require-token 's3cr"et' >"$TMP/upstream-token.log" 2>&1 &
TOKEN_PID=$!
wait_http http://127.0.0.1:8091/ || fail "token upstream did not start"
secret_config=$'[llm_compute]\ntype = "remote_http"\nurl = "http://127.0.0.1:8091"\napi_type = "open_ai"\nauth_token = "kubeswift-spin-secret:KUBESWIFT_SPIN_SECRET_0"\n'
run_sandbox kss-test-ai-secret 3205 "localhost:$REG_PORT/serverless-ai:test" \
  --runtime-config-file=/var/lib/kubeswift-spin/runtime-config.toml \
  -- "KUBESWIFT_SPIN_RUNTIME_CONFIG_B64=$(printf '%s' "$secret_config" | base64 -w0)" 'KUBESWIFT_SPIN_SECRET_0=s3cr"et' SPIN_VARIABLE_LLM_MODEL=default-model
body="$(curl -sS -X POST --data-binary 'two words' http://127.0.0.1:3205/ask)"
[[ "$body" == *'mock completion'* ]] && ok "Secret-backed runtime-config option substituted" || fail "secret substitution: $body"
run_sandbox kss-test-ai-wrong 3206 "localhost:$REG_PORT/serverless-ai:test" \
  --runtime-config-file=/var/lib/kubeswift-spin/runtime-config.toml \
  -- "KUBESWIFT_SPIN_RUNTIME_CONFIG_B64=$(printf '%s' "$secret_config" | base64 -w0)" 'KUBESWIFT_SPIN_SECRET_0=wrong' SPIN_VARIABLE_LLM_MODEL=default-model
[[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST --data-binary 'x' http://127.0.0.1:3206/ask)" == "502" ]] \
  && ok "a wrong Secret value is rejected by the endpoint" || fail "wrong token accepted"
kill "$TOKEN_PID" 2>/dev/null || true

# 8. A runtime config delivered as a Secret file (loadFromSecret): root-owned
# and mode 0400, as KubeSwift writes it; the Spin user gets its own copy.
printf '[key_value_store.default]\ntype = "spin"\npath = "/var/lib/kubeswift-spin/state/kv.db"\n' >"$TMP/rc.toml"
chmod 0400 "$TMP/rc.toml"
run_sandbox kss-test-kv-file 3207 "localhost:$REG_PORT/key-value:test" \
  --runtime-config-file=/var/lib/kubeswift-spin/runtime-config.toml \
  -- KUBESWIFT_SPIN_RUNTIME_CONFIG_FILE=/run/kubeswift-spin/runtime-config.toml \
  "MOUNT:$TMP/rc.toml:/run/kubeswift-spin/runtime-config.toml"
curl -fsS -X PUT --data-binary v3 http://127.0.0.1:3207/kv/k >/dev/null
[[ "$(curl -fsS http://127.0.0.1:3207/kv/k)" == "v3" ]] && ok "runtime config from a Secret file" || fail "runtime config file"

# 9. A private application registry: credentials from imagePullSecrets
# arrive as Docker config secret files and are merged for the Spin user.
python3 -c 'import bcrypt; print("ci:" + bcrypt.hashpw(b"ci-pass", bcrypt.gensalt()).decode())' >"$TMP/htpasswd"
docker run -d --name kss-test-registry-auth -p "127.0.0.1:$AUTH_REG_PORT:5000" \
  -v "$TMP/htpasswd:/auth/htpasswd:ro" -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=test \
  -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd "$REGISTRY_IMAGE" >/dev/null
CONTAINERS+=(kss-test-registry-auth)
for _ in $(seq 1 40); do curl -s -o /dev/null "http://127.0.0.1:$AUTH_REG_PORT/v2/" && break; sleep 0.25; done
mkdir -p "$TMP/docker"
printf '{"auths":{"localhost:%s":{"auth":"%s"}}}' "$AUTH_REG_PORT" "$(printf 'ci:ci-pass' | base64 -w0)" >"$TMP/docker/config.json"
(cd "$ROOT/examples/hello-http" && XDG_CONFIG_HOME="$TMP/spin-config" DOCKER_CONFIG="$TMP/docker" \
  "$SPIN" registry push --insecure "localhost:$AUTH_REG_PORT/hello-http:private" >/dev/null) \
  && ok "pushed to the authenticated registry" || fail "push to authenticated registry"
cp "$TMP/docker/config.json" "$TMP/auth.json"
chmod 0400 "$TMP/auth.json"
run_sandbox kss-test-private 3208 "localhost:$AUTH_REG_PORT/hello-http:private" \
  -- KUBESWIFT_SPIN_REGISTRY_AUTH_FILES=/run/kubeswift-spin/registry-auth/0.json \
  "MOUNT:$TMP/auth.json:/run/kubeswift-spin/registry-auth/0.json"
[[ "$(curl -fsS http://127.0.0.1:3208/hello)" == "Hello from Spin on KubeSwift" ]] && ok "application pulled from a private registry" || fail "private pull"
if docker run --rm --network host --user 0 --entrypoint /usr/local/bin/kubeswift-spin-entrypoint "$IMAGE" \
  up "--from=localhost:$AUTH_REG_PORT/hello-http:private" --insecure --listen=127.0.0.1:3209 >"$TMP/noauth.log" 2>&1; then
  fail "private application pulled without credentials"
else
  ok "private application is not pulled without credentials"
fi

# 10. A contract violation fails fast with a clear message.
if docker run --rm --user 0 --entrypoint /usr/local/bin/kubeswift-spin-entrypoint \
  -e KUBESWIFT_SPIN_RUNTIME_CONFIG_B64=eA== "$IMAGE" up --from=localhost/x:1 >"$TMP/out" 2>&1; then
  fail "entrypoint accepted runtime config without the flag"
elif grep -q "runtime-config-file" "$TMP/out"; then
  ok "entrypoint rejects a runtime config without the flag"
else
  fail "unexpected entrypoint error: $(cat "$TMP/out")"
fi

# 11. Compile mode (ADR 0014). It accepts no --insecure, so the application
# comes from a TLS registry whose throwaway CA is added to the image's bundle.
mkdir -p "$TMP/tls" "$TMP/multi"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$TMP/tls/ca.key" -out "$TMP/tls/ca.crt" \
  -days 1 -subj /CN=kss-test-ca 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$TMP/tls/srv.key" -out "$TMP/tls/srv.csr" -subj /CN=localhost 2>/dev/null
printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\n' >"$TMP/tls/ext"
openssl x509 -req -in "$TMP/tls/srv.csr" -CA "$TMP/tls/ca.crt" -CAkey "$TMP/tls/ca.key" -CAcreateserial \
  -out "$TMP/tls/srv.crt" -days 1 -extfile "$TMP/tls/ext" 2>/dev/null
cat /etc/ssl/certs/ca-certificates.crt "$TMP/tls/ca.crt" >"$TMP/tls/bundle.crt"
chmod 0644 "$TMP/tls/"*
docker run -d --name kss-test-registry-tls -p "127.0.0.1:$TLS_REG_PORT:5000" -v "$TMP/tls:/certs:ro" \
  -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/srv.crt -e REGISTRY_HTTP_TLS_KEY=/certs/srv.key "$REGISTRY_IMAGE" >/dev/null
CONTAINERS+=(kss-test-registry-tls)
for _ in $(seq 1 40); do curl -s --cacert "$TMP/tls/ca.crt" -o /dev/null "https://localhost:$TLS_REG_PORT/v2/" && break; sleep 0.25; done
# Three HTTP components (two identical), a required variable used in an
# outbound host template, a Secret-like required variable and a key-value
# label that needs runtime configuration.
W="$ROOT/examples/target/wasm32-wasip2/release"
cat >"$TMP/multi/spin.toml" <<EOF
spin_manifest_version = 2
[application]
name = "kss-multi"
version = "0.1.0"
[variables]
api_host = { required = true }
token = { required = true, secret = true }
[[trigger.http]]
route = "/hello/..."
component = "hello"
[[trigger.http]]
route = "/kv/..."
component = "kv"
[[trigger.http]]
route = "/info/..."
component = "info"
[component.hello]
source = "$W/hello_http.wasm"
[component.kv]
source = "$W/key_value.wasm"
key_value_stores = ["default", "cache"]
allowed_outbound_hosts = ["https://{{ api_host }}"]
[component.kv.variables]
token = "{{ token }}"
[component.info]
source = "$W/hello_http.wasm"
EOF
push_tls() { SSL_CERT_FILE="$TMP/tls/bundle.crt" "$SPIN" registry push "$@" 2>/dev/null | sed -n 's/.*digest \(sha256:[0-9a-f]*\).*/\1/p'; }
multi_digest="$(cd "$TMP/multi" && push_tls "localhost:$TLS_REG_PORT/kss-multi:test")"
hello_digest="$(cd "$ROOT/examples/hello-http" && push_tls "localhost:$TLS_REG_PORT/hello-http:test")"
RT_REF="ghcr.io/kubeswift-io/kubeswift-spin-runtime@sha256:$(printf '0%.0s' $(seq 64))"
ARCH="$(docker image inspect "$IMAGE" --format '{{.Architecture}}')"
# compile <name> <source> [extra docker args]
compile() {
  local name="$1" src="$2"
  shift 2
  mkdir -p "$TMP/out-$name"
  chmod 0777 "$TMP/out-$name"
  docker run --rm --network host --user 0 -v "$TMP/tls/bundle.crt:/etc/ssl/certs/ca-certificates.crt:ro" \
    -v "$TMP/out-$name:/var/lib/kubeswift-spin-output" "$@" --entrypoint /usr/local/bin/kubeswift-spin-entrypoint "$IMAGE" \
    compile "--source=$src" "--runtime=$RT_REF" "--architecture=$ARCH" >"$TMP/compile-$name.log" 2>&1
}
if compile multi "localhost:$TLS_REG_PORT/kss-multi@$multi_digest"; then
  meta="$TMP/out-multi/metadata.json"
  comps="$(python3 -c 'import json,sys; m=json.load(open(sys.argv[1])); print(",".join(m["components"]), len(m["entries"]), m["source"].endswith(sys.argv[2]))' "$meta" "$multi_digest")"
  [[ "$comps" == "hello,info,kv 2 True" ]] && ok "compile mode: 3 components, 2 entries (identical components share one)" || fail "compile metadata: $comps"
  files="$(cd "$TMP/out-multi" && find . -type f | grep -v -E '^\./metadata\.json$|^\./cache/modules/wasmtime-[0-9.]+/[A-Za-z0-9_-]{43}$' || true)"
  [[ -z "$files" ]] && ok "compile mode: only metadata and cache entries" || fail "unexpected output files: $files"
  grep -q "2 placeholder variables" "$TMP/compile-multi.log" && ok "compile mode: placeholders for required variables" || fail "no placeholders"
else
  cat "$TMP/compile-multi.log" >&2
  fail "compile mode failed for a multi-component application"
fi
# The compiled entry is the entry a normal start of the same application
# writes: same key, so a replica can use it.
if compile hello "localhost:$TLS_REG_PORT/hello-http@$hello_digest"; then
  docker run -d --name kss-test-tls-hello --network host --user 0 -v "$TMP/tls/bundle.crt:/etc/ssl/certs/ca-certificates.crt:ro" \
    --entrypoint /usr/local/bin/kubeswift-spin-entrypoint "$IMAGE" \
    up "--from=localhost:$TLS_REG_PORT/hello-http@$hello_digest" --listen=127.0.0.1:3210 >/dev/null
  CONTAINERS+=(kss-test-tls-hello)
  wait_http http://127.0.0.1:3210/hello || fail "hello-http from the TLS registry"
  docker cp kss-test-tls-hello:/var/lib/kubeswift-spin/home/.cache/wasmtime "$TMP/runtime-cache" >/dev/null
  want="$(ls "$TMP/out-hello/cache/modules/"*/ | sort)"
  got="$(find "$TMP/runtime-cache/modules" -mindepth 2 -maxdepth 2 -type f -printf '%f\n' | grep -E '^[A-Za-z0-9_-]{43}$' | sort)"
  [[ -n "$want" && "$want" == "$got" ]] && ok "compile mode writes the key a normal start uses" || fail "compiled [$want], runtime [$got]"
else
  cat "$TMP/compile-hello.log" >&2
  fail "compile mode failed for hello-http"
fi
# Refusals: a tag, the wrong architecture, a non-empty output.
compile tag "localhost:$TLS_REG_PORT/hello-http:test" && fail "compile accepted a tag" || ok "compile mode refuses a tag"
other=amd64; [[ "$ARCH" == amd64 ]] && other=arm64
if docker run --rm --user 0 -v "$TMP/out-hello:/var/lib/kubeswift-spin-output" --entrypoint /usr/local/bin/kubeswift-spin-entrypoint "$IMAGE" \
  compile "--source=localhost:$TLS_REG_PORT/hello-http@$hello_digest" "--runtime=$RT_REF" "--architecture=$other" >/dev/null 2>&1; then
  fail "compile accepted another architecture"
else
  ok "compile mode refuses another architecture"
fi
compile hello "localhost:$TLS_REG_PORT/hello-http@$hello_digest" && fail "compile wrote into a non-empty output" || ok "compile mode refuses a non-empty output"

if [[ $FAILED -ne 0 ]]; then
  echo "runtime image tests FAILED" >&2
  exit 1
fi
log "all runtime image tests passed"
