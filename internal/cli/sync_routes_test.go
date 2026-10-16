package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/config"
)

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runCommand(t, SyncCmd(), args...)
	if err != nil {
		t.Fatalf("zuano sync %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func TestSyncRoutes_AddListRemoveAndValidation(t *testing.T) {
	syncHome(t)

	mustRun(t, "routes", "add", "--from", "claude", "--to", "codex", "--kind", "skills,subagents")
	mustRun(t, "routes", "add", "--kind", "hooks,rules", "--to", "claude")
	out := mustRun(t, "routes", "list")
	for _, want := range []string{"1. claude -> codex (skills, subagents)", "default hooks -> claude", "default rules -> claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if _, err := runCommand(t, SyncCmd(), "routes", "add", "--from", "claude", "--to", "codex", "--kind", "skills,subagents"); err == nil {
		t.Error("a duplicate route was accepted")
	}

	for name, args := range map[string][]string{
		"skills need a source":    {"--to", "codex", "--kind", "skills"},
		"env is not copied":       {"--from", "cursor", "--to", "codex", "--kind", "env"},
		"copy to itself":          {"--from", "cursor", "--to", "cursor,codex", "--kind", "rules"},
		"unknown source agent":    {"--from", "nope", "--to", "codex", "--kind", "rules"},
		"unknown kind":            {"--to", "codex", "--kind", "bogus"},
		"unknown agent":           {"--to", "nope", "--kind", "rules"},
		"unknown provider":        {"--from", "nope", "--to", "codex", "--kind", "skills"},
		"no targets":              {"--kind", "rules"},
		"route without any kinds": {"--to", "codex"},
	} {
		if _, err := runCommand(t, SyncCmd(), append([]string{"routes", "add"}, args...)...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	mustRun(t, "routes", "remove", "1")
	mustRun(t, "routes", "remove", "rules")
	out = mustRun(t, "routes", "list")
	if strings.Contains(out, "claude -> codex") || strings.Contains(out, "default rules") || !strings.Contains(out, "default hooks") {
		t.Errorf("after removing:\n%s", out)
	}
	if _, err := runCommand(t, SyncCmd(), "routes", "remove", "9"); err == nil {
		t.Error("removing a missing route succeeded")
	}
}

func TestSyncRoutes_PlainSyncFollowsTheRoutesAndFlagsOverrideThem(t *testing.T) {
	home := syncHome(t)
	saveGateway(t, "https://gateway.example.com")
	mustRun(t, "routes", "add", "--kind", "rules", "--to", "claude")

	out := mustRun(t, "--dry-run")
	if !strings.Contains(out, "Routes:") || !strings.Contains(out, "zuano -> claude (rules) [default]") {
		t.Errorf("dry run lacks the per-route plan:\n%s", out)
	}
	if !strings.Contains(out, "no route or default covers") {
		t.Errorf("dry run does not say what is left out:\n%s", out)
	}

	mustRun(t, "--yes")
	if !strings.Contains(readTestFile(t, filepath.Join(home, ".claude", "CLAUDE.md")), "Be terse.") {
		t.Error("the routed rule was not written to claude")
	}
	for _, rel := range []string{".codex/AGENTS.md", ".claude/commands/review.md", ".claude.json"} {
		if _, err := os.Stat(filepath.Join(home, rel)); err == nil {
			t.Errorf("%s was written although no route covers it", rel)
		}
	}

	mustRun(t, "--to", "codex", "--kind", "rules", "--yes")
	if _, err := os.Stat(filepath.Join(home, ".codex", "AGENTS.md")); err != nil {
		t.Errorf("--to/--kind did not run an ordinary sync: %v", err)
	}
}

func TestSyncRoutes_SkillRouteCopiesFromTheNamedProvider(t *testing.T) {
	home := syncHome(t)
	seedClaudeArtifacts(t, home, "Say hello.", "Review carefully.")
	mustRun(t, "routes", "add", "--from", "claude", "--to", "codex", "--kind", "skills")

	mustRun(t, "--yes")
	if _, err := os.Stat(codexSkillFile(t, "hello", "SKILL.md")); err != nil {
		t.Errorf("the skill route did not copy the skill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "agents", "reviewer.md")); err == nil {
		t.Error("subagents were copied although the route names only skills")
	}
}

func TestSyncRoutes_CopySettingsBetweenAgents(t *testing.T) {
	home := syncHome(t)
	saveGateway(t, "https://gateway.example.com")
	// Claude's own content; the zuano-managed blocks zuano itself wrote must not come back.
	writeTestFile(t, filepath.Join(home, ".claude", "CLAUDE.md"), "Always answer in French.\n")
	writeTestFile(t, filepath.Join(home, ".claude", "commands", "ship.md"), "---\ndescription: Ship\n---\nShip $ARGUMENTS\n")
	writeTestFile(t, filepath.Join(home, ".claude", "settings.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/mine/done.sh"}]}]}}`)
	writeTestFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"files":{"type":"stdio","command":"npx","args":["-y","files-mcp"],"env":{"MODE":"fast"}}}}`)
	mustRun(t, "--kind", "rules,hooks", "--to", "claude", "--yes") // zuano-written entries now sit beside the user's

	mustRun(t, "routes", "add", "--from", "claude", "--to", "codex,gemini", "--kind", "rules,commands,hooks,mcp")

	if _, err := runCommand(t, SyncCmd()); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("copying a hook and a stdio server needs confirmation, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "prompts", "ship.md")); err == nil {
		t.Fatal("a refused sync wrote files")
	}
	mustRun(t, "--yes")

	rules := readTestFile(t, filepath.Join(home, ".codex", "AGENTS.md"))
	if !strings.Contains(rules, "Always answer in French.") || strings.Contains(rules, "Be terse.") {
		t.Errorf("codex rules should hold claude's own text only:\n%s", rules)
	}
	if !strings.Contains(readTestFile(t, filepath.Join(home, ".codex", "prompts", "ship.md")), "Ship $ARGUMENTS") {
		t.Error("command not copied to codex")
	}
	if !strings.Contains(readTestFile(t, filepath.Join(home, ".gemini", "commands", "ship.toml")), "{{args}}") {
		t.Error("command not translated for gemini")
	}
	hooks := readTestFile(t, filepath.Join(home, ".codex", "hooks.json"))
	if !strings.Contains(hooks, "/mine/done.sh") || strings.Contains(hooks, "/opt/notify.sh") {
		t.Errorf("codex hooks should hold claude's own hook only:\n%s", hooks)
	}
	if got := readTestFile(t, filepath.Join(home, ".codex", "config.toml")); !strings.Contains(got, "[mcp_servers.files]") || strings.Contains(got, "mcp_servers.zuano") {
		t.Errorf("codex MCP should carry claude's own server only:\n%s", got)
	}

	again := mustRun(t, "--yes")
	if strings.Contains(again, " added ") || strings.Contains(again, " updated ") {
		t.Errorf("second routed sync changed something:\n%s", again)
	}
}

func TestSyncRoutes_CyclesAreRefused(t *testing.T) {
	syncHome(t)
	mustRun(t, "routes", "add", "--from", "claude", "--to", "cursor", "--kind", "rules")
	_, err := runCommand(t, SyncCmd(), "routes", "add", "--from", "cursor", "--to", "claude", "--kind", "rules,commands")
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("a cycle was accepted: %v", err)
	}
	mustRun(t, "routes", "add", "--from", "cursor", "--to", "claude", "--kind", "commands")
}

func TestSyncRoutes_TwoRoutesWritingOneFileAreRefused(t *testing.T) {
	syncHome(t)
	mustRun(t, "routes", "add", "--kind", "hooks", "--to", "claude")
	mustRun(t, "routes", "add", "--kind", "env", "--to", "claude")
	// Both kinds live in ~/.claude/settings.json; separate default groups
	// have the same target list, so they merge into one route and succeed.
	mustRun(t, "--dry-run")

	if err := config.UpdateSync(func(s *config.SyncSettings) error {
		s.Routes = append(s.Routes, config.SyncRoute{From: "zuano", To: []string{"claude"}, Kinds: []string{"env"}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, SyncCmd(), "--dry-run"); err == nil || !strings.Contains(err.Error(), "both write") {
		t.Errorf("overlapping routes = %v, want a both-write error", err)
	}
}

func TestDoctor_ReportsRoutesToAgentsThatAreNotInstalled(t *testing.T) {
	syncHome(t)
	if err := config.UpdateSync(func(s *config.SyncSettings) error {
		s.Routes = []config.SyncRoute{
			{From: "zuano", To: []string{"claude"}, Kinds: []string{"rules"}},
			{From: "zuano", To: []string{"cursor"}, Kinds: []string{"hooks"}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	doctorRouteChecks(func(name string, ok bool, detail string) {
		if ok {
			got[name] = "ok"
		} else {
			got[name] = detail
		}
	})
	if got["sync-route:1"] != "ok" {
		t.Errorf("installed target flagged: %q", got["sync-route:1"])
	}
	if !strings.Contains(got["sync-route:2"], "cursor") {
		t.Errorf("missing agent not reported: %q", got["sync-route:2"])
	}
}
