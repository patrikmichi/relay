package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/agentport"
	"github.com/patrikmichi/zuano/internal/config"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// saveGateway stores the gateway in the config file, the only place the
// gateway MCP entry takes its URL from.
func saveGateway(t *testing.T, url string) {
	t.Helper()
	if err := config.Save(config.Config{GatewayURL: url}); err != nil {
		t.Fatal(err)
	}
}

// syncHome isolates HOME, installs fake agent homes and relay sync sources.
func syncHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_URL", "")
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")
	for _, d := range []string{".claude", ".codex", ".gemini"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(home, ".config", "relay", "sync")
	writeTestFile(t, filepath.Join(src, "rules", "style.md"), "---\ndescription: Style\n---\nBe terse.\n")
	writeTestFile(t, filepath.Join(src, "commands", "review.md"), "---\ndescription: Review\n---\nReview $ARGUMENTS\n")
	writeTestFile(t, filepath.Join(src, "hooks.json"), `{"hooks":[{"event":"stop","command":"/opt/notify.sh"}]}`)
	return home
}

func TestSyncDetectsAgentsWritesAndIsIdempotent(t *testing.T) {
	home := syncHome(t)
	saveGateway(t, "https://gateway.example.com")

	out, err := runCommand(t, SyncCmd(), "--yes")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	for _, rel := range []string{
		".claude.json", ".claude/settings.json", ".claude/CLAUDE.md", ".claude/commands/review.md",
		".codex/config.toml", ".codex/hooks.json", ".codex/AGENTS.md", ".codex/prompts/review.md",
		".gemini/settings.json", ".gemini/GEMINI.md", ".gemini/commands/review.toml",
	} {
		if _, err := os.Stat(filepath.Join(home, rel)); err != nil {
			t.Errorf("%s not written: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor")); err == nil {
		t.Error("undetected agent cursor was written")
	}
	claude := readTestFile(t, filepath.Join(home, ".claude.json"))
	if !strings.Contains(claude, "https://gateway.example.com/api/mcp") || strings.Contains(claude, "Authorization") {
		t.Errorf("gateway entry without an API key should carry no credentials:\n%s", claude)
	}

	again, err := runCommand(t, SyncCmd())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(again, " added ") || strings.Contains(again, " updated ") {
		t.Errorf("second sync changed something:\n%s", again)
	}
}

func TestSyncReferencesTheAPIKeyOnlyWhenAsked(t *testing.T) {
	home := syncHome(t)
	saveGateway(t, "https://gateway.example.com")
	t.Setenv("GATEWAY_API_KEY", "gw-secret-value")
	if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "mcp"); err != nil {
		t.Fatal(err)
	}
	got := readTestFile(t, filepath.Join(home, ".claude.json"))
	if strings.Contains(got, "GATEWAY_API_KEY") || strings.Contains(got, "gw-secret-value") || strings.Contains(got, "Authorization") {
		t.Errorf("auto must use OAuth even when a key is set:\n%s", got)
	}

	for _, mode := range []string{"api-key", "apikey"} {
		if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "mcp", "--mcp-auth", mode, "--force"); err != nil {
			t.Fatal(err)
		}
		got = readTestFile(t, filepath.Join(home, ".claude.json"))
		if !strings.Contains(got, "Bearer ${GATEWAY_API_KEY}") || strings.Contains(got, "gw-secret-value") {
			t.Errorf("--mcp-auth %s must reference the variable, never embed the key:\n%s", mode, got)
		}
	}
}

func TestSyncDryRunShowsDiffAndWritesNothing(t *testing.T) {
	home := syncHome(t)
	out, err := runCommand(t, SyncCmd(), "--dry-run", "--to", "claude")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[dry-run] no files written", "+++ b/", "CLAUDE.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "CLAUDE.md")); err == nil {
		t.Error("dry run wrote CLAUDE.md")
	}
}

func TestSyncJSONReport(t *testing.T) {
	syncHome(t)
	out, err := runCommand(t, SyncCmd(), "--dry-run", "--json", "--to", "codex", "--kind", "rules")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		DryRun bool
		Items  []struct{ Agent, Kind, Status string }
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if !doc.DryRun || len(doc.Items) != 1 || doc.Items[0].Agent != "codex" || doc.Items[0].Status != "added" {
		t.Errorf("report = %+v", doc)
	}
}

func TestSyncValidatesFlags(t *testing.T) {
	syncHome(t)
	for args, want := range map[string]string{
		"--to vim":                             "unknown agent",
		"--kind widgets":                       "unknown artifact kind",
		"--scope galaxy":                       "invalid --scope",
		"--mcp-auth magic":                     "invalid --mcp-auth",
		"--from nope --kind skills --to codex": "unknown --from provider",
	} {
		saveGateway(t, "https://gateway.example.com")
		_, err := runCommand(t, SyncCmd(), strings.Fields(args)...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", args, err, want)
		}
	}
}

func TestSyncProjectScopeUsesProjectSources(t *testing.T) {
	syncHome(t)
	project := t.TempDir()
	t.Chdir(project)
	writeTestFile(t, filepath.Join(project, ".relay", "rules", "repo.md"), "Run the tests.\n")
	if _, err := runCommand(t, SyncCmd(), "--scope", "project", "--to", "claude,gemini", "--yes"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"CLAUDE.md", "GEMINI.md"} {
		if !strings.Contains(readTestFile(t, filepath.Join(project, rel)), "Run the tests.") {
			t.Errorf("%s lacks the project rule", rel)
		}
	}
}

func TestSyncReportsConflictsAndKeepsUserFiles(t *testing.T) {
	home := syncHome(t)
	own := filepath.Join(home, ".claude", "commands", "review.md")
	writeTestFile(t, own, "mine\n")
	out, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "commands")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "conflict") || !strings.Contains(out, "--force") {
		t.Errorf("conflict not reported:\n%s", out)
	}
	if readTestFile(t, own) != "mine\n" {
		t.Error("user command overwritten")
	}
}

func TestSyncMatrixListsEveryAgent(t *testing.T) {
	syncHome(t)
	out, err := runCommand(t, SyncCmd(), "--matrix")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"claude", "cline", "windsurf", "copilot", "HOOKS", "SUBAGENTS", "Not supported:", "cline hooks (user)"} {
		if !strings.Contains(out, want) {
			t.Errorf("matrix missing %q:\n%s", want, out)
		}
	}
}

func TestSyncCopiesSkillsAndSubagentsAndReportsUnsupported(t *testing.T) {
	home := syncHome(t)
	writeTestFile(t, filepath.Join(home, ".claude", "skills", "hello", "SKILL.md"), "---\nname: hello\ndescription: Say hello\n---\nSay hello.\n")
	writeTestFile(t, filepath.Join(home, ".claude", "agents", "reviewer.md"), "---\nname: reviewer\ndescription: Reviews code\n---\nReview carefully.\n")

	out, err := runCommand(t, SyncCmd(), "--to", "claude,codex,windsurf", "--kind", "skills,subagents")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	codexSkills, _ := agentport.AdapterByID("codex")
	if _, err := os.Stat(filepath.Join(codexSkills.UserDirs()[0], "hello", "SKILL.md")); err != nil {
		t.Errorf("skill not copied to codex: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "agents", "reviewer.toml")); err != nil {
		t.Errorf("subagent not copied to codex: %v", err)
	}
	if !strings.Contains(out, "windsurf") || !strings.Contains(out, "unsupported") {
		t.Errorf("windsurf subagents should be reported unsupported:\n%s", out)
	}

	again, err := runCommand(t, SyncCmd(), "--to", "claude,codex,windsurf", "--kind", "skills,subagents")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(again, " added ") {
		t.Errorf("second run re-added artifacts:\n%s", again)
	}
}

func TestSyncDoesNotCopyLegacyClaudeCommandsAsSkills(t *testing.T) {
	home := syncHome(t)
	writeTestFile(t, filepath.Join(home, ".claude", "commands", "legacy.md"), "---\ndescription: Legacy\n---\nDo it.\n")
	if _, err := runCommand(t, SyncCmd(), "--to", "claude,codex", "--kind", "skills"); err != nil {
		t.Fatal(err)
	}
	codexSkills, _ := agentport.AdapterByID("codex")
	if _, err := os.Stat(filepath.Join(codexSkills.UserDirs()[0], "legacy")); err == nil {
		t.Error("a flat Claude command was copied as a skill")
	}
}
