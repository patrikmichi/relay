#!/usr/bin/env bash
# Fails when a tracked file carries an internal review, milestone, or
# design-doc label (finding IDs, design section refs, gateway source paths)
# instead of plain-language text. Prints file:line:text for every hit.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

patterns=(
  '\bR[0-9]{2}\b'
  '\b(design|plan) (D[0-9]|§)'
  '§'
  '\bP[0-9]\.[0-9]\b'
  '[Ww]ave[ -]?[0-9]'
  '[Pp]hase[ -]?[0-9]'
  '\bD[0-9]\b'
  '\bpre-[A-Z][0-9]\b'
  '(//|#) *[CMNLTAH][0-9]{1,2}( \([^)]*\))?:'
  '\([A-Z][0-9]{1,2}\)'
  '\b[CMNLTAms][0-9]{1,2} (regression|fix)\b'
  '\b(deep|Go|code) review\b'
  'relay-(standalone|cli-completion)'
  'gateway/lib/'
  'app/api/'
  'inert until'
  'evidence/'
  'vault/reviews'
)

args=()
for p in "${patterns[@]}"; do
  args+=(-e "$p")
done

files=()
while IFS= read -r -d '' f; do
  case "$f" in
    docs/releases/* | go.sum | scripts/check-internal-labels.sh | scripts/check-internal-labels.test.sh) continue ;;
  esac
  if [ -f "$f" ]; then
    files+=("$f")
  fi
done < <(git ls-files -z)

if [ "${#files[@]}" -eq 0 ]; then
  exit 0
fi

hits=$(printf '%s\0' "${files[@]}" | xargs -0 grep -nHIE "${args[@]}" -- || true)
if [ -n "$hits" ]; then
  echo "internal labels found (rewrite as plain text):" >&2
  printf '%s\n' "$hits"
  exit 1
fi
exit 0
