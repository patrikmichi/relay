#!/usr/bin/env bash
# Fails when any given file contains an entry of HOSTNAME_DENYLIST
# (newline- or comma-separated, case-insensitive). The denylist lives only in
# a CI secret; hits are reported by file and entry number so no entry is ever
# printed to a public log.
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: scan-denylist.sh <file>..." >&2
  exit 2
fi

entries=()
while IFS= read -r entry; do
  entry="${entry#"${entry%%[![:space:]]*}"}"
  entry="${entry%"${entry##*[![:space:]]}"}"
  if [ -n "$entry" ]; then
    entries+=("$entry")
  fi
done < <(printf '%s\n' "${HOSTNAME_DENYLIST:-}" | tr ',' '\n')

if [ "${#entries[@]}" -eq 0 ]; then
  if [ "${REQUIRE_HOSTNAME_DENYLIST:-0}" = "1" ]; then
    echo "error: HOSTNAME_DENYLIST is empty; set the HOSTNAME_DENYLIST repository secret" >&2
    exit 1
  fi
  echo "warning: HOSTNAME_DENYLIST is empty; skipping denylist scan" >&2
  exit 0
fi

if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
  for entry in "${entries[@]}"; do
    echo "::add-mask::$entry"
  done
fi

patterns=$(printf '%s\n' "${entries[@]}")
status=0
for file in "$@"; do
  if [ ! -f "$file" ]; then
    echo "error: $file is not a file" >&2
    status=1
    continue
  fi
  grep -aqiF -e "$patterns" -- "$file" || continue
  status=1
  for i in "${!entries[@]}"; do
    if grep -aqiF -e "${entries[$i]}" -- "$file"; then
      echo "error: $file contains denylisted hostname #$((i + 1))" >&2
    fi
  done
done
exit "$status"
