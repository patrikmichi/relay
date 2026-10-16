package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/agentsync"
)

func doctorChecksByName(t *testing.T) (map[string]doctorCheck, error) {
	t.Helper()
	cmd := DoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"--json"})
	err := cmd.Execute()
	var parsed struct{ Checks []doctorCheck }
	if jerr := json.Unmarshal(out.Bytes(), &parsed); jerr != nil {
		t.Fatalf("invalid JSON: %v: %s", jerr, out.String())
	}
	byName := map[string]doctorCheck{}
	for _, c := range parsed.Checks {
		byName[c.Name] = c
	}
	return byName, err
}

func doctorSyncSetup(t *testing.T) string {
	t.Helper()
	home := setupDoctor(t)
	for _, d := range []string{".claude", ".gemini"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(home, ".config", "relay", "sync")
	writeTestFile(t, filepath.Join(src, "rules", "style.md"), "Be terse.\n")
	writeTestFile(t, filepath.Join(src, "commands", "review.md"), "Review it.\n")
	writeTestFile(t, filepath.Join(src, "hooks.json"), `{"hooks":[{"event":"stop","command":"/opt/notify.sh"}]}`)
	return home
}

func TestDoctorReportsEveryArtifactKindPerAgent(t *testing.T) {
	doctorSyncSetup(t)

	checks, err := doctorChecksByName(t)
	if err != nil {
		t.Fatalf("unsynced machine should be healthy: %v", err)
	}
	for _, name := range []string{"sync:user:claude:rules", "sync:user:claude:commands", "sync:user:claude:hooks", "sync:user:gemini-cli:rules", "sync-source"} {
		c, ok := checks[name]
		if !ok || !c.OK {
			t.Errorf("%s = %+v", name, c)
		}
	}
	if !strings.Contains(checks["sync:user:claude:rules"].Detail, "not synced yet") {
		t.Errorf("detail = %q", checks["sync:user:claude:rules"].Detail)
	}

	if _, err := runCommand(t, SyncCmd(), "--yes"); err != nil {
		t.Fatal(err)
	}
	checks, err = doctorChecksByName(t)
	if err != nil {
		t.Fatalf("synced machine unhealthy: %v", checks)
	}
	if got := checks["sync:user:claude:hooks"].Detail; !strings.Contains(got, "in sync") {
		t.Errorf("hooks detail = %q", got)
	}
}

func TestDoctorFlagsDriftEditedEntriesAndBrokenConfigs(t *testing.T) {
	home := doctorSyncSetup(t)
	saveGateway(t, "https://gateway.example.com")
	t.Setenv("ZUANO_API_KEY", "")
	SetOffline(true)
	t.Cleanup(func() { SetOffline(false) })
	if _, err := runCommand(t, SyncCmd(), "--mcp-auth", "oauth", "--yes"); err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join(home, ".config", "relay", "sync", "rules", "extra.md"), "Another rule.\n")
	checks, err := doctorChecksByName(t)
	if err == nil || checks["sync:user:claude:rules"].OK || !strings.Contains(checks["sync:user:claude:rules"].Detail, "out of sync") {
		t.Fatalf("drift not reported: %v %+v", err, checks["sync:user:claude:rules"])
	}

	if _, err := runCommand(t, SyncCmd(), "--mcp-auth", "oauth"); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(home, ".claude.json")
	writeTestFile(t, claude, strings.Replace(readTestFile(t, claude), "/api/mcp", "/api/other", 1))
	checks, _ = doctorChecksByName(t)
	if c := checks["sync:user:claude:mcp"]; c.OK || !strings.Contains(c.Detail, "edited since relay wrote") {
		t.Errorf("edited entry: %+v", c)
	}

	writeTestFile(t, filepath.Join(home, ".gemini", "settings.json"), `{"mcpServers": `)
	checks, _ = doctorChecksByName(t)
	if c := checks["sync:user:gemini-cli:mcp"]; c.OK || !strings.Contains(c.Detail, "cannot sync") {
		t.Errorf("broken config: %+v", c)
	}
}

func TestDoctorChecksBearerEnvironmentOfSyncedMCPServers(t *testing.T) {
	doctorSyncSetup(t)
	saveGateway(t, "https://gateway.example.com")
	t.Setenv("ZUANO_API_KEY", "x")
	SetOffline(true)
	t.Cleanup(func() { SetOffline(false) })

	checks, _ := doctorChecksByName(t)
	if c, ok := checks["mcp-env:ZUANO_API_KEY"]; ok {
		t.Errorf("the gateway entry authenticates with OAuth and reads no variable: %+v", c)
	}

	writeTestFile(t, filepath.Join(os.Getenv("HOME"), ".config", "relay", "sync", "mcp.json"),
		`{"servers":[{"name":"docs","url":"https://docs.example.com/mcp","bearerEnv":"DOCS_TOKEN"}]}`)
	t.Setenv("DOCS_TOKEN", "x")
	checks, _ = doctorChecksByName(t)
	if c := checks["mcp-env:DOCS_TOKEN"]; !c.OK {
		t.Errorf("set variable: %+v", c)
	}
}

func TestClassifySyncItems(t *testing.T) {
	item := func(status, detail string) agentsync.Item {
		return agentsync.Item{Agent: agentsync.Codex, Kind: agentsync.KindMCP, Scope: agentsync.ScopeUser, Name: "n", Path: "/p", Status: status, Detail: detail}
	}
	cases := map[string]struct {
		items  []agentsync.Item
		ok     bool
		detail string
	}{
		"in sync":        {[]agentsync.Item{item("unchanged", "")}, true, "in sync (1 entries)"},
		"not synced yet": {[]agentsync.Item{item("added", "")}, true, "not synced yet"},
		"partial":        {[]agentsync.Item{item("added", ""), item("unchanged", "")}, false, "out of sync (1 pending)"},
		"updated":        {[]agentsync.Item{item("updated", "")}, false, "out of sync"},
		"removed":        {[]agentsync.Item{item("removed", "")}, false, "out of sync"},
		"edited":         {[]agentsync.Item{item("conflict", "edited since relay wrote it; --force overwrites")}, false, "edited since relay wrote them"},
		"unparseable":    {[]agentsync.Item{item("skipped", "not valid TOML (x); edit it by hand")}, false, "cannot sync"},
		"unsupported":    {[]agentsync.Item{item("unsupported", "no hooks")}, true, "unsupported: no hooks"},
		"user owned":     {[]agentsync.Item{item("conflict", "already defined and not managed by relay"), item("unchanged", "")}, true, "1 of yours block"},
	}
	for name, tc := range cases {
		got := classifySyncItems(tc.items)
		if len(got) != 1 || got[0].OK != tc.ok || !strings.Contains(got[0].Detail, tc.detail) {
			t.Errorf("%s: %+v, want ok=%v detail containing %q", name, got, tc.ok, tc.detail)
		}
	}
}
