package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport"
)

func TestAgentUninstall_RemovesFileAndManifestEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentPath := writeClaudeUserAgent(t, home)

	// Record a manifest entry as if this agent had been migrated in from
	// somewhere, so uninstall's manifest cleanup has something to drop.
	if err := agentport.RecordEntry(agentport.ManifestEntry{
		Name: "reviewer", Kind: agentport.KindAgent, Provider: agentport.ProviderClaude, Scope: agentport.ScopeUser,
	}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	cmd := AgentUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	if _, err := os.Stat(agentPath); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed, stat err = %v", agentPath, err)
	}

	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("Entries = %#v, want none after uninstall", m.Entries)
	}
}

func TestAgentUninstall_NeverTouchesSkillEntryWithSameName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)

	// A skill entry that happens to share name+provider+scope with the
	// agent must survive an agent uninstall untouched.
	if err := agentport.RecordEntry(agentport.ManifestEntry{
		Name: "reviewer", Kind: agentport.KindSkill, Provider: agentport.ProviderClaude, Scope: agentport.ScopeUser,
	}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	cmd := AgentUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Kind != agentport.KindSkill {
		t.Fatalf("Entries = %#v, want the skill entry to survive untouched", m.Entries)
	}
}

func TestAgentUninstall_RefusesWhenNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"does-not-exist", "--from", "claude", "--yes"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an agent that doesn't exist")
	}
}

func TestAgentUninstall_DoesNotFallBackToOtherProviderDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentPath := writeClaudeUserAgent(t, home)

	cmd := AgentUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "opencode", "--yes"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error: opencode must not resolve a Claude-owned agent")
	}

	if _, err := os.Stat(agentPath); err != nil {
		t.Fatalf("expected %s to still exist (never deleted), stat err = %v", agentPath, err)
	}
}

func TestAgentUninstall_RejectsPathTraversalName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentPath := writeClaudeUserAgent(t, home)

	sentinel := filepath.Join(home, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("do not delete"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	cmd := AgentUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"../../sentinel", "--from", "claude", "--yes"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error rejecting a path-traversal name")
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel file should be untouched: %v", err)
	}
	if _, err := os.Stat(agentPath); err != nil {
		t.Fatalf("unrelated agent should be untouched: %v", err)
	}
}

func TestAgentUninstall_RequiresFrom(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--yes"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when --from is missing")
	}
}

func TestAgentUninstall_UnknownProviderErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "codex", "--yes"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an unsupported --from provider")
	}
}

func TestAgentUninstall_InvalidScopeErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--scope", "bogus", "--yes"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an invalid --scope")
	}
}

// TestAgentUninstall_RemoveFileErrorPropagates confirms a filesystem
// removal failure (containing directory made unwritable) surfaces as an
// error rather than a silent no-op.
func TestAgentUninstall_RemoveFileErrorPropagates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — permission checks don't apply")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)

	agentsDir := filepath.Join(home, ".claude", "agents")
	if err := os.Chmod(agentsDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(agentsDir, 0o755) })

	cmd := AgentUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--yes"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected a remove error when the containing directory is unwritable")
	}
}

// TestAgentUninstall_InteractivePromptDeclines confirms a "no" answer to
// the removal confirmation prompt leaves the file and manifest untouched.
func TestAgentUninstall_InteractivePromptDeclines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentPath := writeClaudeUserAgent(t, home)
	withFakeInteractiveStdin(t, "n\n")

	cmd := AgentUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude"}) // no --yes: exercises the prompt
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "skipped") {
		t.Errorf("expected 'skipped' after declining the prompt, got:\n%s", buf.String())
	}
	if _, err := os.Stat(agentPath); err != nil {
		t.Fatalf("expected %s to remain after declining: %v", agentPath, err)
	}
}

// TestAgentUninstall_InteractivePromptAccepts confirms a "yes" answer to
// the removal confirmation prompt proceeds with the removal.
func TestAgentUninstall_InteractivePromptAccepts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentPath := writeClaudeUserAgent(t, home)
	withFakeInteractiveStdin(t, "y\n")

	cmd := AgentUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude"}) // no --yes: exercises the prompt
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}
	if _, err := os.Stat(agentPath); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed after accepting the prompt, stat err = %v", agentPath, err)
	}
}

// TestAgentUninstall_ManifestUpdateErrorPropagates confirms a manifest
// write failure (the manifest's directory made unwritable) after the file
// has already been removed still surfaces as an error.
func TestAgentUninstall_ManifestUpdateErrorPropagates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — permission checks don't apply")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)

	if err := agentport.RecordEntry(agentport.ManifestEntry{
		Name: "reviewer", Kind: agentport.KindAgent, Provider: agentport.ProviderClaude, Scope: agentport.ScopeUser,
	}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	relayConfigDir := filepath.Join(home, ".config", "relay")
	if err := os.Chmod(relayConfigDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(relayConfigDir, 0o755) })

	cmd := AgentUninstallCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--yes"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected a manifest-update error when ~/.config/relay is unwritable")
	}
}
