package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport"
)

// TestAgentMigrate_ClaudeToOpencodeAndBack exercises the full round trip
// P1.7 requires: migrate a claude agent to opencode, then migrate the
// opencode-projected agent back to claude, asserting the on-disk file
// shape at each step (flat "<name>.md", opencode's {tool: bool} map),
// the manifest entry's Kind: agent, and that a lossiness report is shown.
func TestAgentMigrate_ClaudeToOpencodeAndBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)

	// --- claude -> opencode ---
	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--to", "opencode"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "fidelity") {
		t.Errorf("expected a fidelity/loss report in the output, got:\n%s", out)
	}

	// Flat "<name>.md" file shape — no per-agent subdirectory.
	opencodePath := filepath.Join(home, ".config", "opencode", "agent", "reviewer.md")
	content, err := os.ReadFile(opencodePath)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", opencodePath, err)
	}
	// opencode's on-disk tools shape is a {tool: bool} map, not Claude's
	// CSV allowlist string.
	if !strings.Contains(string(content), "Read: true") {
		t.Errorf("expected opencode's {tool: bool} map shape in projected content, got:\n%s", content)
	}
	if strings.Contains(string(content), "tools: Read, Bash") {
		t.Errorf("did not expect claude's CSV tools shape to survive projection, got:\n%s", content)
	}

	// Manifest entry recorded with Kind: agent.
	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entry, ok := agentport.LastEntryFor(m, "reviewer", agentport.ProviderOpencode, agentport.ScopeUser, agentport.KindAgent)
	if !ok {
		t.Fatalf("expected a KindAgent manifest entry for reviewer -> opencode")
	}
	if entry.SourceProvider != agentport.ProviderClaude {
		t.Errorf("SourceProvider = %s, want claude", entry.SourceProvider)
	}

	// --- opencode -> claude (migrate the projected agent back) ---
	cmd2 := AgentMigrateCmd()
	var buf2 bytes.Buffer
	cmd2.SetOut(&buf2)
	cmd2.SetArgs([]string{"reviewer", "--from", "opencode", "--to", "claude"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("Execute (back): %v\noutput:\n%s", err, buf2.String())
	}

	claudePath := filepath.Join(home, ".claude", "agents", "reviewer.md")
	claudeContent, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("expected %s to exist after migrating back: %v", claudePath, err)
	}
	if !strings.Contains(string(claudeContent), "name: reviewer") {
		t.Errorf("expected claude's name field in projected content, got:\n%s", claudeContent)
	}

	m2, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if _, ok := agentport.LastEntryFor(m2, "reviewer", agentport.ProviderClaude, agentport.ScopeUser, agentport.KindAgent); !ok {
		t.Fatalf("expected a KindAgent manifest entry for reviewer -> claude (migrated back)")
	}
}

func TestAgentMigrate_RequiresFrom(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--to", "opencode"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when --from is missing")
	}
}

func TestAgentMigrate_UnknownFromProviderErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "codex"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an unsupported --from provider (codex has no agent-file primitive)")
	}
}

func TestAgentMigrate_UnknownToProviderErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--to", "cursor"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an unsupported --to provider (cursor has no subagent primitive)")
	}
}

func TestAgentMigrate_NotFoundErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"does-not-exist", "--from", "claude", "--to", "opencode"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when the named agent doesn't exist")
	}
}

func TestAgentMigrate_DryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--to", "opencode", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "[dry-run] no files written") {
		t.Errorf("expected a dry-run marker, got:\n%s", buf.String())
	}

	opencodePath := filepath.Join(home, ".config", "opencode", "agent", "reviewer.md")
	if _, err := os.Stat(opencodePath); !os.IsNotExist(err) {
		t.Fatalf("expected nothing written under --dry-run, stat err = %v", err)
	}
	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("Entries = %#v, want none under --dry-run", m.Entries)
	}
}

func TestAgentMigrate_StrictAbortsOnDroppedFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home) // has no memory/skills, but "tools"/"model" degrade — Temperature/Mode/Memory/Skills only relevant per-source

	// Claude -> opencode drops nothing (claude source has no
	// Memory/Skills set), so use an agent WITH a claude-only field to
	// force a genuine LossDropped: memory.
	dir := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "with-memory.md"), []byte("---\nname: with-memory\ndescription: d\nmemory: project\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"with-memory", "--from", "claude", "--to", "opencode", "--strict"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected --strict to abort on a dropped field (Memory)")
	}

	opencodePath := filepath.Join(home, ".config", "opencode", "agent", "with-memory.md")
	if _, err := os.Stat(opencodePath); !os.IsNotExist(err) {
		t.Fatalf("expected nothing written when --strict aborts, stat err = %v", err)
	}
}

func TestAgentMigrate_DefaultToAllDetectedExceptFrom(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)
	// Detect() for opencode requires its own user dir to exist.
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode", "agent"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "claude -> opencode") {
		t.Errorf("expected the default --to to include opencode (detected), got:\n%s", buf.String())
	}
}

func TestAgentMigrate_NoOtherProvidersDetectedErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)
	// No opencode dir present -> AgentDetectedProviders() only finds claude.

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when no other agent providers are detected and --to is omitted")
	}
}

func TestAgentMigrate_InvalidScopeErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--scope", "bogus"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an invalid --scope")
	}
}

// TestAgentMigrate_WriteErrorPropagates confirms a write failure (target
// directory's parent unwritable) surfaces as an error from
// applyAgentMigrationToTargets/agentport.WriteAgent, not a silent no-op.
func TestAgentMigrate_WriteErrorPropagates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — permission checks don't apply")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)

	// Make $HOME/.config read-only so opencode's target dir
	// (~/.config/opencode/agent) can never be created.
	configDir := filepath.Join(home, ".config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(configDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(configDir, 0o755) })

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--to", "opencode"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected a write error when the target directory can't be created")
	}
}

// withFakeInteractiveStdin overrides isInteractiveTerminalFn to always
// report true and replaces os.Stdin with a pipe pre-loaded with input, so
// tests can exercise the interactive confirmation-prompt branch without an
// actual pty. Restores both on cleanup.
func withFakeInteractiveStdin(t *testing.T, input string) {
	t.Helper()
	origFn := isInteractiveTerminalFn
	origStdin := os.Stdin
	isInteractiveTerminalFn = func(*os.File) bool { return true }

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdin = r
	if _, err := w.WriteString(input); err != nil {
		t.Fatalf("write to stdin pipe: %v", err)
	}
	_ = w.Close()

	t.Cleanup(func() {
		isInteractiveTerminalFn = origFn
		os.Stdin = origStdin
		_ = r.Close()
	})
}

// TestAgentMigrate_InteractivePromptDeclines confirms a "no" answer to the
// fidelity-loss confirmation prompt skips writing that target.
func TestAgentMigrate_InteractivePromptDeclines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "with-memory.md"), []byte("---\nname: with-memory\ndescription: d\nmemory: project\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	withFakeInteractiveStdin(t, "n\n")

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"with-memory", "--from", "claude", "--to", "opencode"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "skipped") {
		t.Errorf("expected 'skipped' after declining the prompt, got:\n%s", buf.String())
	}
	opencodePath := filepath.Join(home, ".config", "opencode", "agent", "with-memory.md")
	if _, err := os.Stat(opencodePath); !os.IsNotExist(err) {
		t.Fatalf("expected nothing written after declining, stat err = %v", err)
	}
}

// TestAgentMigrate_InteractivePromptAccepts confirms a "yes" answer to the
// fidelity-loss confirmation prompt proceeds with the write.
func TestAgentMigrate_InteractivePromptAccepts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "with-memory.md"), []byte("---\nname: with-memory\ndescription: d\nmemory: project\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	withFakeInteractiveStdin(t, "y\n")

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"with-memory", "--from", "claude", "--to", "opencode"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, buf.String())
	}
	opencodePath := filepath.Join(home, ".config", "opencode", "agent", "with-memory.md")
	if _, err := os.Stat(opencodePath); err != nil {
		t.Fatalf("expected %s written after accepting the prompt: %v", opencodePath, err)
	}
}

// TestApplyAgentMigrationToTargets_MigrateErrorPropagates exercises
// applyAgentMigrationToTargets directly (rather than through the CLI's
// already-validated Load path) with an Agent whose Name fails
// agentport.ValidateName, so agentport.MigrateAgent's own validation
// surfaces as an error.
func TestApplyAgentMigrationToTargets_MigrateErrorPropagates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	invalid := &agentport.Agent{Name: "Not Valid", Description: "d"}
	var buf bytes.Buffer
	err := applyAgentMigrationToTargets(&buf, invalid, "claude", []agentport.AgentAdapter{agentport.NewOpencodeAgentAdapter()}, agentport.ScopeUser, false, false)
	if err == nil {
		t.Fatalf("expected an error migrating an agent with an invalid name")
	}
}
