package agentsync

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWindsurfMCPUsesDocumentedDevinPathAndLegacyWhenPresent(t *testing.T) {
	env := newEnv(t)
	syncAll(t, env, userBundle(fixtureBundle()), userOpts(Windsurf))
	devin := filepath.Join(env.Home, ".config", "devin", "mcp_config.json")
	contains(t, readFile(t, devin), "serverUrl")
	if fileExists(filepath.Join(env.Home, ".codeium", "windsurf", "mcp_config.json")) {
		t.Error("legacy path written although Windsurf's directory is absent")
	}

	writeFile(t, filepath.Join(env.Home, ".codeium", "windsurf", "keep"), "")
	syncAll(t, env, userBundle(fixtureBundle()), userOpts(Windsurf))
	contains(t, readFile(t, filepath.Join(env.Home, ".codeium", "windsurf", "mcp_config.json")), "serverUrl")
}

func TestWindsurfMCPHonoursXDGConfigHome(t *testing.T) {
	env := newEnv(t)
	xdg := filepath.Join(t.TempDir(), "xdg")
	env.Getenv = func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return xdg
		}
		return ""
	}
	syncAll(t, env, userBundle(fixtureBundle()), userOpts(Windsurf))
	contains(t, readFile(t, filepath.Join(xdg, "devin", "mcp_config.json")), "serverUrl")
}

func TestWindsurfProjectPrefersDevinDirectory(t *testing.T) {
	env := newEnv(t)
	writeFile(t, filepath.Join(env.Project, ".devin", "keep"), "")
	writeFile(t, filepath.Join(env.Project, ".windsurf", "keep"), "")
	syncAll(t, env, map[Scope]Bundle{ScopeProject: fixtureBundle()}, Options{Agents: []AgentID{Windsurf}, Scopes: []Scope{ScopeProject}})
	for _, rel := range []string{".devin/rules/style.md", ".devin/workflows/review.md", ".devin/hooks.json"} {
		if !fileExists(filepath.Join(env.Project, filepath.FromSlash(rel))) {
			t.Errorf("%s not written", rel)
		}
	}
	if fileExists(filepath.Join(env.Project, ".windsurf", "hooks.json")) {
		t.Error("legacy directory used although .devin exists")
	}
}

func TestCopilotCarriesNoCredentialsAndUsesAgentFrontmatter(t *testing.T) {
	env := newEnv(t)
	plan := syncAll(t, env, map[Scope]Bundle{ScopeProject: fixtureBundle()}, Options{Agents: []AgentID{Copilot}, Scopes: []Scope{ScopeProject}})
	if strings.Contains(readFile(t, filepath.Join(env.Project, ".vscode", "mcp.json")), "Authorization") {
		t.Error("copilot entry carries an Authorization header")
	}
	if got := statusOf(plan.Items, KindMCP, "relay"); got != StatusAdded {
		t.Errorf("relay status = %q", got)
	}
	prompt := readFile(t, filepath.Join(env.Project, ".github", "prompts", "review.prompt.md"))
	contains(t, prompt, "agent: agent")
	if strings.Contains(prompt, "mode:") {
		t.Error("undocumented mode key written")
	}
}
