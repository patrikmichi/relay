#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p completions
for completion_shell in bash zsh fish; do
  go run ./cmd/relay completion "$completion_shell" > "completions/relay.$completion_shell"
done
