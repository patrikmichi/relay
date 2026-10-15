package agentsync

import (
	"path/filepath"
	"strings"
	"testing"
)

// seeds are the user-owned files each agent already has; the golden output
// proves relay merged around them.
var seeds = map[AgentID]map[Scope]map[string]string{
	Claude: {
		ScopeUser: {
			".claude.json":          "{\n  \"numStartups\": 3,\n  \"mcpServers\": {\n    \"mine\": {\n      \"command\": \"mine-mcp\"\n    }\n  }\n}\n",
			".claude/settings.json": "{\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(ls:*)\"\n    ]\n  },\n  \"hooks\": {\n    \"PreToolUse\": [\n      {\n        \"matcher\": \"Write\",\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n            \"command\": \"/home/me/own.sh\"\n          }\n        ]\n      }\n    ]\n  }\n}\n",
			".claude/CLAUDE.md":     "# My notes\n\nKeep answers short.\n",
		},
		ScopeProject: {"CLAUDE.md": "# Project\n\nUser notes.\n"},
	},
	Codex: {
		ScopeUser: {
			".codex/config.toml": "model = \"gpt-5\"\n\n[features]\nhooks = true\n",
			".codex/AGENTS.md":   "# My instructions\n",
		},
	},
	Cursor:   {ScopeUser: {".cursor/mcp.json": "{\n  \"mcpServers\": {\n    \"mine\": {\n      \"url\": \"https://example.com/mcp\"\n    }\n  }\n}\n"}},
	OpenCode: {ScopeUser: {".config/opencode/opencode.json": "{\n  \"theme\": \"dark\"\n}\n"}},
	Gemini:   {ScopeUser: {".gemini/settings.json": "{\n  \"theme\": \"GitHub\"\n}\n"}},
	Windsurf: {ScopeUser: {".codeium/windsurf/mcp_config.json": "{\n  \"mcpServers\": {}\n}\n"}},
	Cline:    {ScopeUser: {".cline/data/settings/cline_mcp_settings.json": "{\n  \"mcpServers\": {}\n}\n"}},
	Copilot:  {ScopeProject: {".github/copilot-instructions.md": "Be concise.\n"}},
}

func TestWritersGolden(t *testing.T) {
	for _, agent := range Agents() {
		for _, scope := range []Scope{ScopeUser, ScopeProject} {
			t.Run(string(agent.ID)+"/"+string(scope), func(t *testing.T) {
				env := newEnv(t)
				root := env.root(scope)
				for rel, content := range seeds[agent.ID][scope] {
					writeFile(t, filepath.Join(root, filepath.FromSlash(rel)), content)
				}
				plan := syncAll(t, env, map[Scope]Bundle{scope: fixtureBundle()}, Options{Agents: []AgentID{agent.ID}, Scopes: []Scope{scope}})

				for _, it := range plan.Items {
					if it.Status == StatusConflict || it.Status == StatusSkipped && it.Kind != KindMCP {
						t.Errorf("unexpected %s: %s", it.Status, it)
					}
				}
				assertGolden(t, string(agent.ID)+"-"+string(scope), snapshot(t, root))

				// A second run must be a no-op.
				again, err := Build(env, map[Scope]Bundle{scope: fixtureBundle()}, Options{Agents: []AgentID{agent.ID}, Scopes: []Scope{scope}})
				if err != nil {
					t.Fatal(err)
				}
				if again.Changed() {
					t.Errorf("second sync is not idempotent: %d edits", len(again.Edits))
				}
				for _, it := range again.Items {
					if it.Status == StatusAdded || it.Status == StatusUpdated || it.Status == StatusRemoved {
						t.Errorf("second sync changed %s", it)
					}
				}
			})
		}
	}
}

func TestUnsupportedKindsAreReported(t *testing.T) {
	env := newEnv(t)
	plan, err := Build(env, map[Scope]Bundle{ScopeUser: fixtureBundle()}, Options{Agents: []AgentID{Cline, Cursor}, Scopes: []Scope{ScopeUser}})
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOf(plan.Items, KindHooks, "*"); got != StatusUnsupported {
		t.Errorf("cline hooks: want unsupported, got %q", got)
	}
	if got := statusOf(plan.Items, KindRules, "*"); got != StatusUnsupported {
		t.Errorf("cursor user rules: want unsupported, got %q", got)
	}
}

func TestMatrixCoversEveryAgentKindAndScope(t *testing.T) {
	rows := Matrix(newEnv(t))
	if want := len(Agents()) * 6 * 2; len(rows) != want {
		t.Fatalf("matrix has %d rows, want %d", len(rows), want)
	}
	byKey := map[string]MatrixRow{}
	for _, r := range rows {
		byKey[string(r.Agent)+"/"+string(r.Kind)+"/"+string(r.Scope)] = r
	}
	for key, supported := range map[string]bool{
		"claude/hooks/user":      true,
		"cline/hooks/user":       false,
		"cline/subagents/user":   false,
		"windsurf/subagents/user": false,
		"codex/commands/project": false,
		"cursor/rules/user":      false,
		"gemini-cli/rules/user":  true,
		"copilot/mcp/user":       false,
		"copilot/mcp/project":    true,
	} {
		if byKey[key].Supported != supported {
			t.Errorf("%s: supported=%v, want %v (%s)", key, byKey[key].Supported, supported, byKey[key].Reason)
		}
	}
}

func TestParseAgents(t *testing.T) {
	got, err := ParseAgents([]string{"claude,codex", "gemini", "claude"})
	if err != nil || len(got) != 3 || got[2] != Gemini {
		t.Fatalf("got %v err %v", got, err)
	}
	all, err := ParseAgents([]string{"all"})
	if err != nil || len(all) != len(Agents()) {
		t.Fatalf("all: %v %v", all, err)
	}
	if _, err := ParseAgents([]string{"vim"}); err == nil || !strings.Contains(err.Error(), "supported") {
		t.Fatalf("unknown agent error = %v", err)
	}
}

func TestDetectUsesHome(t *testing.T) {
	env := newEnv(t)
	if got := Detected(env); len(got) != 0 {
		t.Fatalf("empty home detected %v", got)
	}
	writeFile(t, filepath.Join(env.Home, ".codex", "config.toml"), "")
	writeFile(t, filepath.Join(env.Home, ".claude.json"), "{}")
	got := Detected(env)
	if len(got) != 2 || got[0] != Claude || got[1] != Codex {
		t.Fatalf("detected %v", got)
	}
}
