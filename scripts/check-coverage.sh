#!/usr/bin/env bash
# Checks a Go cover profile against a total statement floor and a
# per-package floor. Prints a per-package table and exits 1 when any floor
# is missed.
#
# Usage: check-coverage.sh PROFILE
# Env:   COVERAGE_TOTAL_MIN   (default 85)
#        COVERAGE_PACKAGE_MIN (default 80)
#
# Excluded from both floors:
#   - cmd/relay/main.go: main() is process wiring (signal context, os.Exit)
#     that only runs inside the built binary; the helpers it calls are
#     tested in cmd/relay and internal/cli.
#   - generated files (a "// Code generated ... DO NOT EDIT." line): their
#     source of truth is the generator, not hand-written code.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 PROFILE" >&2
  exit 2
fi
profile="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
if [ ! -f "$profile" ]; then
  echo "cover profile not found: $1" >&2
  exit 2
fi

total_min="${COVERAGE_TOTAL_MIN:-85}"
package_min="${COVERAGE_PACKAGE_MIN:-80}"

cd "$(git rev-parse --show-toplevel)"
module="$(awk '$1 == "module" { print $2; exit }' go.mod)"
if [ -z "$module" ]; then
  echo "no module line in go.mod" >&2
  exit 2
fi

excluded=("$module/cmd/relay/main.go")
while IFS= read -r file; do
  rel="${file#"$module"/}"
  if [ -f "$rel" ] && grep -qE '^// Code generated .* DO NOT EDIT\.$' "$rel"; then
    excluded+=("$file")
  fi
done < <(awk -F: 'NR > 1 && NF > 1 { print $1 }' "$profile" | sort -u)

printf '%s\n' "${excluded[@]}" | awk \
  -v module="$module" -v total_min="$total_min" -v package_min="$package_min" '
  FNR == NR { skip[$0] = 1; next }
  FNR == 1 { next }
  {
    split($1, loc, ":")
    if (loc[1] in skip) next
    block = $1
    stmts[block] = $2
    if (!(block in hits) || $3 + 0 > hits[block]) hits[block] = $3 + 0
    pkg = loc[1]
    sub(/\/[^\/]*$/, "", pkg)
    pkg_of[block] = pkg
  }
  END {
    for (b in stmts) {
      p = pkg_of[b]
      ptotal[p] += stmts[b]
      all += stmts[b]
      if (hits[b] > 0) { pcovered[p] += stmts[b]; covered += stmts[b] }
    }
    if (all == 0) { print "cover profile has no statements" > "/dev/stderr"; exit 2 }
    failed = 0
    n = 0
    for (p in ptotal) names[++n] = p
    for (i = 2; i <= n; i++) {
      v = names[i]
      for (j = i - 1; j >= 1 && names[j] > v; j--) names[j + 1] = names[j]
      names[j + 1] = v
    }
    for (i = 1; i <= n; i++) {
      p = names[i]
      pct = 100 * pcovered[p] / ptotal[p]
      mark = ""
      if (pct < package_min) { mark = "  below " package_min "%"; failed = 1 }
      label = p
      if (index(label, module "/") == 1) label = substr(label, length(module) + 2)
      if (label == module) label = "."
      printf "%-40s %6.1f%%  (%d/%d)%s\n", label, pct, pcovered[p], ptotal[p], mark
    }
    pct = 100 * covered / all
    mark = ""
    if (pct < total_min) { mark = "  below " total_min "%"; failed = 1 }
    printf "%-40s %6.1f%%  (%d/%d)%s\n", "total", pct, covered, all, mark
    exit failed
  }
' - "$profile"
