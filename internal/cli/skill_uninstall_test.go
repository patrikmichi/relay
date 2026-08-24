package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport"
)

func TestSkillUninstall_RemovesFilesAndManifestEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeSkillDir := writeClaudeUserSkill(t, home)

	// Record a manifest entry as if this skill had been migrated in from
	// somewhere, so uninstall's manifest cleanup has something to drop.
	if err := agentport.RecordEntry(agentport.ManifestEntry{
		Name: "git-helper", Provider: agentport.ProviderClaude, Scope: agentport.ScopeUser,
	}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	cmd := SkillUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"git-helper", "--from", "claude", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	if _, err := os.Stat(claudeSkillDir); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed, stat err = %v", claudeSkillDir, err)
	}

	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("Entries = %#v, want none after uninstall", m.Entries)
	}
}

func TestSkillUninstall_RefusesWhenNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := SkillUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"does-not-exist", "--from", "claude", "--yes"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for a skill that doesn't exist")
	}
}

// TestSkillUninstall_DoesNotFallBackToOtherProviderDir is the regression
// test for BLOCKER 2: `relay skill uninstall foo --from cursor` must not
// resolve into ~/.claude/skills/foo just because cursorAdapter documents it
// as a read-compatibility fallback directory. It must fail with "not found"
// and leave the Claude-owned skill untouched.
func TestSkillUninstall_DoesNotFallBackToOtherProviderDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeSkillDir := writeClaudeUserSkill(t, home)

	cmd := SkillUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"git-helper", "--from", "cursor", "--yes"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error: cursor must not resolve a Claude-owned skill")
	}

	if _, err := os.Stat(claudeSkillDir); err != nil {
		t.Fatalf("expected %s to still exist (never deleted), stat err = %v", claudeSkillDir, err)
	}
}

// TestSkillUninstall_RejectsPathTraversalName is the regression test for
// BLOCKER 1: a name containing path separators must be rejected before any
// filesystem operation, rather than escaping the provider's skills dir.
func TestSkillUninstall_RejectsPathTraversalName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeSkillDir := writeClaudeUserSkill(t, home)

	// A sentinel file outside the skills tree that path traversal could
	// otherwise reach.
	sentinel := filepath.Join(home, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("do not delete"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	cmd := SkillUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"../../sentinel", "--from", "claude", "--yes"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error rejecting a path-traversal name")
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel file should be untouched: %v", err)
	}
	if _, err := os.Stat(claudeSkillDir); err != nil {
		t.Fatalf("unrelated skill should be untouched: %v", err)
	}
}

func TestSkillUninstall_RemovesLegacyFlatCommandFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	commandsDir := filepath.Join(home, ".claude", "commands")
	if err := os.MkdirAll(commandsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	flatFile := filepath.Join(commandsDir, "old-command.md")
	if err := os.WriteFile(flatFile, []byte("---\nname: old-command\ndescription: d\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := SkillUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"old-command", "--from", "claude", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}
	if _, err := os.Stat(flatFile); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed, stat err = %v", flatFile, err)
	}
}
