package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport"
)

func TestSkillRollback_Last(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeSkillDir := writeClaudeUserSkill(t, home)

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

	cmd := SkillRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--last"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	if _, err := os.Stat(plan.TargetPaths); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed after rollback --last, stat err = %v", plan.TargetPaths, err)
	}
	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("Entries = %#v, want none after rollback", m.Entries)
	}
}

// TestSkillRollback_LastSkipsNewerAgentEntry confirms --last rolls back the
// most recent SKILL entry specifically, even when an agent entry sorts
// later in the shared manifest ledger — the reverse of
// TestAgentRollback_LastSkipsNewerSkillEntry (agent_rollback_test.go).
func TestSkillRollback_LastSkipsNewerAgentEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeSkillDir := writeClaudeUserSkill(t, home)

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

	// A newer agent entry recorded after the skill entry.
	if err := agentport.RecordEntry(agentport.ManifestEntry{
		Name: "unrelated-agent", Kind: agentport.KindAgent, Provider: agentport.ProviderClaude, Scope: agentport.ScopeUser,
	}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	cmd := SkillRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--last"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	if _, err := os.Stat(plan.TargetPaths); !os.IsNotExist(err) {
		t.Fatalf("expected the skill removed, stat err = %v", err)
	}

	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Name != "unrelated-agent" {
		t.Fatalf("Entries = %#v, want only the untouched agent entry to remain", m.Entries)
	}
}

// TestSkillRollback_RejectsAgentKindID confirms an explicit manifest entry
// id that resolves to an AGENT entry is rejected by `relay skill rollback`
// — the reverse of TestAgentRollback_RejectsSkillKindID.
func TestSkillRollback_RejectsAgentKindID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := agentport.RecordEntry(agentport.ManifestEntry{
		Name: "some-agent", Kind: agentport.KindAgent, Provider: agentport.ProviderClaude, Scope: agentport.ScopeUser,
	}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}
	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entry, ok := agentport.LastEntry(m)
	if !ok {
		t.Fatalf("expected a manifest entry")
	}

	cmd := SkillRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{entry.ID})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error rolling back an agent-kind entry via 'relay skill rollback'")
	}
}

// TestSkillRollback_NoSkillEntriesErrors confirms --last errors clearly
// when the manifest has agent entries but no skill entries.
func TestSkillRollback_NoSkillEntriesErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := agentport.RecordEntry(agentport.ManifestEntry{
		Name: "some-agent", Kind: agentport.KindAgent, Provider: agentport.ProviderClaude, Scope: agentport.ScopeUser,
	}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	cmd := SkillRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--last"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when the manifest has no skill entries")
	}
}

func TestSkillRollback_ByID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeSkillDir := writeClaudeUserSkill(t, home)

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

	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entry, ok := agentport.LastEntry(m)
	if !ok {
		t.Fatalf("expected a manifest entry")
	}

	cmd := SkillRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{entry.ID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	if _, err := os.Stat(plan.TargetPaths); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed, stat err = %v", plan.TargetPaths, err)
	}
}

func TestSkillRollback_RequiresExactlyOneSelector(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := SkillRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{}) // neither --last nor an id
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when neither --last nor an id is given")
	}

	cmd2 := SkillRollbackCmd()
	cmd2.SetOut(&buf)
	cmd2.SetArgs([]string{"some-id", "--last"}) // both
	if err := cmd2.Execute(); err == nil {
		t.Fatalf("expected an error when both --last and an id are given")
	}
}

func TestSkillRollback_RefusesModifiedFileWithoutForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeSkillDir := writeClaudeUserSkill(t, home)

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

	// Modify the written SKILL.md after the fact.
	skillMd := filepath.Join(plan.TargetPaths, "SKILL.md")
	if err := os.WriteFile(skillMd, []byte("modified\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := SkillRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--last"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected rollback to refuse a modified file without --force")
	}
	if _, err := os.Stat(skillMd); err != nil {
		t.Fatalf("expected the modified file to remain: %v", err)
	}

	cmd2 := SkillRollbackCmd()
	var buf2 bytes.Buffer
	cmd2.SetOut(&buf2)
	cmd2.SetArgs([]string{"--last", "--force"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("Execute with --force: %v\noutput:\n%s", err, buf2.String())
	}
	if _, err := os.Stat(skillMd); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed with --force, stat err = %v", skillMd, err)
	}
}
