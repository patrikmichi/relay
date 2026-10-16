package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentsync"
)

func runWithInput(t *testing.T, cmd *cobra.Command, input string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetIn(strings.NewReader(input))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	return out.String(), err
}

func TestAgentModeRefusesAgentConfigWrites(t *testing.T) {
	home := syncHome(t)
	t.Setenv("RELAY_AGENT", "1")

	cases := map[string]*cobra.Command{
		"sync":        SyncCmd(),
		"mcp install": McpInstallCmd(),
		"mcp remove":  McpRemoveCmd(),
	}
	args := map[string][]string{
		"sync":        {"--to", "claude", "--yes"},
		"mcp install": {"files", "--command", "npx", "--to", "claude", "--yes"},
		"mcp remove":  {"files", "--to", "claude", "--yes"},
	}
	for name, cmd := range cases {
		_, err := runCommand(t, cmd, args[name]...)
		if err == nil || !strings.Contains(err.Error(), "RELAY_AGENT") {
			t.Errorf("%s = %v, want an agent-mode refusal", name, err)
		}
	}
	if _, err := runCommand(t, SkillInstallCmd(), t.TempDir(), "--to", "claude", "--yes"); err == nil || !strings.Contains(err.Error(), "RELAY_AGENT") {
		t.Errorf("skill install = %v, want an agent-mode refusal", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err == nil {
		t.Error("an agent-mode run wrote the Claude settings")
	}
	if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--dry-run"); err != nil {
		t.Errorf("dry run is read-only and must work in agent mode: %v", err)
	}
}

func TestHookWriteNeedsConfirmationWithFullDiff(t *testing.T) {
	home := syncHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")

	// The test process's own stdin is not a terminal.
	out, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "hooks")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-terminal sync of a hook = %v, want a refusal naming --yes\n%s", err, out)
	}
	if _, statErr := os.Stat(settings); statErr == nil {
		t.Fatal("a refused sync wrote settings.json")
	}

	out, err = runWithInput(t, SyncCmd(), "n\n", "--to", "claude", "--kind", "hooks")
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("declined sync = %v\n%s", err, out)
	}
	for _, want := range []string{"/opt/notify.sh", "+++ b/", "Write these changes?"} {
		if !strings.Contains(out, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, out)
		}
	}
	if _, statErr := os.Stat(settings); statErr == nil {
		t.Fatal("a declined sync wrote settings.json")
	}

	if out, err = runWithInput(t, SyncCmd(), "y\n", "--to", "claude", "--kind", "hooks"); err != nil {
		t.Fatalf("confirmed sync: %v\n%s", err, out)
	}
	if !strings.Contains(readTestFile(t, settings), "/opt/notify.sh") {
		t.Error("confirmed sync did not write the hook")
	}
}

func TestStdioServerInstallNeedsConfirmation(t *testing.T) {
	home := syncHome(t)
	claude := filepath.Join(home, ".claude.json")

	_, err := runCommand(t, McpInstallCmd(), "files", "--command", "npx", "--arg", "-y", "--to", "claude")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-terminal install of a stdio server = %v, want a refusal naming --yes", err)
	}
	if _, statErr := os.Stat(claude); statErr == nil {
		t.Fatal("a refused install wrote the agent config")
	}
	if out, err := runWithInput(t, McpInstallCmd(), "y\n", "files", "--command", "npx", "--arg", "-y", "--to", "claude"); err != nil {
		t.Fatalf("confirmed install: %v\n%s", err, out)
	}
}

func TestRemoteServerAndRulesNeedNoConfirmation(t *testing.T) {
	home := syncHome(t)
	if out, err := runWithInput(t, McpInstallCmd(), "", "docs", "--url", "https://docs.example.com/mcp", "--to", "claude"); err != nil {
		t.Fatalf("remote server install: %v\n%s", err, out)
	}
	if out, err := runWithInput(t, SyncCmd(), "", "--to", "claude", "--kind", "rules"); err != nil {
		t.Fatalf("rules sync: %v\n%s", err, out)
	}
	if !strings.Contains(readTestFile(t, filepath.Join(home, ".claude.json")), "docs.example.com") {
		t.Error("remote server was not written")
	}
}

func TestMCPServerURLsMustBeHTTPS(t *testing.T) {
	for url, ok := range map[string]bool{
		"https://docs.example.com/mcp": true,
		"http://localhost:8080/mcp":    true,
		"http://127.0.0.1:9000/mcp":    true,
		"http://docs.example.com/mcp":  false,
		"ftp://docs.example.com/mcp":   false,
		"https://user:pw@example.com/": false,
		"not a url":                    false,
	} {
		err := agentsync.MCPServer{Name: "x", URL: url}.Validate()
		if (err == nil) != ok {
			t.Errorf("Validate(%q) = %v, want ok=%v", url, err, ok)
		}
	}
}
