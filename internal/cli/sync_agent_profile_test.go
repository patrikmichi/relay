package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/config"
)

func TestSync_SetsTheAgentProfileWhereAgentsHaveAnEnvironment(t *testing.T) {
	home := syncHome(t)

	out, err := runCommand(t, SyncCmd(), "--to", "claude,codex,gemini-cli", "--kind", "env")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	settings := readTestFile(t, filepath.Join(home, ".claude", "settings.json"))
	if !strings.Contains(settings, `"env"`) || !strings.Contains(settings, `"ZUANO_AGENT": "1"`) {
		t.Errorf("Claude settings:\n%s", settings)
	}
	codex := readTestFile(t, filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(codex, "[shell_environment_policy.set]") || !strings.Contains(codex, `ZUANO_AGENT = "1"`) {
		t.Errorf("Codex config:\n%s", codex)
	}
	if !strings.Contains(out, "no agent session exists; run `zuano login --agent`") {
		t.Errorf("missing the agent login warning:\n%s", out)
	}
	if !strings.Contains(out, "gemini-cli") || !strings.Contains(out, "unsupported") {
		t.Errorf("agents without an environment setting should be reported as unsupported:\n%s", out)
	}
}

func TestSync_NoLoginWarningOnceAnAgentSessionExists(t *testing.T) {
	syncHome(t)
	if err := config.SetAgentEmail("user@example.com"); err != nil {
		t.Fatal(err)
	}
	out, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "zuano login --agent") {
		t.Errorf("unexpected warning:\n%s", out)
	}
}

func TestSync_OtherKindsLeaveTheEnvironmentAlone(t *testing.T) {
	home := syncHome(t)
	out, err := runCommand(t, SyncCmd(), "--to", "claude,codex", "--kind", "mcp,rules")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "login --agent") {
		t.Errorf("unexpected warning:\n%s", out)
	}
	if got := readTestFile(t, filepath.Join(home, ".claude", "CLAUDE.md")); got == "" {
		t.Fatal("rules were not synced")
	}
	for _, rel := range []string{".claude/settings.json", ".codex/config.toml"} {
		if _, err := os.Stat(filepath.Join(home, rel)); err == nil {
			t.Errorf("%s was written by a sync that excluded the env kind", rel)
		}
	}
}

func TestSync_MarksStdioZuanoServersWithoutChangingTheSavedSource(t *testing.T) {
	home := syncHome(t)
	if _, err := runCommand(t, McpInstallCmd(), "bridge", "--command", "/usr/local/bin/zuano", "--arg", "mcp", "--to", "claude", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, McpInstallCmd(), "files", "--command", "npx", "--arg", "-y", "--to", "claude", "--yes"); err != nil {
		t.Fatal(err)
	}

	claude := readTestFile(t, filepath.Join(home, ".claude.json"))
	var bridge, files string
	for _, part := range strings.SplitAfter(claude, "}") {
		switch {
		case strings.Contains(part, `"/usr/local/bin/zuano"`):
			bridge = part
		case strings.Contains(part, `"npx"`):
			files = part
		}
	}
	if !strings.Contains(bridge, `"ZUANO_AGENT": "1"`) {
		t.Errorf("zuano stdio server is not marked:\n%s", claude)
	}
	if strings.Contains(files, "ZUANO_AGENT") {
		t.Errorf("an unrelated server was marked:\n%s", claude)
	}
	source := readTestFile(t, agentsync.MCPSourcePath(agentsync.Env{Home: home}, agentsync.ScopeUser))
	if strings.Contains(source, "ZUANO_AGENT") {
		t.Errorf("the saved source must stay as the user wrote it:\n%s", source)
	}
}

func TestMarkZuanoServers(t *testing.T) {
	in := []agentsync.MCPServer{
		{Name: "a", Command: "zuano"},
		{Name: "b", Command: `C:\tools\zuano.exe`},
		{Name: "c", Command: "/usr/bin/zuano", Env: map[string]string{"X": "1"}},
		{Name: "d", Command: "zuano", Env: map[string]string{agentEnvVar: "0"}},
		{Name: "e", Command: "zuanoctl"},
		{Name: "f", URL: "https://example.com/mcp"},
	}
	out := markZuanoServers(in)
	for i, want := range map[int]string{0: "1", 2: "1", 3: "0", 4: "", 5: ""} {
		if got := out[i].Env[agentEnvVar]; got != want {
			t.Errorf("%s: %s = %q, want %q", in[i].Name, agentEnvVar, got, want)
		}
	}
	if out[2].Env["X"] != "1" {
		t.Errorf("existing environment lost: %v", out[2].Env)
	}
	for _, s := range in {
		if s.Name == "a" && s.Env != nil {
			t.Error("the input slice was modified")
		}
	}
}

func TestSync_WarnsWhenTheGatewayEntryUsesAnAPIKey(t *testing.T) {
	syncHome(t)
	saveGateway(t, "https://gateway.example.com")
	t.Setenv("ZUANO_API_KEY", "gw-secret-value")

	out, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "mcp", "--mcp-auth", "api-key")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "$ZUANO_API_KEY, which is not marked as an agent's: calls through it bypass approvals") {
		t.Errorf("missing the API key warning:\n%s", out)
	}
	if strings.Contains(out, "gw-secret-value") {
		t.Error("the key itself must never be printed")
	}

	for _, mode := range []string{"auto", "oauth"} {
		out, err = runCommand(t, SyncCmd(), "--to", "claude", "--kind", "mcp", "--mcp-auth", mode, "--force")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "bypass approvals") {
			t.Errorf("--mcp-auth %s: OAuth entries are marked by the gateway; unexpected warning:\n%s", mode, out)
		}
	}
}

func TestSync_AgentsWithoutOAuthGetNoCredentialAndAHowTo(t *testing.T) {
	home := syncHome(t)
	for _, d := range []string{".cline"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(home, "proj", ".vscode", "mcp.json"), "{}")
	saveGateway(t, "https://gateway.example.com")
	t.Setenv("ZUANO_API_KEY", "gw-secret-value")

	out, err := runCommand(t, SyncCmd(), "--to", "cline,copilot", "--kind", "mcp", "--scope", "user,project")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "gw-secret-value") {
		t.Error("the key must never be printed")
	}
	if !strings.Contains(out, "authenticate the server in Cline") {
		t.Errorf("no Cline authorization hint:\n%s", out)
	}
	cline := readTestFile(t, filepath.Join(home, ".cline", "data", "settings", "cline_mcp_settings.json"))
	if strings.Contains(cline, "Authorization") || strings.Contains(cline, "ZUANO_API_KEY") {
		t.Errorf("Cline entry carries a credential:\n%s", cline)
	}
}
