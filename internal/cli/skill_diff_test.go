package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport"
)

func TestSkillDiff_ReportsDroppedFieldsAndWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserSkill(t, home)

	cmd := SkillDiffCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"git-helper", "--from", "claude", "--to", "codex"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "AllowedTools") {
		t.Errorf("expected the fidelity/field-diff report to mention the dropped AllowedTools field, got:\n%s", out)
	}
	if !strings.Contains(out, "preview only") {
		t.Errorf("expected the header to mark this as preview-only, got:\n%s", out)
	}

	// Nothing should have been written to the REAL codex target directory.
	realCodexTarget := filepath.Join(home, ".agents", "skills", "git-helper")
	if _, err := os.Stat(realCodexTarget); !os.IsNotExist(err) {
		t.Fatalf("expected nothing written to %s, stat err = %v", realCodexTarget, err)
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

func TestSkillDiff_NoDifferencesWhenTargetSupportsEverything(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A skill with only fields Cursor also supports (name/description/
	// body) round-trips through Claude->Cursor with no field diff...
	// actually AllowedTools is Claude-only so use a plain skill instead.
	dir := filepath.Join(home, ".claude", "skills", "plain-skill")
	writeSkillFiles(t, dir, map[string][]byte{
		"SKILL.md": []byte("---\nname: plain-skill\ndescription: nothing fancy\n---\n\nbody\n"),
	})

	cmd := SkillDiffCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"plain-skill", "--from", "claude", "--to", "cursor"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	if !strings.Contains(buf.String(), "field diff: no differences") {
		t.Errorf("expected no field differences for a plain skill, got:\n%s", buf.String())
	}
}

func TestSkillDiff_RequiresFromAndTo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := SkillDiffCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"whatever", "--from", "claude"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when --to is missing")
	}
}
