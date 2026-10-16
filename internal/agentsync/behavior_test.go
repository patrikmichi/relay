package agentsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func userBundle(b Bundle) map[Scope]Bundle { return map[Scope]Bundle{ScopeUser: b} }

func userOpts(agents ...AgentID) Options {
	return Options{Agents: agents, Scopes: []Scope{ScopeUser}}
}

func TestUserOwnedMCPEntryIsNeverClobbered(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude.json")
	own := "{\n  \"mcpServers\": {\n    \"relay\": {\n      \"command\": \"my-own-relay\"\n    }\n  }\n}\n"
	writeFile(t, path, own)

	plan := syncAll(t, env, userBundle(fixtureBundle()), userOpts(Claude))
	if got := statusOf(plan.Items, KindMCP, "relay"); got != StatusConflict {
		t.Fatalf("relay status = %q, want conflict", got)
	}
	if got := statusOf(plan.Items, KindMCP, "docs"); got != StatusAdded {
		t.Fatalf("docs status = %q, want added", got)
	}
	contains(t, readFile(t, path), "my-own-relay")

	forced := userOpts(Claude)
	forced.Force = true
	plan = syncAll(t, env, userBundle(fixtureBundle()), forced)
	if got := statusOf(plan.Items, KindMCP, "relay"); got != StatusUpdated {
		t.Fatalf("forced relay status = %q, want updated", got)
	}
	if strings.Contains(readFile(t, path), "my-own-relay") {
		t.Fatal("force did not replace the entry")
	}
}

func TestManagedEntryEditedByUserIsNotOverwritten(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude.json")
	syncAll(t, env, userBundle(fixtureBundle()), userOpts(Claude))

	edited := strings.Replace(readFile(t, path), "eu", "us", 1)
	writeFile(t, path, edited)

	plan := syncAll(t, env, userBundle(fixtureBundle()), userOpts(Claude))
	if got := statusOf(plan.Items, KindMCP, "docs"); got != StatusConflict {
		t.Fatalf("edited entry status = %q, want conflict", got)
	}
	contains(t, readFile(t, path), `"DOCS_REGION": "us"`)
}

func TestChangedSourceUpdatesManagedEntry(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude.json")
	syncAll(t, env, userBundle(fixtureBundle()), userOpts(Claude))

	b := fixtureBundle()
	b.MCP[0].URL = "https://other.example.com/api/mcp"
	plan := syncAll(t, env, userBundle(b), userOpts(Claude))
	if got := statusOf(plan.Items, KindMCP, "relay"); got != StatusUpdated {
		t.Fatalf("relay status = %q, want updated", got)
	}
	contains(t, readFile(t, path), "other.example.com")
}

func TestStaleManagedEntriesAreRemovedAndUserEntriesKept(t *testing.T) {
	env := newEnv(t)
	writeFile(t, filepath.Join(env.Home, ".claude.json"), "{\n  \"mcpServers\": {\n    \"mine\": {\n      \"command\": \"x\"\n    }\n  }\n}\n")
	writeFile(t, filepath.Join(env.Home, ".claude", "CLAUDE.md"), "# Mine\n")
	syncAll(t, env, userBundle(fixtureBundle()), userOpts(Claude, Codex, Gemini))

	plan := syncAll(t, env, userBundle(Bundle{}), userOpts(Claude, Codex, Gemini))
	removed := map[Kind]int{}
	for _, it := range plan.Items {
		if it.Status == StatusRemoved {
			removed[it.Kind]++
		}
	}
	for _, k := range FileKinds {
		if removed[k] == 0 {
			t.Errorf("no %s entries removed", k)
		}
	}
	got := snapshot(t, env.Home)
	contains(t, got[".claude.json"], `"mine"`)
	if strings.Contains(got[".claude.json"], "relay") {
		t.Errorf("relay entry left behind:\n%s", got[".claude.json"])
	}
	if got[".claude/CLAUDE.md"] != "# Mine\n" {
		t.Errorf("CLAUDE.md = %q", got[".claude/CLAUDE.md"])
	}
	for rel := range got {
		if strings.Contains(rel, "prompts/") || strings.Contains(rel, "commands/") || rel == ".codex/hooks.json" || rel == ".codex/config.toml" || rel == ".codex/AGENTS.md" {
			t.Errorf("managed file %s survived removal", rel)
		}
	}
	if _, ok := got[".gemini/settings.json"]; ok {
		t.Errorf("emptied settings file survived:\n%s", got[".gemini/settings.json"])
	}
}

func TestDryRunWritesNothingAndShowsDiff(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude.json")
	writeFile(t, path, "{\n  \"numStartups\": 1\n}\n")

	plan, err := Build(env, userBundle(fixtureBundle()), userOpts(Claude))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Changed() {
		t.Fatal("plan reports no changes")
	}
	if got := readFile(t, path); got != "{\n  \"numStartups\": 1\n}\n" {
		t.Fatalf("dry run modified the file: %s", got)
	}
	var diff string
	for _, e := range plan.Edits {
		diff += e.Diff()
	}
	contains(t, diff, "+  \"mcpServers\"")
	contains(t, diff, "--- a/")
	if _, err := os.Stat(LedgerPath(env.Home)); err == nil {
		t.Fatal("dry run wrote the ledger")
	}
}

func TestFailedApplyRestoresEarlierFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not block root")
	}
	env := newEnv(t)
	claude := filepath.Join(env.Home, ".claude.json")
	writeFile(t, claude, "{}\n")
	locked := filepath.Join(env.Home, ".gemini")
	writeFile(t, filepath.Join(locked, "settings.json"), "{}\n")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	plan, err := Build(env, userBundle(fixtureBundle()), userOpts(Claude, Gemini))
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(context.Background()); err == nil {
		t.Fatal("expected apply to fail on the read-only directory")
	}
	if got := readFile(t, claude); got != "{}\n" {
		t.Fatalf("claude config was not restored: %s", got)
	}
}

func TestSymlinkedConfigIsEditedInPlace(t *testing.T) {
	env := newEnv(t)
	real := filepath.Join(env.Home, "dotfiles", "claude.json")
	writeFile(t, real, "{}\n")
	link := filepath.Join(env.Home, ".claude.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	plan := syncAll(t, env, userBundle(fixtureBundle()), userOpts(Claude))
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was replaced: %v", err)
	}
	contains(t, readFile(t, real), "mcpServers")
	var reported bool
	for _, it := range plan.Items {
		reported = reported || strings.Contains(it.Detail, "followed symlink "+link)
	}
	if !reported {
		t.Fatalf("followed symlink was not reported: %+v", plan.Items)
	}
}

func TestUnparseableConfigIsSkippedNotOverwritten(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".config", "opencode", "opencode.json")
	jsonc := "{\n  // my theme\n  \"theme\": \"dark\"\n}\n"
	writeFile(t, path, jsonc)
	plan := syncAll(t, env, userBundle(fixtureBundle()), userOpts(OpenCode))
	if got := statusOf(plan.Items, KindMCP, "*"); got != StatusSkipped {
		t.Fatalf("status = %q, want skipped", got)
	}
	if readFile(t, path) != jsonc {
		t.Fatal("unparseable file was modified")
	}
}

func TestTOMLTableDefinedByUserIsNotDuplicated(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".codex", "config.toml")
	own := "[mcp_servers.relay]\ncommand = \"mine\"\n"
	writeFile(t, path, own)
	plan := syncAll(t, env, userBundle(fixtureBundle()), userOpts(Codex))
	if got := statusOf(plan.Items, KindMCP, "mcp_servers.relay"); got != StatusConflict {
		t.Fatalf("relay table status = %q, want conflict", got)
	}
	got := readFile(t, path)
	if strings.Count(got, "[mcp_servers.relay]") != 1 || !strings.Contains(got, "[mcp_servers.docs]") {
		t.Fatalf("unexpected config:\n%s", got)
	}
}

func TestMCPItemsCarryTheServerNameForQuotedKeys(t *testing.T) {
	b := Bundle{MCP: []MCPServer{{Name: "a.b", Command: "npx", Args: []string{"x"}}}}
	for _, agent := range []AgentID{Codex, Claude} {
		env := newEnv(t)
		plan := syncAll(t, env, userBundle(b), Options{Agents: []AgentID{agent}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindMCP}})
		var found bool
		for _, it := range plan.Items {
			if it.Kind == KindMCP && it.Status == StatusAdded {
				found = true
				if it.Server != "a.b" {
					t.Errorf("%s: item %q has server %q, want a.b", agent, it.Name, it.Server)
				}
			}
		}
		if !found {
			t.Errorf("%s: no added MCP item in %v", agent, plan.Items)
		}
	}
}

func TestEnvReferencesAreTranslatedPerAgent(t *testing.T) {
	b := Bundle{MCP: []MCPServer{{Name: "files", Command: "npx", Args: []string{"--root=${HOME}/x"}, Env: map[string]string{"ROOT": "${HOME}"}}}}
	cases := []struct {
		agent AgentID
		path  []string
		want  string
	}{
		{Claude, []string{".claude.json"}, `"ROOT": "${HOME}"`},
		{Gemini, []string{".gemini", "settings.json"}, `"ROOT": "${HOME}"`},
		{Cursor, []string{".cursor", "mcp.json"}, `"ROOT": "${env:HOME}"`},
		{OpenCode, []string{".config", "opencode", "opencode.json"}, `"ROOT": "{env:HOME}"`},
	}
	for _, c := range cases {
		env := newEnv(t)
		syncAll(t, env, userBundle(b), Options{Agents: []AgentID{c.agent}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindMCP}})
		got := readFile(t, filepath.Join(append([]string{env.Home}, c.path...)...))
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: want %s in\n%s", c.agent, c.want, got)
		}
	}
	argWant := map[AgentID]string{Cursor: `--root=${env:HOME}/x`, OpenCode: `--root={env:HOME}/x`}
	for agent, want := range argWant {
		env := newEnv(t)
		syncAll(t, env, userBundle(b), Options{Agents: []AgentID{agent}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindMCP}})
		path := map[AgentID][]string{Cursor: {".cursor", "mcp.json"}, OpenCode: {".config", "opencode", "opencode.json"}}[agent]
		if got := readFile(t, filepath.Join(append([]string{env.Home}, path...)...)); !strings.Contains(got, want) {
			t.Errorf("%s: args lack %s:\n%s", agent, want, got)
		}
	}

	for _, agent := range []AgentID{Codex, Cline} {
		env := newEnv(t)
		plan := syncAll(t, env, userBundle(b), Options{Agents: []AgentID{agent}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindMCP}})
		if got := statusOf(plan.Items, KindMCP, "files"); got != StatusSkipped {
			t.Errorf("%s: status = %q, want skipped", agent, got)
		}
		for _, e := range plan.Edits {
			if strings.Contains(string(e.After), "${HOME}") {
				t.Errorf("%s: wrote a literal reference:\n%s", agent, e.After)
			}
		}
	}
}

func TestMarkdownBlockPreservesSurroundingText(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".gemini", "GEMINI.md")
	writeFile(t, path, "before\n")
	syncAll(t, env, userBundle(fixtureBundle()), userOpts(Gemini))

	withTail := readFile(t, path) + "\nafter\n"
	writeFile(t, path, withTail)
	b := fixtureBundle()
	b.Rules = b.Rules[:1]
	syncAll(t, env, userBundle(b), userOpts(Gemini))
	got := readFile(t, path)
	if !strings.HasPrefix(got, "before\n") || !strings.HasSuffix(got, "after\n") || strings.Contains(got, "table tests") {
		t.Fatalf("unexpected GEMINI.md:\n%s", got)
	}

	writeFile(t, path, "x\n"+mdBegin("rules")+"\nno end\n")
	plan := syncAll(t, env, userBundle(b), userOpts(Gemini))
	if got := statusOf(plan.Items, KindRules, "rules"); got != StatusSkipped {
		t.Fatalf("unbalanced markers status = %q, want skipped", got)
	}
}

func TestUnmanagedFileWithSameNameIsKept(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude", "commands", "review.md")
	writeFile(t, path, "my own review command\n")
	plan := syncAll(t, env, userBundle(fixtureBundle()), userOpts(Claude))
	if got := statusOf(plan.Items, KindCommands, "review"); got != StatusConflict {
		t.Fatalf("status = %q, want conflict", got)
	}
	if readFile(t, path) != "my own review command\n" {
		t.Fatal("user command was overwritten")
	}
}

func TestEquivalentUserHookIsNotDuplicated(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude", "settings.json")
	writeFile(t, path, `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/opt/relay/guard.sh","timeout":99}]}]}}`)
	plan := syncAll(t, env, userBundle(fixtureBundle()), userOpts(Claude))
	if got := statusOf(plan.Items, KindHooks, "pre-tool-use: /opt/relay/guard.sh"); got != StatusSkipped {
		t.Fatalf("status = %q, want skipped", got)
	}
	if strings.Count(readFile(t, path), "guard.sh") != 1 {
		t.Fatalf("hook duplicated:\n%s", readFile(t, path))
	}
}

func TestHookAgentFilterAndUnsupportedEvents(t *testing.T) {
	env := newEnv(t)
	b := Bundle{Hooks: []Hook{
		{Event: EventStop, Command: "/a.sh", Agents: []AgentID{Claude}},
		{Event: EventSubagentStop, Command: "/b.sh"},
	}}
	plan := syncAll(t, env, userBundle(b), userOpts(Claude, Gemini, OpenCode))
	if _, err := os.Stat(filepath.Join(env.Home, ".gemini", "settings.json")); err == nil {
		t.Error("gemini received a hook limited to claude")
	}
	var unsupported []string
	for _, it := range plan.Items {
		if it.Status == StatusUnsupported {
			unsupported = append(unsupported, string(it.Agent))
		}
	}
	if strings.Join(unsupported, ",") != "gemini-cli,opencode" {
		t.Errorf("unsupported subagent-stop agents = %v", unsupported)
	}
}

func TestSharedAgentsMarkdownComposes(t *testing.T) {
	env := newEnv(t)
	bundles := map[Scope]Bundle{ScopeProject: fixtureBundle()}
	plan := syncAll(t, env, bundles, Options{Agents: []AgentID{Codex, OpenCode}, Scopes: []Scope{ScopeProject}})
	got := readFile(t, filepath.Join(env.Project, "AGENTS.md"))
	if strings.Count(got, mdBegin("rules")) != 1 {
		t.Fatalf("AGENTS.md block duplicated:\n%s", got)
	}
	var added int
	for _, it := range plan.Items {
		if it.Kind == KindRules && it.Status == StatusAdded {
			added++
		}
	}
	if added != 1 {
		t.Errorf("rules added %d times, want 1", added)
	}
}

func TestProjectScopeNeedsProjectDir(t *testing.T) {
	env := newEnv(t)
	env.Project = ""
	if _, err := Build(env, map[Scope]Bundle{ScopeProject: {}}, Options{Agents: []AgentID{Claude}, Scopes: []Scope{ScopeProject}}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestWindsurfHookMatcherExpansion(t *testing.T) {
	events, _ := windsurfToolHookEvents("pre", "Bash|Write")
	if strings.Join(events, ",") != "pre_run_command,pre_write_code" {
		t.Fatalf("events = %v", events)
	}
	if events, why := windsurfToolHookEvents("post", "Task"); events != nil || !strings.Contains(why, "Task") {
		t.Fatalf("unknown matcher: %v %q", events, why)
	}
	if events, _ := windsurfToolHookEvents("pre", ""); len(events) != 4 {
		t.Fatalf("empty matcher events = %v", events)
	}
}

func TestWindsurfGlobalRulesLengthWarning(t *testing.T) {
	env := newEnv(t)
	b := Bundle{Rules: []Rule{{Name: "big", Always: true, Body: strings.Repeat("x", windsurfGlobalRulesLimit+10)}}}
	plan, err := Build(env, userBundle(b), userOpts(Windsurf))
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOf(plan.Items, KindRules, "global_rules.md"); got != StatusWarning {
		t.Fatalf("status = %q, want warning", got)
	}
}
