#!/usr/bin/env bash
# Enforce security invariants on the rendered Helm chart:
#   - no wildcard RBAC, and no access to Secrets, ConfigMaps or Pods
#   - hardened container and pod security contexts
#   - no ":latest" image references
#   - every rendered SpinAppExecutor is a kubeswift-spin executor
set -euo pipefail
cd "$(dirname "$0")/.."

render() { helm template kubeswift-spin charts/kubeswift-spin --namespace kubeswift-spin-system "$@"; }

check() { python3 hack/check_chart_policy.py; }

render | check
render --set metrics.secure=true --set metrics.serviceMonitor.enabled=true | check
render --set image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 \
       --set runtimeImage.digest=sha256:1111111111111111111111111111111111111111111111111111111111111111 | check
cc=(--set runtimeImage.digest=sha256:1111111111111111111111111111111111111111111111111111111111111111
    --set compiledCache.enabled=true --set compiledCache.repository=registry.example.com/kubeswift-spin-cache
    --set compiledCache.signer.registryCredentialsSecret=cache-registry)
render "${cc[@]}" --set compiledCache.signer.key.secretName=cache-signing-key | check
render "${cc[@]}" --set compiledCache.signer.key.kmsRef=gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k --set metrics.secure=true | check
# The signer needs exactly one key source and registry credentials.
if render "${cc[@]}" >/dev/null 2>&1; then echo "compiledCache rendered without a signing key" >&2; exit 1; fi
if render "${cc[@]}" --set compiledCache.signer.key.secretName=k --set compiledCache.fetchTimeout=10m >/dev/null 2>&1; then
  echo "compiledCache.fetchTimeout above 5m rendered" >&2; exit 1
fi
if render "${cc[@]}" --set compiledCache.signer.key.kmsRef=azurekms://vault/key >/dev/null 2>&1; then echo "the signer rendered with an azurekms key (GO-2026-6225)" >&2; exit 1; fi
if render "${cc[@]}" --set compiledCache.signer.key.secretName=k --set 'compiledCache.signer.extraEnv[0].name=AZURE_CLIENT_SECRET' --set 'compiledCache.signer.extraEnv[0].value=x' >/dev/null 2>&1; then
  echo "the signer rendered with AZURE_* variables (GO-2026-6225)" >&2; exit 1
fi
if render "${cc[@]}" --set compiledCache.signer.key.secretName=k --set compiledCache.signer.key.kmsRef=gcpkms://x >/dev/null 2>&1; then
  echo "compiledCache rendered with two signing keys" >&2; exit 1
fi
# A private CA for the cache registry: mounted into the signer as a file
# (subPath) in SSL_CERT_DIR; nothing else may set the trust store.
ca=(--set compiledCache.signer.key.secretName=k --set-file compiledCache.signer.registryCABundle=hack/testdata/registry-ca.pem)
render "${cc[@]}" "${ca[@]}" | check
signer_ca="$(render "${cc[@]}" "${ca[@]}" | python3 -c '
import sys, yaml
for d in yaml.safe_load_all(sys.stdin):
    if d and d["kind"] == "Deployment" and d["metadata"]["name"].endswith("cache-signer"):
        c = d["spec"]["template"]["spec"]["containers"][0]
        env = {e["name"]: e.get("value") for e in c["env"]}
        m = [v for v in c["volumeMounts"] if v["name"] == "registry-ca"]
        print(env.get("SSL_CERT_DIR"), m[0]["mountPath"], m[0].get("subPath"), m[0].get("readOnly"))')"
if [[ "$signer_ca" != "/var/run/kubeswift-spin-signer/ca /var/run/kubeswift-spin-signer/ca/ca.crt ca.crt True" ]]; then
  echo "the signer's registry CA is not mounted as expected: $signer_ca" >&2; exit 1
fi
if render "${cc[@]}" --set compiledCache.signer.key.secretName=k --set compiledCache.signer.registryCABundle=x >/dev/null 2>&1; then
  echo "compiledCache.signer.registryCABundle rendered without PEM certificates" >&2; exit 1
fi
if render "${cc[@]}" --set compiledCache.signer.key.secretName=k --set-string "compiledCache.signer.registryCABundle=$(cat hack/testdata/registry-ca.pem)
-----BEGIN EC PRIVATE KEY-----
x
-----END EC PRIVATE KEY-----" >/dev/null 2>&1; then
  echo "compiledCache.signer.registryCABundle rendered with a private key" >&2; exit 1
fi
for var in SSL_CERT_FILE SSL_CERT_DIR; do
  if render "${cc[@]}" --set compiledCache.signer.key.secretName=k --set "compiledCache.signer.extraEnv[0].name=$var" --set 'compiledCache.signer.extraEnv[0].value=/x' >/dev/null 2>&1; then
    echo "the signer rendered with $var in extraEnv" >&2; exit 1
  fi
done
# compiledCache.enabled needs a repository and a runtime image digest.
if render --set compiledCache.enabled=true --set compiledCache.repository=registry.example.com/c >/dev/null 2>&1; then
  echo "compiledCache.enabled rendered without runtimeImage.digest" >&2; exit 1
fi
if render --set compiledCache.enabled=true --set runtimeImage.digest=sha256:1111111111111111111111111111111111111111111111111111111111111111 >/dev/null 2>&1; then
  echo "compiledCache.enabled rendered without compiledCache.repository" >&2; exit 1
fi

# The chart's default runtime image must be the version in runtime/VERSION.
want="$(cat runtime/VERSION)"
got="$(python3 -c "import yaml; print(yaml.safe_load(open('charts/kubeswift-spin/values.yaml'))['runtimeImage']['tag'])")"
if [[ "$got" != "$want" ]]; then
  echo "charts/kubeswift-spin/values.yaml runtimeImage.tag is $got, runtime/VERSION is $want" >&2
  exit 1
fi
echo "runtime image version: $want"
