package agentsync

import (
	"path/filepath"
	"strings"
	"testing"
)

func readAs(t *testing.T, env Env, agent AgentID, scope Scope, kinds ...Kind) (Bundle, []string) {
	t.Helper()
	b, warns, err := ReadAgent(env, agent, scope, kinds)
	if err != nil {
		t.Fatalf("ReadAgent: %v", err)
	}
	return b, warns
}

func names[T any](items []T, name func(T) string) string {
	var out []string
	for _, i := range items {
		out = append(out, name(i))
	}
	return strings.Join(out, ",")
}

func TestReadRulesSkipsRelayBlocksAndFiles(t *testing.T) {
	env := newEnv(t)
	claude := filepath.Join(env.Home, ".claude", "CLAUDE.md")
	writeFile(t, claude, "My own notes.\n")
	syncAll(t, env, userBundle(fixtureBundle()), Options{Agents: []AgentID{Claude}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindRules}})
	if !strings.Contains(readFile(t, claude), Marker) {
		t.Fatal("setup: no relay block written")
	}
	b, _ := readAs(t, env, Claude, ScopeUser, KindRules)
	if len(b.Rules) != 1 || b.Rules[0].Body != "My own notes." || strings.Contains(b.Rules[0].Body, "Prefer small functions") {
		t.Fatalf("rules = %+v, want only the user's text", b.Rules)
	}

	cursorDir := filepath.Join(env.Project, ".cursor", "rules")
	writeFile(t, filepath.Join(cursorDir, "mine.mdc"), "---\ndescription: Mine\nglobs: \"**/*.go,**/*.mod\"\nalwaysApply: false\n---\nUse tabs.\n")
	syncAll(t, env, map[Scope]Bundle{ScopeProject: fixtureBundle()}, Options{Agents: []AgentID{Cursor}, Scopes: []Scope{ScopeProject}, Kinds: []Kind{KindRules}})
	b, _ = readAs(t, env, Cursor, ScopeProject, KindRules)
	if len(b.Rules) != 1 || b.Rules[0].Name != "mine" || b.Rules[0].Always || len(b.Rules[0].Globs) != 2 || b.Rules[0].Body != "Use tabs." {
		t.Fatalf("cursor rules = %+v, want only the user's mine.mdc", b.Rules)
	}
}

func TestReadCommandsFromMarkdownAndGeminiTOML(t *testing.T) {
	env := newEnv(t)
	writeFile(t, filepath.Join(env.Home, ".claude", "commands", "ship.md"), "---\ndescription: Ship it\nargument-hint: \"[env]\"\n---\nShip $ARGUMENTS now.\n")
	writeFile(t, filepath.Join(env.Home, ".gemini", "commands", "triage.toml"), "description = \"Triage\"\nprompt = \"\"\"\nTriage {{args}}\n\"\"\"\n")
	syncAll(t, env, userBundle(fixtureBundle()), Options{Agents: []AgentID{Claude, Gemini}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindCommands}})

	b, _ := readAs(t, env, Claude, ScopeUser, KindCommands)
	if got := names(b.Commands, func(c Command) string { return c.Name }); got != "ship" || b.Commands[0].ArgumentHint != "[env]" || b.Commands[0].Description != "Ship it" {
		t.Fatalf("claude commands = %+v", b.Commands)
	}
	b, _ = readAs(t, env, Gemini, ScopeUser, KindCommands)
	if len(b.Commands) != 1 || b.Commands[0].Name != "triage" || b.Commands[0].Body != "Triage $ARGUMENTS" {
		t.Fatalf("gemini commands = %+v", b.Commands)
	}
}

func TestReadHooksSkipsTheOnesRelayWrote(t *testing.T) {
	env := newEnv(t)
	settings := filepath.Join(env.Home, ".claude", "settings.json")
	writeFile(t, settings, `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/mine/guard.sh","timeout":5}]}]}}`)
	syncAll(t, env, userBundle(fixtureBundle()), Options{Agents: []AgentID{Claude}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindHooks}})

	b, _ := readAs(t, env, Claude, ScopeUser, KindHooks)
	if len(b.Hooks) != 1 || b.Hooks[0].Command != "/mine/guard.sh" || b.Hooks[0].Event != EventPreToolUse || b.Hooks[0].Matcher != "Bash" || b.Hooks[0].Timeout != 5 {
		t.Fatalf("hooks = %+v, want only the user's guard", b.Hooks)
	}

	writeFile(t, filepath.Join(env.Home, ".gemini", "settings.json"), `{"hooks":{"BeforeTool":[{"matcher":"run_shell_command","hooks":[{"type":"command","command":"/mine/g.sh","timeout":3000}]}]}}`)
	b, _ = readAs(t, env, Gemini, ScopeUser, KindHooks)
	if len(b.Hooks) != 1 || b.Hooks[0].Event != EventPreToolUse || b.Hooks[0].Matcher != "Bash" || b.Hooks[0].Timeout != 3 {
		t.Fatalf("gemini hooks = %+v", b.Hooks)
	}

	_, warns := readAs(t, env, Windsurf, ScopeUser, KindHooks)
	if len(warns) == 0 {
		t.Error("an unreadable hook format must be reported")
	}
}

func TestReadMCPNormalisesEachAgentsSyntaxAndSkipsRelayEntries(t *testing.T) {
	env := newEnv(t)
	writeFile(t, filepath.Join(env.Home, ".cursor", "mcp.json"), `{"mcpServers":{"files":{"command":"npx","args":["-y","@acme/files","--root=${env:HOME}"],"env":{"ROOT":"${env:HOME}"}},"docs":{"url":"https://docs.example.com/mcp","headers":{"Authorization":"Bearer ${env:DOCS_TOKEN}"}}}}`)
	syncAll(t, env, userBundle(fixtureBundle()), Options{Agents: []AgentID{Cursor}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindMCP}})

	b, _ := readAs(t, env, Cursor, ScopeUser, KindMCP)
	got := map[string]MCPServer{}
	for _, s := range b.MCP {
		got[s.Name] = s
	}
	if len(got) != 2 || got["relay"].Name != "" {
		t.Fatalf("servers = %+v, want only files and docs", b.MCP)
	}
	if f := got["files"]; f.Command != "npx" || f.Env["ROOT"] != "${HOME}" || f.Args[2] != "--root=${HOME}" {
		t.Errorf("files = %+v", f)
	}
	if d := got["docs"]; d.URL != "https://docs.example.com/mcp" || d.BearerEnv != "DOCS_TOKEN" {
		t.Errorf("docs = %+v", d)
	}
}

func TestReadMCPCodexTOMLAndLiteralHeaders(t *testing.T) {
	env := newEnv(t)
	writeFile(t, filepath.Join(env.Home, ".codex", "config.toml"), "[mcp_servers.mine]\ncommand = \"uvx\"\nargs = [\"tool\"]\n\n[mcp_servers.mine.env]\nAPI_KEY = \"literal\"\n")
	syncAll(t, env, userBundle(fixtureBundle()), Options{Agents: []AgentID{Codex}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindMCP}})
	b, warns := readAs(t, env, Codex, ScopeUser, KindMCP)
	if len(b.MCP) != 1 || b.MCP[0].Name != "mine" || b.MCP[0].Command != "uvx" {
		t.Fatalf("codex servers = %+v", b.MCP)
	}
	if !strings.Contains(strings.Join(warns, "\n"), "API_KEY is a literal value") {
		t.Errorf("a copied literal secret was not flagged: %v", warns)
	}

	writeFile(t, filepath.Join(env.Home, ".gemini", "settings.json"), `{"mcpServers":{"leaky":{"httpUrl":"https://x.example.com/mcp","headers":{"Authorization":"Bearer sk-live-123"}}}}`)
	b, warns = readAs(t, env, Gemini, ScopeUser, KindMCP)
	if len(b.MCP) != 0 || !strings.Contains(strings.Join(warns, "\n"), "credential") {
		t.Errorf("a literal Authorization header was copied: %+v %v", b.MCP, warns)
	}
}
