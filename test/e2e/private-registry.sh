#!/usr/bin/env bash
# A registry behind a private CA, for the end-to-end tests on a test
# cluster: an in-cluster TLS registry with htpasswd authentication and a CA
# created for the run. It stands in for an organization's private registry
# in test/e2e/kvm-e2e.sh (phase 6) and test/e2e/compiled-cache-e2e.sh.
#
# KubeSwift must trust the CA before sandboxes use the registry: an
# administrator sets KubeSwift's chart value
# sandboxMaterialize.registryCABundle to the CA this script writes, and
# kubeswift-spin's compiledCache.signer.registryCABundle for compiled
# caches. The script changes neither.
#
# Usage:
#   test/e2e/private-registry.sh up
#       Start the registry e2e-registry in E2E_NAMESPACE and write, under
#       PRIVATE_REGISTRY_DIR: ca.crt (the CA certificate), host, user,
#       password and auth.json (a Docker config for the registry; both
#       mode 0600). Creates the docker-registry Secret e2e-registry-auth in
#       E2E_NAMESPACE. The CA key is deleted once the server certificate
#       is signed.
#   test/e2e/private-registry.sh secret <namespace> <name> [<label>=<value>]
#       Create a docker-registry Secret for the registry in a namespace.
#   test/e2e/private-registry.sh copy <source> <repository path>:<tag>
#       Copy an image or artifact into the registry; prints repo@digest
#       (without the tag, as kubeswift-spin records references).
#   test/e2e/private-registry.sh runtime <source> <repository path>:<tag>
#       Copy a runtime image into the registry with the CA appended to its
#       /etc/ssl/certs/ca-certificates.crt (Spin trusts only that bundle);
#       prints repo@digest. linux/amd64.
#   test/e2e/private-registry.sh down
#       Delete everything above, including the Secrets created with
#       "secret", and PRIVATE_REGISTRY_DIR.
#
# Environment:
#   E2E_CONTEXT           kubectl context of the test cluster (required; must
#                         be the current context)
#   E2E_NAMESPACE         namespace of the registry (default kubeswift-spin-e2e)
#   PRIVATE_REGISTRY_DIR  local state directory (default ./private-registry)
#
# copy and runtime run in a tool pod (curl image plus a crane binary built
# here from the go-containerregistry version in go.mod), so nothing is
# pushed from the machine running the script.
set -euo pipefail

NS="${E2E_NAMESPACE:-kubeswift-spin-e2e}"
DIR="${PRIVATE_REGISTRY_DIR:-./private-registry}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
K="kubectl -n $NS"
CURL_IMAGE="curlimages/curl:8.16.0@sha256:463eaf6072688fe96ac64fa623fe73e1dbe25d8ad6c34404a669ad3ce1f104b6"
REGISTRY_IMAGE="registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
LABEL="app.kubernetes.io/created-by=kss-e2e-private-registry"

die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[[ -n "${E2E_CONTEXT:-}" ]] || die "set E2E_CONTEXT to the kubectl context of the test cluster"
[[ "$(kubectl config current-context)" == "$E2E_CONTEXT" ]] || die "the current kubectl context is not E2E_CONTEXT ($E2E_CONTEXT)"

host="e2e-registry.$NS.svc.cluster.local:5000"

tool() { $K exec e2e-registry-tools -- "$@"; }

up() {
  command -v openssl >/dev/null || die "needs openssl"
  python3 -c 'import bcrypt' 2>/dev/null || die "needs the Python bcrypt module"
  [[ ! -e "$DIR" ]] || die "$DIR exists: run down first"
  mkdir -p "$DIR" && chmod 0700 "$DIR"
  local user=e2e pass
  pass="kss-e2e-private-$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj /CN=kubeswift-spin-e2e-private-ca \
    -keyout "$DIR/ca.key" -out "$DIR/ca.crt" 2>/dev/null
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=e2e-registry \
    -keyout "$DIR/tls.key" -out "$DIR/tls.csr" 2>/dev/null
  printf 'subjectAltName=DNS:e2e-registry.%s.svc.cluster.local,DNS:e2e-registry.%s.svc,DNS:e2e-registry\nextendedKeyUsage=serverAuth\n' "$NS" "$NS" >"$DIR/ext"
  openssl x509 -req -in "$DIR/tls.csr" -CA "$DIR/ca.crt" -CAkey "$DIR/ca.key" -CAcreateserial -days 2 \
    -extfile "$DIR/ext" -out "$DIR/tls.crt" 2>/dev/null
  rm -f "$DIR/ca.key" "$DIR/ca.srl" "$DIR/tls.csr" "$DIR/ext"
  (umask 077
    printf '%s' "$user" >"$DIR/user"
    printf '%s' "$pass" >"$DIR/password"
    printf '%s' "$host" >"$DIR/host"
    printf '{"auths":{"%s":{"auth":"%s"}}}' "$host" "$(printf '%s:%s' "$user" "$pass" | base64 -w0)" >"$DIR/auth.json"
    REG_PASS="$pass" python3 -c 'import bcrypt, os, sys; print(sys.argv[1] + ":" + bcrypt.hashpw(os.environ["REG_PASS"].encode(), bcrypt.gensalt()).decode())' "$user" >"$DIR/htpasswd")
  $K create secret generic e2e-registry-files --from-file="$DIR/tls.crt" --from-file="$DIR/tls.key" \
    --from-file="$DIR/ca.crt" --from-file="$DIR/htpasswd" --dry-run=client -o yaml | $K label --local -f - "$LABEL" -o yaml | $K apply -f - >/dev/null
  rm -f "$DIR/tls.key" "$DIR/htpasswd"
  secret "$NS" e2e-registry-auth
  cat <<YAML | $K apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e-registry
  labels: {app: e2e-registry, app.kubernetes.io/created-by: kss-e2e-private-registry}
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
  labels: {app.kubernetes.io/created-by: kss-e2e-private-registry}
spec:
  selector: {app: e2e-registry}
  ports: [{port: 5000, targetPort: 5000}]
---
apiVersion: v1
kind: Pod
metadata:
  name: e2e-registry-tools
  labels: {app.kubernetes.io/created-by: kss-e2e-private-registry}
spec:
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: tools
      image: $CURL_IMAGE
      command: [sleep, "172800"]
      env:
        - {name: HOME, value: /tmp}
        # Go adds the files here to the system bundle.
        - {name: SSL_CERT_DIR, value: /certs}
        - {name: DOCKER_CONFIG, value: /docker}
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
      volumeMounts:
        - {name: tmp, mountPath: /tmp}
        - {name: files, mountPath: /certs/ca.crt, subPath: ca.crt, readOnly: true}
        - {name: auth, mountPath: /docker, readOnly: true}
  volumes:
    - name: tmp
      emptyDir: {}
    - name: files
      secret: {secretName: e2e-registry-files}
    - name: auth
      secret: {secretName: e2e-registry-auth, items: [{key: .dockerconfigjson, path: config.json}]}
YAML
  local version
  version="$(cd "$ROOT" && go list -m -f '{{.Version}}' github.com/google/go-containerregistry)"
  mkdir -p "$DIR/bin"
  GOBIN="$(cd "$DIR/bin" && pwd)" CGO_ENABLED=0 go install "github.com/google/go-containerregistry/cmd/crane@$version"
  $K wait --for=condition=Ready pod/e2e-registry pod/e2e-registry-tools --timeout=300s >/dev/null || die "registry or tool pod not ready"
  $K cp "$DIR/bin/crane" e2e-registry-tools:/tmp/crane >/dev/null
  tool chmod 0755 /tmp/crane
  local code
  code="$(tool curl -s -o /dev/null -w '%{http_code}' --cacert /certs/ca.crt "https://$host/v2/")"
  [[ "$code" == "401" ]] || die "the registry answered HTTP $code to an anonymous request, want 401"
  printf 'registry %s ready (CA %s/ca.crt)\n' "$host" "$DIR"
}

# secret creates a docker-registry Secret for the registry.
secret() {
  local ns="$1" name="$2" label="${3:-}"
  [[ -f "$DIR/auth.json" ]] || die "no $DIR/auth.json: run up first"
  kubectl -n "$ns" create secret generic "$name" --type=kubernetes.io/dockerconfigjson \
    --from-file=.dockerconfigjson="$DIR/auth.json" --dry-run=client -o yaml \
    | kubectl -n "$ns" label --local -f - "$LABEL" ${label:+"$label"} -o yaml | kubectl -n "$ns" apply -f - >/dev/null
}

copy() {
  local src="$1" dst="$host/$2"
  tool /tmp/crane copy --platform linux/amd64 "$src" "$dst" >/dev/null
  printf '%s@%s\n' "${dst%:*}" "$(tool /tmp/crane digest "$dst")"
}

runtime() {
  local src="$1" dst="$host/$2"
  tool sh -c 'set -eu; rm -rf /tmp/rt; mkdir -p /tmp/rt
    /tmp/crane export --platform linux/amd64 "$1" - | tar -x -C /tmp/rt etc/ssl/certs/ca-certificates.crt
    chmod 0644 /tmp/rt/etc/ssl/certs/ca-certificates.crt
    cat /certs/ca.crt >>/tmp/rt/etc/ssl/certs/ca-certificates.crt
    tar -c -C /tmp/rt -f /tmp/layer.tar etc
    /tmp/crane append --platform linux/amd64 -b "$1" -f /tmp/layer.tar -t "$2" >/dev/null
    rm -rf /tmp/rt /tmp/layer.tar' sh "$src" "$dst"
  printf '%s@%s\n' "${dst%:*}" "$(tool /tmp/crane digest "$dst")"
}

down() {
  local ns
  for ns in $(kubectl get secrets -A -l "$LABEL" -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' | sort -u); do
    kubectl -n "$ns" delete secret -l "$LABEL" --ignore-not-found >/dev/null
  done
  $K delete pod,svc -l "$LABEL" --ignore-not-found --wait=false >/dev/null
  rm -rf "$DIR"
}

case "${1:-}" in
  up) up ;;
  secret) shift; [[ $# -ge 2 ]] || die "usage: $0 secret <namespace> <name> [<label>=<value>]"; secret "$@" ;;
  copy) shift; [[ $# -eq 2 && "$2" =~ ^[a-z0-9._/-]+:[A-Za-z0-9._-]+$ ]] || die "usage: $0 copy <source> <repository path>:<tag>"; copy "$@" ;;
  runtime) shift; [[ $# -eq 2 && "$2" =~ ^[a-z0-9._/-]+:[A-Za-z0-9._-]+$ ]] || die "usage: $0 runtime <source> <repository path>:<tag>"; runtime "$@" ;;
  down) down ;;
  *) die "usage: $0 up|secret|copy|runtime|down" ;;
esac
