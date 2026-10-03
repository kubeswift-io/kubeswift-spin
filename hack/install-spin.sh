#!/usr/bin/env bash
# Install the pinned Spin CLI into ./bin, verifying the release checksum.
#
# Usage: hack/install-spin.sh [install-dir]
#
# The version and digests match runtime/Dockerfile. Update both together
# (see docs/runtime-image.md, "Updating Spin").
set -euo pipefail

SPIN_VERSION="${SPIN_VERSION:-v4.2.1}"
declare -A SHA256=(
  [linux-amd64]=60cb9f78312acc577a1b839740dc932f2647f438461734bb96a1e374bcf6a3d1
  [linux-aarch64]=199f55510c534cd71aaf589dcd010f19162614696830ddbdb938739c39d71b39
  [macos-amd64]=""
  [macos-aarch64]=""
)

dest="${1:-$(cd "$(dirname "$0")/.." && pwd)/bin}"
mkdir -p "$dest"

if [[ -x "$dest/spin" ]] && "$dest/spin" --version 2>/dev/null | grep -q "^spin ${SPIN_VERSION#v} "; then
  echo "spin ${SPIN_VERSION} already installed in $dest"
  exit 0
fi

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=macos ;;
  *) echo "unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=aarch64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

asset="spin-${SPIN_VERSION}-${os}-${arch}.tar.gz"
base="https://github.com/spinframework/spin/releases/download/${SPIN_VERSION}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/$asset" "$base/$asset"
want="${SHA256[$os-$arch]:-}"
if [[ -z "$want" ]]; then
  # Platforms without a pinned digest fall back to the release checksum file.
  curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/checksums.txt" "$base/checksums-${SPIN_VERSION}.txt"
  want="$(awk -v a="$asset" '$2 == a { print $1 }' "$tmp/checksums.txt")"
fi
if [[ -z "$want" ]]; then
  echo "no checksum for $asset" >&2
  exit 1
fi
if command -v sha256sum >/dev/null; then
  got="$(sha256sum "$tmp/$asset" | awk '{print $1}')"
else
  got="$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')"
fi
if [[ "$got" != "$want" ]]; then
  echo "checksum mismatch for $asset: got $got, want $want" >&2
  exit 1
fi
tar -xzf "$tmp/$asset" -C "$tmp" spin
install -m 0755 "$tmp/spin" "$dest/spin"
"$dest/spin" --version
