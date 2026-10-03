#!/usr/bin/env bash
# Verify the pinned Spin release used by the runtime image:
#   1. the archive digests pinned in runtime/Dockerfile match the release's
#      checksums file, and
#   2. the spin binary in each archive carries a valid Sigstore signature
#      from Spin's release workflow for that tag.
#
# Run it when updating Spin. Requires curl, sha256sum and cosign.
set -euo pipefail
cd "$(dirname "$0")/.."

dockerfile=runtime/Dockerfile
version="$(sed -n 's/^ARG SPIN_VERSION=//p' "$dockerfile" | head -1)"
declare -A pinned=(
  [amd64]="$(sed -n 's/^ARG SPIN_SHA256_AMD64=//p' "$dockerfile")"
  [aarch64]="$(sed -n 's/^ARG SPIN_SHA256_ARM64=//p' "$dockerfile")"
)
base="https://github.com/spinframework/spin/releases/download/$version"
identity="https://github.com/spinframework/spin/.github/workflows/release.yml@refs/tags/$version"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL --proto '=https' -o "$tmp/checksums.txt" "$base/checksums-$version.txt"
for arch in amd64 aarch64; do
  asset="spin-$version-static-linux-$arch.tar.gz"
  published="$(awk -v a="$asset" '$2 == a { print $1 }' "$tmp/checksums.txt")"
  [[ "$published" == "${pinned[$arch]}" ]] || { echo "$asset: pinned ${pinned[$arch]}, release lists $published" >&2; exit 1; }
  curl -fsSL --proto '=https' -o "$tmp/$asset" "$base/$asset"
  echo "${pinned[$arch]}  $tmp/$asset" | sha256sum -c - >/dev/null
  mkdir -p "$tmp/$arch"
  tar -xzf "$tmp/$asset" -C "$tmp/$arch" spin spin.sig crt.pem
  cosign verify-blob --signature "$tmp/$arch/spin.sig" --certificate "$tmp/$arch/crt.pem" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity "$identity" "$tmp/$arch/spin" >/dev/null 2>&1 \
    || { echo "$asset: signature verification failed" >&2; exit 1; }
  echo "ok: $asset digest matches the release checksums and the binary is signed by $identity"
done
