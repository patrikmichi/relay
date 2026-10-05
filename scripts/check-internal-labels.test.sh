#!/usr/bin/env bash
# Exercises check-internal-labels.sh against a throwaway git repository.
set -euo pipefail

script="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-internal-labels.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

cd "$work"
git init -q
git config user.email test@example.invalid
git config user.name test

mkdir -p internal docs/releases scripts
cat >internal/clean.go <<'GO'
package internal

// Uses S256 PKCE, sha256 digests, arm64 and x86_64 builds, and the
// RFC 8628 section 3.5 slow_down rule. A Plan previews a migration.
const round = "round trip"
GO
cat >internal/finding.go <<'GO'
package internal

// R12: symlinked roots must load.
func f() {}
GO
cat >internal/tag.go <<'GO'
package internal

func g() {
	// M4: cap response body.
}
GO
cat >internal/paren.go <<'GO'
package internal

// escaped to prevent request hijacking (H3).
GO
cat >config.yml <<'YML'
# out of scope for Phase 1 Part A
key: value
YML
cat >internal/source.go <<'GO'
package internal

// mirrors gateway/lib/db/schema/marketplace.ts
GO
echo 'R01 shipped' >docs/releases/v0.1.0.md
echo 'example.com/mod v1.0.0 h1:R01=' >go.sum
cp "$script" scripts/check-internal-labels.sh
git add -A
echo '// R99 untracked' >internal/untracked.go

set +e
out="$(bash "$script" 2>/dev/null)"
code=$?
set -e

[ "$code" -eq 1 ] || fail "expected exit 1 with violations, got $code"
for want in internal/finding.go:3: internal/tag.go:4: internal/paren.go:3: config.yml:1: internal/source.go:3:; do
  case "$out" in
    *"$want"*) ;;
    *) fail "expected a hit for $want, got: $out" ;;
  esac
done
for unwanted in internal/clean.go docs/releases go.sum internal/untracked.go scripts/check-internal-labels.sh; do
  case "$out" in
    *"$unwanted"*) fail "unexpected hit in $unwanted: $out" ;;
  esac
done

git rm -q --cached internal/finding.go internal/tag.go internal/paren.go config.yml internal/source.go
rm internal/finding.go internal/tag.go internal/paren.go config.yml internal/source.go

set +e
out="$(bash "$script" 2>&1)"
code=$?
set -e

[ "$code" -eq 0 ] || fail "expected exit 0 on a clean tree, got $code: $out"
[ -z "$out" ] || fail "expected no output on a clean tree, got: $out"

echo "ok"
