package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport"
)

func TestAgentDiff_ReportsDroppedFieldsAndWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)

	cmd := AgentDiffCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--to", "opencode"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "preview only") {
		t.Errorf("expected the header to mark this as preview-only, got:\n%s", out)
	}
	if !strings.Contains(out, "Tools") {
		t.Errorf("expected the field diff to mention the reshaped Tools field, got:\n%s", out)
	}

	// Nothing should have been written to the REAL opencode target directory.
	realTarget := filepath.Join(home, ".config", "opencode", "agent", "reviewer.md")
	if _, err := os.Stat(realTarget); !os.IsNotExist(err) {
		t.Fatalf("expected nothing written to %s, stat err = %v", realTarget, err)
	}

	// No manifest entry should have been recorded either.
	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("Entries = %#v, want none — diff must never record a manifest entry", m.Entries)
	}
}

func TestAgentDiff_NoDifferencesForIdenticalRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A plain agent with only fields both claude and opencode support
	// identically (description + body; no tools/model/memory) round-trips
	// with no field diff.
	if err := os.WriteFile(filepath.Join(dir, "plain-agent.md"), []byte("---\nname: plain-agent\ndescription: nothing fancy\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := AgentDiffCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"plain-agent", "--from", "claude", "--to", "opencode"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	if !strings.Contains(buf.String(), "field diff: no differences") {
		t.Errorf("expected no field differences for a plain agent, got:\n%s", buf.String())
	}
}

func TestAgentDiff_RequiresFromAndTo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentDiffCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"whatever", "--from", "claude"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when --to is missing")
	}
}

func TestAgentDiff_UnknownProvidersError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)

	cmd := AgentDiffCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "codex", "--to", "opencode"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an unsupported --from provider")
	}

	cmd2 := AgentDiffCmd()
	var buf2 bytes.Buffer
	cmd2.SetOut(&buf2)
	cmd2.SetArgs([]string{"reviewer", "--from", "claude", "--to", "cursor"})
	if err := cmd2.Execute(); err == nil {
		t.Fatalf("expected an error for an unsupported --to provider")
	}
}

func TestAgentDiff_NotFoundErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentDiffCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"does-not-exist", "--from", "claude", "--to", "opencode"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when the named agent doesn't exist")
	}
}

func TestAgentDiff_InvalidScopeErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentDiffCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--to", "opencode", "--scope", "bogus"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an invalid --scope")
	}
}

// TestAgentDiff_LoadErrorPropagates confirms a malformed agent file (found
// by ResolveAgentPath, but failing Load's frontmatter parse) surfaces as an
// error rather than panicking.
func TestAgentDiff_LoadErrorPropagates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Missing the closing "---" frontmatter delimiter.
	if err := os.WriteFile(filepath.Join(dir, "broken.md"), []byte("---\nname: broken\ndescription: d\n\nbody with no closing delimiter\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := AgentDiffCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"broken", "--from", "claude", "--to", "opencode"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error loading a malformed agent file")
	}
}
