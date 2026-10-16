package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentsync"
)

const shadowConfig = `{"mcpServers":{"shadow":{"command":"npx","args":["-y","@evil/shadow"]},"mine":{"url":"https://docs.example.com/mcp"}}}` + "\n"

func quarantineSetup(t *testing.T, list *string) (*packServer, string) {
	t.Helper()
	gw := a2aGateway(t, routeFns{
		"GET /api/agents/shadow-mcp/quarantine":          func(_ *packServer, w http.ResponseWriter, r *http.Request) { jsonReply(200, *list)(nil, w, r) },
		"POST /api/agents/shadow-mcp/quarantine/actions": jsonReply(200, `{}`),
	})
	home := os.Getenv("HOME")
	cfg := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(cfg, []byte(shadowConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return gw, cfg
}

func TestQuarantineAppliesListsAndRestores(t *testing.T) {
	list := `{"action":"quarantine","entries":[{"agent":"claude","scope":"user","name":"shadow"},{"agent":"claude","scope":"user","name":"gone"}]}`
	gw, cfg := quarantineSetup(t, &list)

	out, err := runCommand(t, AgentsCmd(), "quarantine", "apply", "--dry-run")
	if err != nil || !strings.Contains(out, "[dry-run] would quarantine claude shadow") || !strings.Contains(out, "Left claude gone") {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if raw, _ := os.ReadFile(cfg); string(raw) != shadowConfig || gw.count("POST /api/agents/shadow-mcp/quarantine/actions") != 0 {
		t.Error("a dry run changed the config or reported")
	}

	out, err = runCommand(t, AgentsCmd(), "quarantine", "apply")
	if err != nil || !strings.Contains(out, "Quarantined claude shadow (~/.claude.json)") {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	after, _ := os.ReadFile(cfg)
	if strings.Contains(string(after), "shadow") || !strings.Contains(string(after), `"mine"`) {
		t.Errorf("config after:\n%s", after)
	}
	rep := gw.body("POST /api/agents/shadow-mcp/quarantine/actions", 0)
	acts, _ := rep["actions"].([]any)
	if rep["installId"] == "" || len(acts) != 2 {
		t.Fatalf("report = %v", rep)
	}
	first := acts[0].(map[string]any)
	if first["path"] != "~/.claude.json" || first["result"] != "quarantined" || first["name"] != "shadow" || len(first["hash"].(string)) != 16 || strings.Contains(first["path"].(string), os.Getenv("HOME")) {
		t.Errorf("action = %v", first)
	}

	// The copy of the config as it was is kept, privately.
	st, _, err := loadQuarantineState()
	if err != nil || len(st.Items) != 1 {
		t.Fatalf("state = %+v, %v", st, err)
	}
	backup, err := os.ReadFile(filepath.Join(st.Items[0].BackupDir, "config.before"))
	if err != nil || string(backup) != shadowConfig {
		t.Errorf("backup = %q, %v", backup, err)
	}
	if info, _ := os.Stat(filepath.Join(st.Items[0].BackupDir, "config.before")); info.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %v", info.Mode().Perm())
	}

	out, err = runCommand(t, AgentsCmd(), "quarantine", "list")
	if err != nil || !strings.Contains(out, st.Items[0].ID) || !strings.Contains(out, "shadow") {
		t.Errorf("list: %v\n%s", err, out)
	}
	if out, err := runCommand(t, AgentsCmd(), "quarantine", "list", "--json"); err != nil || !strings.Contains(out, `"items"`) {
		t.Errorf("list --json: %v", err)
	}

	out, err = runCommand(t, AgentsCmd(), "quarantine", "restore", st.Items[0].ID)
	if err != nil || !strings.Contains(out, "Restored claude shadow") {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	back, _ := os.ReadFile(cfg)
	if !strings.Contains(string(back), "@evil/shadow") || !strings.Contains(string(back), `"mine"`) {
		t.Errorf("config after restore:\n%s", back)
	}
	if st, _, _ := loadQuarantineState(); len(st.Items) != 0 {
		t.Errorf("state after restore = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config", "zuano", "quarantine", "x")); err == nil {
		t.Error("unexpected path")
	}
	if gw.count("POST /api/agents/shadow-mcp/quarantine/actions") != 2 {
		t.Errorf("reports = %d, want one for the quarantine and one for the restore", gw.count("POST /api/agents/shadow-mcp/quarantine/actions"))
	}
	if out, err := runCommand(t, AgentsCmd(), "quarantine", "list"); err != nil || !strings.Contains(out, "Nothing is quarantined") {
		t.Errorf("empty list: %v %q", err, out)
	}
}

func TestQuarantineReportOnlyAndChangedEntries(t *testing.T) {
	list := `{"action":"report","entries":[{"agent":"claude","scope":"user","name":"shadow"}]}`
	_, cfg := quarantineSetup(t, &list)
	if out, err := runCommand(t, AgentsCmd(), "quarantine", "apply"); err != nil || !strings.Contains(out, "only reports") {
		t.Errorf("report only: %v %q", err, out)
	}
	list = `{"action":"quarantine","entries":[]}`
	if out, err := runCommand(t, AgentsCmd(), "quarantine", "apply"); err != nil || !strings.Contains(out, "No server is listed") {
		t.Errorf("empty: %v %q", err, out)
	}
	list = `{"action":"quarantine","entries":[{"agent":"claude","scope":"user","name":"shadow","hash":"0000000000000000"}]}`
	out, err := runCommand(t, AgentsCmd(), "quarantine", "apply")
	if err != nil || !strings.Contains(out, "changed since it was reported") {
		t.Errorf("changed: %v\n%s", err, out)
	}
	if raw, _ := os.ReadFile(cfg); string(raw) != shadowConfig {
		t.Error("a changed entry was quarantined")
	}
	if st, _, _ := loadQuarantineState(); len(st.Items) != 0 {
		t.Errorf("state = %+v", st)
	}
}

func TestQuarantineIsRefusedForAgentsAndRestoreChecksItsInput(t *testing.T) {
	list := `{"action":"quarantine","entries":[{"agent":"claude","scope":"user","name":"shadow"}]}`
	quarantineSetup(t, &list)
	for _, args := range [][]string{{"quarantine", "apply"}, {"quarantine", "restore", "--all"}} {
		t.Setenv("ZUANO_AGENT", "1")
		if _, err := runCommand(t, AgentsCmd(), args...); exitCodeOf(err) != exitDenied {
			t.Errorf("%v from an agent: exit %d", args, exitCodeOf(err))
		}
	}
	t.Setenv("ZUANO_AGENT", "")
	for name, args := range map[string][]string{
		"neither": {"quarantine", "restore"},
		"both":    {"quarantine", "restore", "qrt_1", "--all"},
		"unknown": {"quarantine", "restore", "qrt_nope"},
	} {
		if _, err := runCommand(t, AgentsCmd(), args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSyncQuarantineIsQuietUnlessTheOrganisationChoseIt(t *testing.T) {
	list := `{"action":"quarantine","entries":[{"agent":"claude","scope":"user","name":"shadow"}]}`
	_, cfg := quarantineSetup(t, &list)
	run := func(dry bool) string {
		cmd := &cobra.Command{Use: "x"}
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		syncQuarantine(cmd, "", dry)
		return out.String()
	}
	t.Setenv("ZUANO_AGENT", "1")
	if out := run(false); out != "" {
		t.Errorf("an agent session: %q", out)
	}
	t.Setenv("ZUANO_AGENT", "")
	list = `{"action":"report","entries":[{"agent":"claude","scope":"user","name":"shadow"}]}`
	if out := run(false); out != "" {
		t.Errorf("report only: %q", out)
	}
	list = `{"action":"quarantine","entries":[{"agent":"claude","scope":"user","name":"shadow"}]}`
	if out := run(true); !strings.Contains(out, "[dry-run] would quarantine") {
		t.Errorf("dry run: %q", out)
	}
	if raw, _ := os.ReadFile(cfg); string(raw) != shadowConfig {
		t.Error("a dry run changed the config")
	}
	if out := run(false); !strings.Contains(out, "Quarantined claude shadow") {
		t.Errorf("sync: %q", out)
	}
	if raw, _ := os.ReadFile(cfg); strings.Contains(string(raw), "shadow") {
		t.Error("the server is still there")
	}
}

func TestHomeRelative(t *testing.T) {
	if homeRelative("/home/a", "/home/a/.claude.json") != "~/.claude.json" || homeRelative("/home/a", "/etc/x") != "/etc/x" {
		t.Error("homeRelative")
	}
	_ = agentsync.Claude
}
