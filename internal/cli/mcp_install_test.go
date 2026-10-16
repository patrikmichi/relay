package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentsync"
)

func TestMcpInstallDirectServerThenRemove(t *testing.T) {
	home := syncHome(t)
	out, err := runCommand(t, McpInstallCmd(), "docs", "--url", "https://docs.example.com/mcp", "--bearer-env", "DOCS_TOKEN", "--to", "claude,codex")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	claude := readTestFile(t, filepath.Join(home, ".claude.json"))
	if !strings.Contains(claude, "Bearer ${DOCS_TOKEN}") {
		t.Errorf("claude config:\n%s", claude)
	}
	if !strings.Contains(readTestFile(t, filepath.Join(home, ".codex", "config.toml")), `bearer_token_env_var = "DOCS_TOKEN"`) {
		t.Error("codex config lacks the bearer env reference")
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "settings.json")); err == nil {
		t.Error("--to claude,codex wrote gemini")
	}

	// A later full sync keeps the installed server.
	if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "mcp"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readTestFile(t, filepath.Join(home, ".claude.json")), "docs.example.com") {
		t.Error("sync dropped the installed server")
	}

	if out, err := runCommand(t, McpRemoveCmd(), "docs", "--to", "claude,codex"); err != nil {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Errorf("emptied claude config left behind:\n%s", readTestFile(t, filepath.Join(home, ".claude.json")))
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); err == nil {
		t.Error("emptied codex config left behind")
	}
	src, err := agentsync.LoadMCPSource(agentsync.Env{Home: home}, agentsync.ScopeUser)
	if err != nil || len(src.Servers) != 0 {
		t.Errorf("source still lists servers: %+v %v", src, err)
	}
}

func TestMcpInstallAndRemoveEmitOneJSONDocument(t *testing.T) {
	syncHome(t)
	run := func(cmd *cobra.Command, args ...string) (stdout, stderr string) {
		t.Helper()
		var out, errBuf bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errBuf)
		cmd.SetArgs(args)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%s: %v\n%s", cmd.Name(), err, errBuf.String())
		}
		return out.String(), errBuf.String()
	}
	for name, cmd := range map[string]*cobra.Command{
		"install": McpInstallCmd(),
	} {
		stdout, stderr := run(cmd, "files", "--command", "npx", "--env", "ROOT=/srv", "--to", "claude", "--yes", "--json")
		var doc struct {
			Items []agentsync.Item `json:"items"`
		}
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil || len(doc.Items) == 0 {
			t.Fatalf("%s: stdout is not one JSON report: %v\n%s", name, err, stdout)
		}
		if !strings.Contains(stderr, "warning: ROOT") {
			t.Errorf("%s: the literal-value note should go to stderr, got %q", name, stderr)
		}
	}
	stdout, _ := run(McpRemoveCmd(), "files", "--to", "claude", "--json")
	var doc struct {
		Items []agentsync.Item `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || len(doc.Items) == 0 {
		t.Fatalf("remove: stdout is not one JSON report: %v\n%s", err, stdout)
	}
}

func TestMcpSourceRefusesDuplicateNamesAndTheGatewaysName(t *testing.T) {
	home := syncHome(t)
	if _, err := runCommand(t, McpInstallCmd(), "relay", "--url", "https://docs.example.com/mcp", "--to", "claude"); err == nil || !strings.Contains(err.Error(), "gateway's own") {
		t.Errorf("installing a server named relay = %v, want a refusal", err)
	}
	writeTestFile(t, filepath.Join(home, ".config", "relay", "sync", "mcp.json"),
		`{"servers":[{"name":"docs","url":"https://a.example.com/mcp"},{"name":"docs","url":"https://b.example.com/mcp"}]}`)
	if _, err := agentsync.LoadMCPSource(agentsync.Env{Home: home}, agentsync.ScopeUser); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Errorf("duplicate names = %v, want an error", err)
	}
}

func TestMcpInstallDryRunPersistsNothing(t *testing.T) {
	home := syncHome(t)
	out, err := runCommand(t, McpInstallCmd(), "files", "--command", "npx", "--arg", "-y", "--arg", "@acme/files", "--env", "ROOT=/srv", "--dry-run", "--to", "claude")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"warning: ROOT is written as a literal", "[dry-run]", `"command": "npx"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Error("dry run wrote the agent config")
	}
	if _, err := os.Stat(agentsync.MCPSourcePath(agentsync.Env{Home: home}, agentsync.ScopeUser)); err == nil {
		t.Error("dry run saved the MCP source")
	}
}

func TestMcpInstallValidatesInput(t *testing.T) {
	syncHome(t)
	cases := map[string][]string{
		"use either --url or --command": {"x", "--url", "https://x.example/mcp", "--command", "x"},
		"--bearer-env only applies":     {"x", "--command", "x", "--bearer-env", "T"},
		"invalid --env":                 {"x", "--command", "x", "--env", "oops"},
		"invalid --scope":               {"x", "--url", "https://x.example/mcp", "--scope", "galaxy"},
		"invalid MCP server name":       {"../x", "--url", "https://x.example/mcp"},
		"user scope":                    {"x", "--scope", "project"},
	}
	for want, args := range cases {
		_, err := runCommand(t, McpInstallCmd(), args...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want %q", args, err, want)
		}
	}
	if _, err := runCommand(t, McpRemoveCmd(), "ghost"); err == nil || !strings.Contains(err.Error(), "no MCP server named") {
		t.Errorf("remove of unknown server: %v", err)
	}
}

func catalogServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/integrations" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"serviceCount":2,"toolCount":0,"services":[
			{"id":"clockify","name":"Clockify","accessible":true,"toolCount":0,"tools":[]},
			{"id":"fakturoid","name":"Fakturoid","accessible":false,"toolCount":0,"tools":[]}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMcpInstallCatalogServiceEnablesGatewayServer(t *testing.T) {
	srv := catalogServer(t)
	withAPIKeyGateway(t, srv)
	saveGateway(t, srv.URL)
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runCommand(t, McpInstallCmd(), "clockify", "--to", "claude")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "relay connect clockify") {
		t.Errorf("no connect hint:\n%s", out)
	}
	got := readTestFile(t, filepath.Join(home, ".claude.json"))
	if !strings.Contains(got, srv.URL+"/api/mcp") || strings.Contains(got, "GATEWAY_API_KEY") {
		t.Errorf("gateway server not written:\n%s", got)
	}

	if out, err := runCommand(t, McpInstallCmd(), "fakturoid", "--to", "claude"); err != nil || !strings.Contains(out, "no access") {
		t.Errorf("inaccessible service: %v\n%s", err, out)
	}
	if _, err := runCommand(t, McpInstallCmd(), "nonexistent", "--to", "claude"); err == nil || !strings.Contains(err.Error(), "relay connectors search") {
		t.Errorf("unknown service: %v", err)
	}
}

func TestMcpRemoveGatewayStopsSyncingIt(t *testing.T) {
	srv := catalogServer(t)
	withAPIKeyGateway(t, srv)
	saveGateway(t, srv.URL)
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "mcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal("gateway server was not synced")
	}
	if _, err := runCommand(t, McpRemoveCmd(), "relay", "--to", "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, SyncCmd(), "--to", "claude", "--kind", "mcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Error("gateway server came back after remove")
	}
}

func TestMcpInstallCatalogNeedsGateway(t *testing.T) {
	syncHome(t)
	if _, err := runCommand(t, McpInstallCmd(), "clockify"); err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Errorf("err = %v", err)
	}
}
