#!/usr/bin/env bash
# Build and test every example Spin application locally with `spin up`.
#
# Variables are passed the way kubeswift-spin passes them to a sandbox:
# SPIN_VARIABLE_* environment variables. External services are replaced by
# examples/tools/upstream, so no test depends on the internet.
#
# Usage: hack/test-examples.sh [example ...]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SPIN="${SPIN:-$ROOT/bin/spin}"
EX="$ROOT/examples"
TMP="$(mktemp -d)"
PIDS=()
FAILED=0

cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

log() { printf '==> %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAILED=1; }

wait_http() {
  local url="$1"
  for _ in $(seq 1 100); do
    curl -fsS -o /dev/null "$url" 2>/dev/null && return 0
    sleep 0.2
  done
  return 1
}

# start_app <manifest> <port> [env assignments...] [-- extra spin args]
start_app() {
  local manifest="$1" port="$2"
  shift 2
  local envs=() extra=()
  while [[ $# -gt 0 ]]; do
    if [[ "$1" == "--" ]]; then shift; extra=("$@"); break; fi
    envs+=("$1"); shift
  done
  env "${envs[@]}" "$SPIN" up -f "$manifest" --listen "127.0.0.1:$port" "${extra[@]}" \
    >"$TMP/app-$port.log" 2>&1 &
  PIDS+=("$!")
  if ! wait_http "http://127.0.0.1:$port/healthz"; then
    cat "$TMP/app-$port.log" >&2
    fail "$manifest did not become healthy on port $port"
    return 1
  fi
}

stop_apps() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  PIDS=()
  start_upstream
}

expect_eq() {
  local what="$1" got="$2" want="$3"
  if [[ "$got" != "$want" ]]; then fail "$what: got [$got], want [$want]"; else log "ok: $what"; fi
}

expect_contains() {
  local what="$1" got="$2" want="$3"
  if [[ "$got" != *"$want"* ]]; then fail "$what: [$got] does not contain [$want]"; else log "ok: $what"; fi
}

expect_absent() {
  local what="$1" got="$2" banned="$3"
  if [[ "$got" == *"$banned"* ]]; then fail "$what: response leaks [$banned]"; else log "ok: $what"; fi
}

start_upstream() {
  "$TMP/upstream" --listen 127.0.0.1:8090 >"$TMP/upstream.log" 2>&1 &
  PIDS+=("$!")
  wait_http "http://127.0.0.1:8090/" || { cat "$TMP/upstream.log" >&2; fail "upstream did not start"; exit 1; }
}

test_hello_http() {
  start_app "$EX/hello-http/spin.toml" 3101
  expect_eq "hello-http /hello" "$(curl -fsS http://127.0.0.1:3101/hello)" "Hello from Spin on KubeSwift"
  expect_eq "hello-http /" "$(curl -fsS http://127.0.0.1:3101/)" "Hello from Spin on KubeSwift"
  expect_eq "hello-http /healthz" "$(curl -fsS http://127.0.0.1:3101/healthz)" "ok"
  expect_eq "hello-http unknown path" "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:3101/nope)" "404"
}

test_request_info() {
  start_app "$EX/request-info/spin.toml" 3102 SPIN_VARIABLE_APP_VERSION=1.2.3 SPIN_VARIABLE_GREETING=hi
  local body
  body="$(curl -fsS 'http://127.0.0.1:3102/info?x=1' \
    -H 'Authorization: Bearer do-not-echo' -H 'Cookie: session=do-not-echo' \
    -H 'X-Request-Id: req-42' -H 'User-Agent: example-test')"
  expect_contains "request-info path" "$body" '"path":"/info"'
  expect_contains "request-info query" "$body" '"query":"x=1"'
  expect_contains "request-info allowlisted header" "$body" '["x-request-id","req-42"]'
  expect_contains "request-info variable from environment" "$body" '"app_version":"1.2.3"'
  expect_contains "request-info second variable" "$body" '"greeting":"hi"'
  expect_absent "request-info authorization" "$body" "do-not-echo"
  expect_absent "request-info cookie header name" "$body" "cookie"
}

test_outbound_http() {
  start_app "$EX/outbound-http/spin.toml" 3103
  expect_contains "outbound-http allowed target" "$(curl -sS http://127.0.0.1:3103/fetch)" '"ok":true'

  # Nothing listens on 8091: the request is allowed by Spin but fails.
  start_app "$EX/outbound-http/spin.toml" 3104 \
    SPIN_VARIABLE_TARGET_URL=http://127.0.0.1:8091/ SPIN_VARIABLE_TARGET_ORIGIN=http://127.0.0.1:8091
  expect_eq "outbound-http unreachable target status" "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:3104/fetch)" "502"

  # The target is reachable but not in allowed_outbound_hosts: Spin refuses.
  start_app "$EX/outbound-http/spin.toml" 3105 SPIN_VARIABLE_TARGET_ORIGIN=http://127.0.0.1:9999
  local body
  body="$(curl -sS http://127.0.0.1:3105/fetch)"
  expect_contains "outbound-http denied by Spin allowlist" "$body" '"ok":false'
}

test_key_value() {
  # A fresh state directory per run keeps the test repeatable.
  start_app "$EX/key-value/spin.toml" 3106 -- --state-dir "$TMP/kv-state"
  expect_eq "key-value put" "$(curl -s -o /dev/null -w '%{http_code}' -X PUT --data-binary 'v1' http://127.0.0.1:3106/kv/greeting)" "204"
  expect_eq "key-value get" "$(curl -fsS http://127.0.0.1:3106/kv/greeting)" "v1"
  expect_eq "key-value list" "$(curl -fsS http://127.0.0.1:3106/kv)" '["greeting"]'
  expect_eq "key-value invalid key" "$(curl -s -o /dev/null -w '%{http_code}' 'http://127.0.0.1:3106/kv/bad%20key')" "400"
  expect_eq "key-value delete" "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE http://127.0.0.1:3106/kv/greeting)" "204"
  expect_eq "key-value get deleted" "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:3106/kv/greeting)" "404"
}

test_serverless_ai() {
  start_app "$EX/serverless-ai/spin.toml" 3107 -- --runtime-config-file "$EX/serverless-ai/runtime-config.toml"
  local body
  body="$(curl -sS -X POST --data-binary 'hello from the test' http://127.0.0.1:3107/ask)"
  expect_contains "serverless-ai completion" "$body" 'mock completion from model \"default-model\" for a 4-word prompt'
  expect_contains "serverless-ai usage" "$body" '"prompt_tokens":4'
  expect_eq "serverless-ai empty prompt" "$(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:3107/ask)" "400"
}

test_mcp() {
  start_app "$EX/experimental/mcp/spin.toml" 3108
  local url=http://127.0.0.1:3108/mcp body
  body="$(curl -fsS "$url" -H 'content-type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}')"
  expect_contains "mcp initialize" "$body" '"protocolVersion":"2025-06-18"'
  expect_eq "mcp initialized notification" \
    "$(curl -s -o /dev/null -w '%{http_code}' "$url" -H 'content-type: application/json' -d '{"jsonrpc":"2.0","method":"notifications/initialized"}')" "202"
  expect_contains "mcp tools/list" "$(curl -fsS "$url" -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')" '"name":"word_count"'
  expect_contains "mcp word_count" \
    "$(curl -fsS "$url" -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"word_count","arguments":{"text":"a b  c"}}}')" '"text":"3"'
  expect_contains "mcp sha256" \
    "$(curl -fsS "$url" -d '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"sha256","arguments":{"text":"abc"}}}')" \
    'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad'
  expect_contains "mcp unknown method" "$(curl -fsS "$url" -d '{"jsonrpc":"2.0","id":5,"method":"resources/list"}')" '"code":-32601'
  expect_eq "mcp foreign origin" "$(curl -s -o /dev/null -w '%{http_code}' "$url" -H 'Origin: https://evil.example' -d '{"jsonrpc":"2.0","id":6,"method":"ping"}')" "403"
  expect_eq "mcp GET" "$(curl -s -o /dev/null -w '%{http_code}' "$url")" "405"
}

main() {
  [[ -x "$SPIN" ]] || { echo "spin not found at $SPIN; run make spin" >&2; exit 1; }
  log "building examples"
  (cd "$EX" && cargo build --quiet --target wasm32-wasip2 --release)
  (cd "$ROOT" && go build -o "$TMP/upstream" ./examples/tools/upstream)
  start_upstream

  local all=(hello_http request_info outbound_http key_value serverless_ai mcp)
  local selected=("$@")
  [[ ${#selected[@]} -eq 0 ]] && selected=("${all[@]}")
  for t in "${selected[@]}"; do
    t="${t//-/_}"
    log "example: $t"
    "test_$t" || true
    stop_apps
  done

  if [[ $FAILED -ne 0 ]]; then
    echo "example tests FAILED" >&2
    exit 1
  fi
  log "all example tests passed"
}

main "$@"
