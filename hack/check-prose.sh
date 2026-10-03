#!/usr/bin/env bash
# Fail if project-authored files contain an em dash (U+2014) or a section
# sign (U+00A7). Third-party files are excluded. Emoji are not checked here:
# a reliable emoji detector is not worth a build dependency.
set -euo pipefail
cd "$(dirname "$0")/.."

mapfile -t files < <(git ls-files --cached --others --exclude-standard \
  | grep -Ev '^(LICENSE|go\.sum|examples/Cargo\.lock)$' \
  | grep -Ev '\.(png|jpg|jpeg|gif|ico|wasm|tgz)$')

status=0
for f in "${files[@]}"; do
  [[ -f "$f" ]] || continue
  if grep -nP '\x{2014}|\x{00A7}' "$f" >/dev/null 2>&1; then
    grep -nP '\x{2014}|\x{00A7}' "$f" | sed "s|^|$f:|" >&2
    status=1
  fi
done
if [[ $status -ne 0 ]]; then
  echo "em dash or section sign found; use commas, colons, parentheses or hyphens instead" >&2
fi
exit $status
