#!/usr/bin/env bash
# Checks the inputs of the OSV-Scanner scans, offline:
#   - the lockfiles passed with --lockfile in the Makefile (OSV_LOCKFILES) and
#     in every workflow that runs OSV-Scanner are exactly the tracked
#     dependency manifests, so a new lockfile cannot go unscanned. The scans
#     use explicit --lockfile flags because a recursive scan follows
#     .gitignore, even for tracked files.
#   - an osv-scanner.toml exists only at the repository root, and every
#     ignored vulnerability in it names one ID with a reason and an
#     ignoreUntil review date; package-wide ignores are not allowed.
# Usage: hack/check-osv-inputs.sh <lockfile>...
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
want="$(printf '%s\n' "$@" | sort)"
tracked="$(git ls-files ':(glob)**/go.mod' ':(glob)**/Cargo.lock' ':(glob)**/package-lock.json' \
  ':(glob)**/yarn.lock' ':(glob)**/pnpm-lock.yaml' ':(glob)**/requirements*.txt' ':(glob)**/poetry.lock' \
  ':(glob)**/Pipfile.lock' ':(glob)**/uv.lock' ':(glob)**/Gemfile.lock' ':(glob)**/composer.lock' \
  ':(glob)**/pom.xml' ':(glob)**/gradle.lockfile' ':(glob)**/packages.lock.json' | sort)"
if [[ "$want" != "$tracked" ]]; then
  echo "OSV_LOCKFILES does not match the tracked dependency manifests:" >&2
  diff <(echo "$want") <(echo "$tracked") >&2 || true
  fail=1
fi

for wf in .github/workflows/osv-scanner-pr.yaml .github/workflows/osv-scanner.yaml .github/workflows/release.yaml; do
  got="$(grep -o -- '--lockfile=[^[:space:]]*' "$wf" | sed 's/^--lockfile=//' | sort)"
  if [[ "$got" != "$want" ]]; then
    echo "$wf: --lockfile arguments do not match OSV_LOCKFILES" >&2
    fail=1
  fi
done

configs="$(git ls-files ':(glob)**/osv-scanner.toml' | grep -vx 'osv-scanner.toml' || true)"
if [[ -n "$configs" ]]; then
  echo "osv-scanner.toml is only allowed at the repository root: $configs" >&2
  fail=1
fi
if [[ -f osv-scanner.toml ]]; then
  python3 - <<'EOF' || fail=1
import sys, tomllib
cfg = tomllib.load(open("osv-scanner.toml", "rb"))
errors = []
for i, v in enumerate(cfg.get("IgnoredVulns", [])):
    if not v.get("id") or not str(v.get("reason", "")).strip() or not v.get("ignoreUntil"):
        errors.append(f"IgnoredVulns[{i}] needs id, reason and ignoreUntil")
for i, p in enumerate(cfg.get("PackageOverrides", [])):
    if p.get("ignore") or (p.get("vulnerability") or {}).get("ignore"):
        errors.append(f"PackageOverrides[{i}] ignores a package; ignore single vulnerability IDs instead")
for e in errors:
    print("osv-scanner.toml: " + e, file=sys.stderr)
sys.exit(1 if errors else 0)
EOF
fi

if [[ $fail -ne 0 ]]; then
  exit 1
fi
echo "osv inputs: $(echo "$want" | wc -l) lockfiles, config policy ok"
