package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport"
)

func TestAgentRollback_Last(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudePath := writeClaudeUserAgent(t, home)

	src, err := agentport.NewClaudeAgentAdapter().Load(claudePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := agentport.MigrateAgent(src, agentport.NewOpencodeAgentAdapter(), agentport.ScopeUser)
	if err != nil {
		t.Fatalf("MigrateAgent: %v", err)
	}
	if err := agentport.WriteAgent(plan); err != nil {
		t.Fatalf("WriteAgent: %v", err)
	}

	cmd := AgentRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--last"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	opencodePath := filepath.Join(plan.TargetPaths, "reviewer.md")
	if _, err := os.Stat(opencodePath); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed after rollback --last, stat err = %v", opencodePath, err)
	}
	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("Entries = %#v, want none after rollback", m.Entries)
	}
}

// TestAgentRollback_LastSkipsNewerSkillEntry confirms --last rolls back the
// most recent AGENT entry specifically, even when a skill entry sorts
// later in the shared manifest ledger.
func TestAgentRollback_LastSkipsNewerSkillEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudePath := writeClaudeUserAgent(t, home)

	src, err := agentport.NewClaudeAgentAdapter().Load(claudePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := agentport.MigrateAgent(src, agentport.NewOpencodeAgentAdapter(), agentport.ScopeUser)
	if err != nil {
		t.Fatalf("MigrateAgent: %v", err)
	}
	if err := agentport.WriteAgent(plan); err != nil {
		t.Fatalf("WriteAgent: %v", err)
	}

	// A newer skill entry recorded after the agent entry.
	if err := agentport.RecordEntry(agentport.ManifestEntry{
		Name: "unrelated-skill", Kind: agentport.KindSkill, Provider: agentport.ProviderClaude, Scope: agentport.ScopeUser,
	}); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	cmd := AgentRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--last"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	opencodePath := filepath.Join(plan.TargetPaths, "reviewer.md")
	if _, err := os.Stat(opencodePath); !os.IsNotExist(err) {
		t.Fatalf("expected the agent file removed, stat err = %v", err)
	}

	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Name != "unrelated-skill" {
		t.Fatalf("Entries = %#v, want only the untouched skill entry to remain", m.Entries)
	}
}

func TestAgentRollback_ByID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudePath := writeClaudeUserAgent(t, home)

	src, err := agentport.NewClaudeAgentAdapter().Load(claudePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := agentport.MigrateAgent(src, agentport.NewOpencodeAgentAdapter(), agentport.ScopeUser)
	if err != nil {
		t.Fatalf("MigrateAgent: %v", err)
	}
	if err := agentport.WriteAgent(plan); err != nil {
		t.Fatalf("WriteAgent: %v", err)
	}

	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entry, ok := agentport.LastEntry(m)
	if !ok {
		t.Fatalf("expected a manifest entry")
	}

	cmd := AgentRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{entry.ID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	opencodePath := filepath.Join(plan.TargetPaths, "reviewer.md")
	if _, err := os.Stat(opencodePath); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed, stat err = %v", opencodePath, err)
	}
}

func TestAgentRollback_RejectsSkillKindID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HOME", home)

	if err := agentport.RecordEntry(agentport.ManifestEntry{
		Name: "some-skill", Kind: agentport.KindSkill, Provider: agentport.ProviderClaude, Scope: agentport.ScopeUser,
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

	cmd := AgentRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{entry.ID})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error rolling back a skill-kind entry via 'relay agent rollback'")
	}
}

func TestAgentRollback_RequiresExactlyOneSelector(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when neither --last nor an id is given")
	}

	cmd2 := AgentRollbackCmd()
	cmd2.SetOut(&buf)
	cmd2.SetArgs([]string{"some-id", "--last"})
	if err := cmd2.Execute(); err == nil {
		t.Fatalf("expected an error when both --last and an id are given")
	}
}

func TestAgentRollback_NoAgentEntriesErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--last"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when the manifest has no agent entries")
	}
}

func TestAgentRollback_RefusesModifiedFileWithoutForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudePath := writeClaudeUserAgent(t, home)

	src, err := agentport.NewClaudeAgentAdapter().Load(claudePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := agentport.MigrateAgent(src, agentport.NewOpencodeAgentAdapter(), agentport.ScopeUser)
	if err != nil {
		t.Fatalf("MigrateAgent: %v", err)
	}
	if err := agentport.WriteAgent(plan); err != nil {
		t.Fatalf("WriteAgent: %v", err)
	}

	opencodePath := filepath.Join(plan.TargetPaths, "reviewer.md")
	if err := os.WriteFile(opencodePath, []byte("modified\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := AgentRollbackCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--last"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected rollback to refuse a modified file without --force")
	}
	if _, err := os.Stat(opencodePath); err != nil {
		t.Fatalf("expected the modified file to remain: %v", err)
	}

	cmd2 := AgentRollbackCmd()
	var buf2 bytes.Buffer
	cmd2.SetOut(&buf2)
	cmd2.SetArgs([]string{"--last", "--force"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("Execute with --force: %v\noutput:\n%s", err, buf2.String())
	}
	if _, err := os.Stat(opencodePath); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed with --force, stat err = %v", opencodePath, err)
	}
}
