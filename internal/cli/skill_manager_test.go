package cli

// Shared test helpers for the Wave 2 agentport manager CLI commands
// (install, list, diff, scan, score, uninstall, rollback).

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSkillFiles materializes a files map under dir, creating parent
// directories as needed — the CLI-package equivalent of agentport's
// unexported writeFiles test helper.
func writeSkillFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for rel, data := range files {
		dest := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", dest, err)
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", dest, err)
		}
	}
}

// gitHelperSkillMd is a minimal Claude-format SKILL.md fixture (name +
// description + allowed-tools, so migrations off it have something to
// drop when the target doesn't support allowed-tools).
const gitHelperSkillMd = `---
name: git-helper
description: Summarizes uncommitted git changes.
allowed-tools:
  - Bash(git diff *)
---

Summarize the diff.
`

// writeClaudeUserSkill writes a minimal valid Claude Code user-scope skill
// named "git-helper" under $HOME/.claude/skills/git-helper.
func writeClaudeUserSkill(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "skills", "git-helper")
	writeSkillFiles(t, dir, map[string][]byte{"SKILL.md": []byte(gitHelperSkillMd)})
	return dir
}
