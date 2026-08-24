package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport"
)

func TestAgentList_EnumeratesAcrossProviders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)
	writeOpencodeUserAgent(t, home)

	cmd := AgentListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "claude") || !strings.Contains(out, "opencode") {
		t.Errorf("expected output to mention both claude and opencode, got:\n%s", out)
	}
	if strings.Count(out, "reviewer.md") != 2 {
		t.Errorf("expected reviewer listed once per provider, got:\n%s", out)
	}
}

func TestAgentList_FiltersByProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)
	writeOpencodeUserAgent(t, home)

	cmd := AgentListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--provider", "claude"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "claude") {
		t.Errorf("expected output to mention claude, got:\n%s", out)
	}
	if strings.Contains(out, "opencode") {
		t.Errorf("expected output to NOT mention opencode when filtered, got:\n%s", out)
	}
}

func TestAgentList_NoneFoundIsGraceful(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--provider", "claude"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute should not error when nothing is found: %v", err)
	}
	if !strings.Contains(buf.String(), "no agents found") {
		t.Errorf("expected a graceful 'no agents found' message, got:\n%s", buf.String())
	}
}

func TestAgentList_UnknownProviderErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--provider", "cursor"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an unknown/unsupported --provider")
	}
}

func TestAgentList_InvalidScopeErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--scope", "bogus"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an invalid --scope")
	}
}

func TestAgentList_ProvenanceJoinsManifest(t *testing.T) {
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

	cmd := AgentListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--provider", "opencode", "--provenance"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "reviewer") {
		t.Fatalf("expected output to mention reviewer, got:\n%s", out)
	}
	if !strings.Contains(out, "from claude") {
		t.Errorf("expected provenance to show the claude source, got:\n%s", out)
	}
}

func TestAgentList_ProvenanceNoManifestRecordIsGraceful(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home) // present on disk, but never recorded in the manifest

	cmd := AgentListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--provider", "claude", "--provenance"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), "no manifest record") {
		t.Errorf("expected 'no manifest record' for an unrecorded agent, got:\n%s", buf.String())
	}
}
