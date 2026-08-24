package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport"
)

func TestSkillList_EnumeratesInstalledSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserSkill(t, home)

	cmd := SkillListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--provider", "claude"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "git-helper") {
		t.Errorf("expected output to mention git-helper, got:\n%s", out)
	}
	if !strings.Contains(out, "claude") {
		t.Errorf("expected output to mention the claude provider, got:\n%s", out)
	}
}

func TestSkillList_NoneFoundIsGraceful(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := SkillListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--provider", "claude"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute should not error when nothing is found: %v", err)
	}
	if !strings.Contains(buf.String(), "no skills found") {
		t.Errorf("expected a graceful 'no skills found' message, got:\n%s", buf.String())
	}
}

func TestSkillList_UnknownProviderErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := SkillListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--provider", "not-a-real-provider"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an unknown --provider")
	}
}

func TestSkillList_ProvenanceJoinsManifest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeSkillDir := writeClaudeUserSkill(t, home)

	// Migrate the fixture to codex so a manifest entry exists to join.
	src, err := agentport.NewClaudeAdapter().Load(claudeSkillDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := agentport.Migrate(src, agentport.NewCodexAdapter(), agentport.ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := agentport.Write(plan); err != nil {
		t.Fatalf("Write: %v", err)
	}

	cmd := SkillListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--provider", "codex", "--provenance"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "git-helper") {
		t.Fatalf("expected output to mention git-helper, got:\n%s", out)
	}
	if !strings.Contains(out, "from claude") {
		t.Errorf("expected provenance to show the claude source, got:\n%s", out)
	}
}

func TestSkillList_ProvenanceNoManifestRecordIsGraceful(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserSkill(t, home) // present on disk, but never recorded in the manifest

	cmd := SkillListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--provider", "claude", "--provenance"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), "no manifest record") {
		t.Errorf("expected 'no manifest record' for an unrecorded skill, got:\n%s", buf.String())
	}
}
