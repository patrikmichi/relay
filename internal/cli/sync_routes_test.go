package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/config"
)

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runCommand(t, SyncCmd(), args...)
	if err != nil {
		t.Fatalf("relay sync %s: %v\n%s", strings.Join(args, " "), err, out)
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
		"settings are not read":   {"--from", "cursor", "--to", "codex", "--kind", "rules"},
		"mixed kinds":             {"--from", "claude", "--to", "codex", "--kind", "skills,rules"},
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
	if !strings.Contains(out, "Routes:") || !strings.Contains(out, "relay -> claude (rules) [default]") {
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

func TestSyncRoutes_TwoRoutesWritingOneFileAreRefused(t *testing.T) {
	syncHome(t)
	mustRun(t, "routes", "add", "--kind", "hooks", "--to", "claude")
	mustRun(t, "routes", "add", "--kind", "env", "--to", "claude")
	// Both kinds live in ~/.claude/settings.json; separate default groups
	// have the same target list, so they merge into one route and succeed.
	mustRun(t, "--dry-run")

	if err := config.UpdateSync(func(s *config.SyncSettings) error {
		s.Routes = append(s.Routes, config.SyncRoute{From: "relay", To: []string{"claude"}, Kinds: []string{"env"}})
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
			{From: "relay", To: []string{"claude"}, Kinds: []string{"rules"}},
			{From: "relay", To: []string{"cursor"}, Kinds: []string{"hooks"}},
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
