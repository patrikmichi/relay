package agentsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const claudeConfig = `{
  "numStartups": 4,
  "mcpServers": {
    "shadow": {"command": "npx", "args": ["-y", "@evil/shadow"]},
    "mine": {"type": "http", "url": "https://docs.example.com/mcp"}
  }
}
`

func quarantine(t *testing.T, env Env, targets ...QuarantineTarget) []Quarantined {
	t.Helper()
	plan, results, err := PlanQuarantine(env, targets)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	return results
}

func TestQuarantineTakesOnlyThePersonsOwnServerOutOfAJSONConfig(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude.json")
	writeFile(t, path, claudeConfig)
	got := quarantine(t, env, QuarantineTarget{Agent: Claude, Scope: ScopeUser, Name: "shadow"})
	if len(got) != 1 || got[0].Status != StatusQuarantined || got[0].Format != FormatJSON || got[0].Hash == "" || !strings.Contains(got[0].Entry, "@evil/shadow") {
		t.Fatalf("results = %+v", got)
	}
	after := readFile(t, path)
	if strings.Contains(after, "shadow") || !strings.Contains(after, `"mine"`) || !strings.Contains(after, `"numStartups": 4`) {
		t.Errorf("config after:\n%s", after)
	}

	// Restoring puts the server back.
	plan, restored, err := PlanRestore(env, got)
	if err != nil || len(restored) != 1 || restored[0].Status != StatusRestored {
		t.Fatalf("restore: %+v %v", restored, err)
	}
	if err := plan.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	back := readFile(t, path)
	if !strings.Contains(back, "@evil/shadow") || !strings.Contains(back, `"mine"`) {
		t.Errorf("config after restore:\n%s", back)
	}
	// ... and does not put it twice.
	_, again, err := PlanRestore(env, got)
	if err != nil || again[0].Status != StatusSkipped {
		t.Errorf("a second restore: %+v %v", again, err)
	}
}

func TestQuarantineLeavesWhatItShouldNotTouch(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude.json")
	writeFile(t, path, claudeConfig)
	// A server zuano wrote is not shadow.
	led, err := LoadLedger(env.Home)
	if err != nil {
		t.Fatal(err)
	}
	led.set(path, "mcp:mine", "abc")
	if err := led.Save(); err != nil {
		t.Fatal(err)
	}
	probe := func(t QuarantineTarget) Quarantined {
		_, res, err := PlanQuarantine(env, []QuarantineTarget{t})
		if err != nil {
			panic(err)
		}
		return res[0]
	}
	if r := probe(QuarantineTarget{Agent: Claude, Scope: ScopeUser, Name: "mine"}); r.Status != StatusManaged {
		t.Errorf("managed = %+v", r)
	}
	if r := probe(QuarantineTarget{Agent: Claude, Scope: ScopeUser, Name: "missing"}); r.Status != StatusNotFound {
		t.Errorf("missing = %+v", r)
	}
	if r := probe(QuarantineTarget{Agent: Claude, Scope: ScopeUser, Name: "shadow", Hash: "0000000000000000"}); r.Status != StatusChanged {
		t.Errorf("changed = %+v", r)
	}
	good := probe(QuarantineTarget{Agent: Claude, Scope: ScopeUser, Name: "shadow"})
	if r := probe(QuarantineTarget{Agent: Claude, Scope: ScopeUser, Name: "shadow", Hash: good.Hash}); r.Status != StatusQuarantined {
		t.Errorf("the hash it was judged by: %+v", r)
	}
	if r := probe(QuarantineTarget{Agent: Windsurf, Scope: ScopeProject, Name: "x"}); r.Status != StatusNotFound {
		t.Errorf("an agent without project servers: %+v", r)
	}
	if r := probe(QuarantineTarget{Agent: Cursor, Scope: ScopeUser, Name: "x"}); r.Status != StatusNotFound {
		t.Errorf("no config file: %+v", r)
	}
	if after := readFile(t, path); after != claudeConfig {
		t.Errorf("planning changed the file:\n%s", after)
	}
}

const codexConfig = `model = "gpt"

[mcp_servers.shadow]
command = "npx"
args = ["-y", "@evil/shadow"]

[mcp_servers.shadow.env]
TOKEN = "x"

[mcp_servers.keep]
url = "https://docs.example.com/mcp"

[profiles.fast]
model = "mini"
`

func TestQuarantineCutsATOMLServerAndItsSubTables(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".codex", "config.toml")
	writeFile(t, path, codexConfig)
	got := quarantine(t, env, QuarantineTarget{Agent: Codex, Scope: ScopeUser, Name: "shadow"})
	if got[0].Status != StatusQuarantined || got[0].Format != FormatTOML || !strings.Contains(got[0].Entry, "[mcp_servers.shadow.env]") {
		t.Fatalf("results = %+v", got)
	}
	after := readFile(t, path)
	if strings.Contains(after, "shadow") || !strings.Contains(after, "[mcp_servers.keep]") || !strings.Contains(after, "[profiles.fast]") || !strings.Contains(after, `model = "gpt"`) {
		t.Errorf("config after:\n%s", after)
	}
	plan, restored, err := PlanRestore(env, got)
	if err != nil || restored[0].Status != StatusRestored {
		t.Fatalf("restore: %+v %v", restored, err)
	}
	if err := plan.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	back := readFile(t, path)
	if !strings.Contains(back, "[mcp_servers.shadow]") || !strings.Contains(back, `TOKEN = "x"`) || !strings.Contains(back, "[mcp_servers.keep]") {
		t.Errorf("config after restore:\n%s", back)
	}
	if _, again, _ := PlanRestore(env, got); again[0].Status != StatusSkipped {
		t.Errorf("a second restore: %+v", again)
	}
}

func TestQuarantineWorksForTheOtherAgents(t *testing.T) {
	env := newEnv(t)
	files := map[AgentID]struct{ path, body string }{
		Cursor:   {filepath.Join(env.Home, ".cursor", "mcp.json"), `{"mcpServers": {"shadow": {"command": "x"}}}`},
		Gemini:   {filepath.Join(env.Home, ".gemini", "settings.json"), `{"mcpServers": {"shadow": {"command": "x"}}}`},
		OpenCode: {opencodeConfigPath(env, ScopeUser), `{"mcp": {"shadow": {"type": "local", "command": ["x"]}}}`},
	}
	var targets []QuarantineTarget
	for id, f := range files {
		writeFile(t, f.path, f.body)
		targets = append(targets, QuarantineTarget{Agent: id, Scope: ScopeUser, Name: "shadow"})
	}
	writeFile(t, filepath.Join(env.Project, ".vscode", "mcp.json"), `{"servers": {"shadow": {"command": "x"}}}`)
	targets = append(targets, QuarantineTarget{Agent: Copilot, Scope: ScopeProject, Name: "shadow"})
	for _, q := range quarantine(t, env, targets...) {
		if q.Status != StatusQuarantined {
			t.Errorf("%s: %+v", q.Target.Agent, q)
		}
	}
	for id, f := range files {
		if strings.Contains(readFile(t, f.path), "shadow") {
			t.Errorf("%s still has the server", id)
		}
	}
}

func TestQuarantineRefusesAProjectSymlinkAndKeepsWhatItCouldNotRead(t *testing.T) {
	env := newEnv(t)
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	writeFile(t, outside, `{"mcpServers": {"shadow": {"command": "x"}}}`)
	link := filepath.Join(env.Project, ".mcp.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks are not available:", err)
	}
	_, res, err := PlanQuarantine(env, []QuarantineTarget{{Agent: Claude, Scope: ScopeProject, Name: "shadow"}})
	if err != nil || res[0].Status != StatusSkipped {
		t.Errorf("a symlink below the project: %+v %v", res, err)
	}
	if !strings.Contains(readFile(t, outside), "shadow") {
		t.Error("the file behind the link changed")
	}
	writeFile(t, filepath.Join(env.Home, ".gemini", "settings.json"), `not json`)
	if _, _, err := PlanQuarantine(env, []QuarantineTarget{{Agent: Gemini, Scope: ScopeUser, Name: "shadow"}}); err == nil {
		t.Error("a config that is not JSON was edited")
	}
}
