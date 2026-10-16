package agentsync

import (
	"path/filepath"
	"strings"
)

// EnvVar is an environment variable an agent passes to the processes it
// starts, such as the shell commands it runs.
type EnvVar struct {
	Name  string
	Value string
}

func claudeEnv(env Env, scope Scope, vars []EnvVar) ([]Op, string) {
	if scope == ScopeProject {
		return nil, "the environment of Claude Code is set in the user settings"
	}
	m := meta{Claude, KindEnv, scope}
	path := filepath.Join(env.Home, ".claude", "settings.json")
	var entries []namedNode
	for _, v := range vars {
		entries = append(entries, namedNode{v.Name, toNode(v.Value)})
	}
	return []Op{&jsonKeysOp{meta: m, path: path, mode: 0o600, objPath: []string{"env"}, entries: entries}}, ""
}

func codexEnv(env Env, scope Scope, vars []EnvVar) ([]Op, string) {
	if scope == ScopeProject {
		return nil, "the shell environment of Codex is set in the user config"
	}
	m := meta{Codex, KindEnv, scope}
	path := filepath.Join(env.Home, ".codex", "config.toml")
	var tables []tomlTable
	if len(vars) > 0 {
		var b strings.Builder
		b.WriteString("[shell_environment_policy.set]\n")
		for _, v := range vars {
			b.WriteString(tomlKey(v.Name) + " = " + tomlStr(v.Value) + "\n")
		}
		tables = append(tables, tomlTable{header: "shell_environment_policy.set", path: []string{"shell_environment_policy", "set"}, body: b.String()})
	}
	return []Op{&tomlBlockOp{meta: m, path: path, id: "env", tables: tables}}, ""
}
