package cli

// Shared test helpers for the agent-manager CLI commands (migrate, list,
// diff, scan, uninstall, rollback) — the Agent-IR analogue of
// skill_manager_test.go.

import (
	"os"
	"path/filepath"
	"testing"
)

// reviewerClaudeAgentMd is a minimal Claude-format agent fixture (name +
// description + tools + model, so migrations off it have something to
// remap/reshape).
const reviewerClaudeAgentMd = `---
name: reviewer
description: Reviews code for correctness and style.
tools: Read, Bash
model: sonnet
---

Review the diff for correctness and style.
`

// writeClaudeUserAgent writes a minimal valid Claude Code user-scope agent
// named "reviewer" under $HOME/.claude/agents/reviewer.md.
func writeClaudeUserAgent(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "reviewer.md")
	if err := os.WriteFile(path, []byte(reviewerClaudeAgentMd), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// writeOpencodeUserAgent writes a minimal valid opencode user-scope agent
// named "reviewer" under $HOME/.config/opencode/agent/reviewer.md.
func writeOpencodeUserAgent(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".config", "opencode", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "reviewer.md")
	content := "---\ndescription: Reviews code.\nmode: subagent\n---\n\nReview the diff.\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}
