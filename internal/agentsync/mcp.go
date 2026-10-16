package agentsync

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func jsonMCP(m meta, path string, objPath []string, seed obj, servers []MCPServer, entry func(MCPServer) any) []Op {
	var entries []namedNode
	for _, s := range servers {
		entries = append(entries, namedNode{s.Name, toNode(entry(s))})
	}
	return []Op{&jsonKeysOp{meta: m, path: path, mode: 0o600, objPath: objPath, entries: entries, seed: seed}}
}

func bearer(prefix, env, suffix string) map[string]string {
	if env == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + prefix + env + suffix}
}

// withHeaders appends a headers object when auth headers exist.
func withHeaders(o obj, headers map[string]string) obj {
	if len(headers) > 0 {
		o = append(o, kv{"headers", headers})
	}
	return o
}

func withEnv(o obj, env map[string]string) obj {
	if len(env) > 0 {
		o = append(o, kv{"env", env})
	}
	return o
}

func argsOrEmpty(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

func claudeMCP(env Env, scope Scope, servers []MCPServer) ([]Op, string) {
	m := meta{Claude, KindMCP, scope}
	path := filepath.Join(env.Home, ".claude.json")
	if scope == ScopeProject {
		path = filepath.Join(env.Project, ".mcp.json")
	}
	return jsonMCP(m, path, []string{"mcpServers"}, nil, servers, func(s MCPServer) any {
		if s.remote() {
			return withHeaders(obj{{"type", "http"}, {"url", s.URL}}, bearer("${", s.BearerEnv, "}"))
		}
		return withEnv(obj{{"type", "stdio"}, {"command", s.Command}, {"args", argsOrEmpty(s.Args)}}, s.Env)
	}), ""
}

func cursorMCP(env Env, scope Scope, servers []MCPServer) ([]Op, string) {
	m := meta{Cursor, KindMCP, scope}
	path := filepath.Join(env.root(scope), ".cursor", "mcp.json")
	return jsonMCP(m, path, []string{"mcpServers"}, nil, servers, func(s MCPServer) any {
		if s.remote() {
			return withHeaders(obj{{"url", s.URL}}, bearer("${env:", s.BearerEnv, "}"))
		}
		return withEnv(obj{{"command", s.Command}, {"args", argsOrEmpty(s.Args)}}, s.Env)
	}), ""
}

func geminiMCP(env Env, scope Scope, servers []MCPServer) ([]Op, string) {
	m := meta{Gemini, KindMCP, scope}
	path := filepath.Join(env.root(scope), ".gemini", "settings.json")
	return jsonMCP(m, path, []string{"mcpServers"}, nil, servers, func(s MCPServer) any {
		if s.remote() {
			return withHeaders(obj{{"httpUrl", s.URL}}, bearer("${", s.BearerEnv, "}"))
		}
		return withEnv(obj{{"command", s.Command}, {"args", argsOrEmpty(s.Args)}}, s.Env)
	}), ""
}

func opencodeMCP(env Env, scope Scope, servers []MCPServer) ([]Op, string) {
	m := meta{OpenCode, KindMCP, scope}
	path := filepath.Join(env.Project, "opencode.json")
	if scope == ScopeUser {
		path = filepath.Join(env.Home, ".config", "opencode", "opencode.json")
	}
	seed := obj{{"$schema", "https://opencode.ai/config.json"}}
	return jsonMCP(m, path, []string{"mcp"}, seed, servers, func(s MCPServer) any {
		if s.remote() {
			return withHeaders(obj{{"type", "remote"}, {"url", s.URL}, {"enabled", true}}, bearer("{env:", s.BearerEnv, "}"))
		}
		o := obj{{"type", "local"}, {"command", append([]string{s.Command}, s.Args...)}, {"enabled", true}}
		if len(s.Env) > 0 {
			o = append(o, kv{"environment", s.Env})
		}
		return o
	}), ""
}

func windsurfMCP(env Env, scope Scope, servers []MCPServer) ([]Op, string) {
	if scope == ScopeProject {
		return nil, "Windsurf reads MCP servers from the user config only"
	}
	m := meta{Windsurf, KindMCP, scope}
	var ops []Op
	for _, path := range windsurfMCPPaths(env) {
		ops = append(ops, jsonMCP(m, path, []string{"mcpServers"}, nil, servers, func(s MCPServer) any {
			if s.remote() {
				return withHeaders(obj{{"serverUrl", s.URL}}, bearer("${env:", s.BearerEnv, "}"))
			}
			return withEnv(obj{{"command", s.Command}, {"args", argsOrEmpty(s.Args)}}, s.Env)
		})...)
	}
	return ops, ""
}

// windsurfMCPPaths returns the config files to write. The current docs
// place mcp_config.json under the Devin config directory
// ($XDG_CONFIG_HOME/devin, default ~/.config/devin); installs that predate
// the rename keep theirs in ~/.codeium/windsurf, which is also written when
// that directory exists so either generation of the app sees the servers.
func windsurfMCPPaths(env Env) []string {
	xdg := env.getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(env.Home, ".config")
	}
	paths := []string{filepath.Join(xdg, "devin", "mcp_config.json")}
	if dirExists(filepath.Join(env.Home, ".codeium", "windsurf")) {
		paths = append(paths, filepath.Join(env.Home, ".codeium", "windsurf", "mcp_config.json"))
	}
	return paths
}

// clineSettingsPaths returns the MCP settings files for the Cline CLI and,
// when present, the VS Code extension.
func clineSettingsPaths(env Env) []string {
	if p := env.getenv("CLINE_MCP_SETTINGS_PATH"); p != "" {
		return []string{p}
	}
	paths := []string{filepath.Join(env.Home, ".cline", "data", "settings", "cline_mcp_settings.json")}
	vscode := filepath.Join(env.Home, ".config", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings")
	if dirExists(vscode) {
		paths = append(paths, filepath.Join(vscode, "cline_mcp_settings.json"))
	}
	return paths
}

func clineMCP(env Env, scope Scope, servers []MCPServer) ([]Op, string) {
	if scope == ScopeProject {
		return nil, "Cline reads MCP servers from its user settings only"
	}
	m := meta{Cline, KindMCP, scope}
	var ops []Op
	for _, path := range clineSettingsPaths(env) {
		ops = append(ops, jsonMCP(m, path, []string{"mcpServers"}, nil, servers, func(s MCPServer) any {
			if s.remote() {
				return obj{{"type", "streamableHttp"}, {"url", s.URL}, {"disabled", false}}
			}
			return withEnv(obj{{"command", s.Command}, {"args", argsOrEmpty(s.Args)}}, s.Env)
		})...)
	}
	for _, s := range servers {
		if s.remote() {
			ops = append(ops, noteOp{m.item(s.Name, "", StatusSkipped, "Cline cannot read tokens from the environment; no Authorization header written, authenticate the server in Cline")})
		}
	}
	return ops, ""
}

func copilotMCP(env Env, scope Scope, servers []MCPServer) ([]Op, string) {
	if scope == ScopeUser {
		return nil, "GitHub Copilot MCP servers are configured per workspace"
	}
	m := meta{Copilot, KindMCP, scope}
	path := filepath.Join(env.Project, ".vscode", "mcp.json")
	ops := jsonMCP(m, path, []string{"servers"}, nil, servers, func(s MCPServer) any {
		if s.remote() {
			return obj{{"type", "http"}, {"url", s.URL}}
		}
		return withEnv(obj{{"type", "stdio"}, {"command", s.Command}, {"args", argsOrEmpty(s.Args)}}, s.Env)
	})
	// VS Code documents ${input:...} variables for secrets, not environment
	// references, so remote entries carry no header and VS Code asks for
	// authorization itself.
	for _, s := range servers {
		if s.remote() {
			ops = append(ops, noteOp{m.item(s.Name, "", StatusSkipped, "VS Code cannot read tokens from the environment; no Authorization header written, authorize the server in VS Code")})
		}
	}
	return ops, ""
}

var tomlBareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlKey(k string) string {
	if tomlBareKey.MatchString(k) {
		return k
	}
	return tomlStr(k)
}

func tomlStr(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = tomlStr(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func codexMCP(env Env, scope Scope, servers []MCPServer) ([]Op, string) {
	m := meta{Codex, KindMCP, scope}
	path := filepath.Join(env.root(scope), ".codex", "config.toml")
	var tables []tomlTable
	for _, s := range servers {
		header := "mcp_servers." + tomlKey(s.Name)
		var b strings.Builder
		fmt.Fprintf(&b, "[%s]\n", header)
		if s.remote() {
			fmt.Fprintf(&b, "url = %s\n", tomlStr(s.URL))
			if s.BearerEnv != "" {
				fmt.Fprintf(&b, "bearer_token_env_var = %s\n", tomlStr(s.BearerEnv))
			}
		} else {
			fmt.Fprintf(&b, "command = %s\n", tomlStr(s.Command))
			fmt.Fprintf(&b, "args = %s\n", tomlList(argsOrEmpty(s.Args)))
			if len(s.Env) > 0 {
				keys := make([]string, 0, len(s.Env))
				for k := range s.Env {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				fmt.Fprintf(&b, "\n[%s.env]\n", header)
				for _, k := range keys {
					fmt.Fprintf(&b, "%s = %s\n", tomlKey(k), tomlStr(s.Env[k]))
				}
			}
		}
		tables = append(tables, tomlTable{header: header, path: []string{"mcp_servers", s.Name}, body: b.String()})
	}
	return []Op{&tomlBlockOp{meta: m, path: path, id: "mcp", tables: tables}}, ""
}
