package agentsync

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func envBundle() Bundle { return Bundle{Env: []EnvVar{{Name: "RELAY_AGENT", Value: "1"}}} }

func envOpts() Options {
	return Options{Agents: []AgentID{Claude, Codex}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindEnv}}
}

func TestEnvKeepsTheUsersOwnVariables(t *testing.T) {
	env := newEnv(t)
	settings := filepath.Join(env.Home, ".claude", "settings.json")
	writeFile(t, settings, "{\n  \"env\": {\n    \"EDITOR\": \"vim\"\n  }\n}\n")

	syncAll(t, env, userBundle(envBundle()), envOpts())
	got := readFile(t, settings)
	contains(t, got, `"EDITOR": "vim"`)
	contains(t, got, `"RELAY_AGENT": "1"`)

	syncAll(t, env, userBundle(Bundle{}), envOpts())
	got = readFile(t, settings)
	contains(t, got, `"EDITOR": "vim"`)
	if strings.Contains(got, "RELAY_AGENT") {
		t.Errorf("RELAY_AGENT left behind:\n%s", got)
	}
}

func TestEnvVariableTheUserSetDifferentlyIsNotOverwritten(t *testing.T) {
	env := newEnv(t)
	settings := filepath.Join(env.Home, ".claude", "settings.json")
	writeFile(t, settings, "{\n  \"env\": {\n    \"RELAY_AGENT\": \"0\"\n  }\n}\n")

	plan := syncAll(t, env, userBundle(envBundle()), envOpts())
	if got := statusOf(plan.Items, KindEnv, "RELAY_AGENT"); got != StatusConflict {
		t.Fatalf("status = %q, want conflict", got)
	}
	contains(t, readFile(t, settings), `"RELAY_AGENT": "0"`)
}

func TestEnvIsUserScopeOnly(t *testing.T) {
	env := newEnv(t)
	plan, err := Build(env, map[Scope]Bundle{ScopeProject: envBundle()}, Options{Agents: []AgentID{Claude, Codex}, Scopes: []Scope{ScopeProject}, Kinds: []Kind{KindEnv}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Edits) != 0 {
		t.Errorf("project scope wrote %d files", len(plan.Edits))
	}
	if got := statusOf(plan.Items, KindEnv, "*"); got != StatusUnsupported {
		t.Errorf("status = %q, want unsupported", got)
	}
}

func TestForceNeverDuplicatesAUserTOMLTable(t *testing.T) {
	env := newEnv(t)
	config := filepath.Join(env.Home, ".codex", "config.toml")
	writeFile(t, config, "[mcp_servers.relay]\ncommand = \"mine\"\n\n[shell_environment_policy.set]\nFOO = \"bar\"\n")

	opts := userOpts(Codex)
	opts.Force = true
	opts.Kinds = []Kind{KindMCP, KindEnv}
	plan := syncAll(t, env, userBundle(fixtureBundle()), opts)
	if got := statusOf(plan.Items, KindEnv, "shell_environment_policy.set"); got != StatusConflict {
		t.Errorf("env status = %q, want conflict", got)
	}
	got := readFile(t, config)
	for _, header := range []string{"[mcp_servers.relay]", "[shell_environment_policy.set]"} {
		if strings.Count(got, header) > 1 {
			t.Errorf("%s defined twice:\n%s", header, got)
		}
	}
	var parsed map[string]any
	if err := toml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("the forced sync wrote invalid TOML: %v\n%s", err, got)
	}
}

func TestCodexEnvTableTheUserDefinedIsReportedNotReplaced(t *testing.T) {
	env := newEnv(t)
	config := filepath.Join(env.Home, ".codex", "config.toml")
	writeFile(t, config, "[shell_environment_policy.set]\nFOO = \"bar\"\n")

	plan := syncAll(t, env, userBundle(envBundle()), Options{Agents: []AgentID{Codex}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindEnv}})
	if got := statusOf(plan.Items, KindEnv, "shell_environment_policy.set"); got != StatusConflict {
		t.Fatalf("status = %q, want conflict", got)
	}
	if strings.Contains(readFile(t, config), "RELAY_AGENT") {
		t.Error("the user's table was modified")
	}
}
