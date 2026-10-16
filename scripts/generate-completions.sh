#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p completions
for completion_shell in bash zsh fish; do
  go run ./cmd/zuano completion "$completion_shell" > "completions/zuano.$completion_shell"
done
