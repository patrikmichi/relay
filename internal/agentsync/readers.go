package agentsync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"

	"github.com/patrikmichi/zuano/internal/brand"
)

// ReadAgent reads what the user keeps in one agent's own config and returns
// it in zuano's neutral model, so it can be written to other agents. It reads
// only user-owned content: blocks, files and entries zuano wrote (the ledger
// and the zuano markers say which) are left out, so syncing never feeds
// zuano's own output back in as a source.
//
// kinds limits what is read; env is never read, since it is zuano's agent
// profile and not something the user keeps per agent. Anything that cannot be
// read or carried over is returned as a warning instead of dropped silently.
func ReadAgent(env Env, agent AgentID, scope Scope, kinds []Kind) (Bundle, []string, error) {
	led, err := LoadLedger(env.Home)
	if err != nil {
		return Bundle{}, nil, err
	}
	env = pinWindsurfDir(env, led)
	r := &agentReader{env: env, agent: agent, scope: scope, led: led, ctx: &Ctx{Env: env, Ledger: led}}
	var b Bundle
	for _, k := range kinds {
		switch k {
		case KindRules:
			b.Rules = r.rules()
		case KindCommands:
			b.Commands = r.commands()
		case KindHooks:
			b.Hooks = r.hooks()
		case KindMCP:
			b.MCP = r.mcp()
		}
	}
	return b, r.warns, r.err
}

type agentReader struct {
	env   Env
	agent AgentID
	scope Scope
	led   *Ledger
	ctx   *Ctx
	warns []string
	err   error
}

func (r *agentReader) warn(format string, args ...any) {
	r.warns = append(r.warns, fmt.Sprintf("%s %s: ", r.agent, r.scope)+fmt.Sprintf(format, args...))
}

// file reads one file through the same symlink guard the writers use.
func (r *agentReader) file(path string) (data []byte, real string, ok bool) {
	data, exists, real, err := r.ctx.read(r.scope, path)
	if err != nil {
		var u *unsafePathError
		if errors.As(err, &u) {
			r.warn("%s skipped: %s", path, u.reason)
		} else if r.err == nil {
			r.err = err
		}
		return nil, real, false
	}
	return data, real, exists
}

// dir lists the regular files directly in dir with the given suffix.
func (r *agentReader) dir(dir, suffix string) []string {
	if _, err := r.ctx.guard(r.scope, dir); err != nil {
		r.warn("%s skipped: %v", dir, err)
		return nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		if r.err == nil {
			r.err = err
		}
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), suffix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func (r *agentReader) ledgered(real string) map[string]string { return r.led.Files[real] }

func zuanoOwned(data []byte) bool { return strings.Contains(string(data), Marker) }

// stripBlocks removes every region zuano manages (markdown and TOML block
// markers) and reports whether the markers were balanced.
func stripBlocks(text string) (string, bool) {
	var out []string
	skipping := false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case !skipping && strings.Contains(line, Marker+":begin"):
			skipping = true
		case skipping && strings.Contains(line, Marker+":end"):
			skipping = false
		case !skipping:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n"), !skipping
}

type readFrontmatter struct {
	Description  string `yaml:"description"`
	ArgumentHint string `yaml:"argument-hint"`
	Globs        any    `yaml:"globs"`
	Paths        any    `yaml:"paths"`
	AlwaysApply  *bool  `yaml:"alwaysApply"`
	Trigger      string `yaml:"trigger"`
}

func listOf(v any) []string {
	switch t := v.(type) {
	case string:
		var out []string
		for _, g := range strings.Split(t, ",") {
			if g = strings.TrimSpace(g); g != "" {
				out = append(out, g)
			}
		}
		return out
	case []any:
		var out []string
		for _, g := range t {
			if s, ok := g.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

func splitMarkdown(text string) (readFrontmatter, string, error) {
	var fm readFrontmatter
	text = strings.ReplaceAll(strings.TrimPrefix(text, "\xef\xbb\xbf"), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return fm, strings.TrimSpace(text), nil
	}
	rest := text[4:]
	if strings.HasPrefix(rest, "---") {
		return fm, strings.TrimSpace(afterLine(rest[3:])), nil
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, "", errors.New("unterminated frontmatter")
	}
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return fm, "", fmt.Errorf("invalid frontmatter: %w", err)
	}
	return fm, strings.TrimSpace(afterLine(rest[end+4:])), nil
}

// ---- rules ----

func (r *agentReader) rules() []Rule {
	switch r.agent {
	case Claude:
		return r.blockRule(r.pick(home(".claude", "CLAUDE.md"), project("CLAUDE.md")))
	case Codex:
		return r.blockRule(r.pick(home(".codex", "AGENTS.md"), project("AGENTS.md")))
	case OpenCode:
		return r.blockRule(r.pick(func(e Env) string { return filepath.Join(opencodeDir(e), "AGENTS.md") }, project("AGENTS.md")))
	case Gemini:
		return r.blockRule(r.pick(home(".gemini", "GEMINI.md"), project("GEMINI.md")))
	case Copilot:
		if r.scope == ScopeUser {
			r.warn("rules are configured per repository")
			return nil
		}
		return r.blockRule(project(".github", "copilot-instructions.md")(r.env))
	case Cursor:
		if r.scope == ScopeUser {
			r.warn("Cursor user rules live in Settings, not in a file zuano can read")
			return nil
		}
		return r.ruleFiles(filepath.Join(r.env.Project, ".cursor", "rules"), ".mdc", func(fm readFrontmatter) ([]string, bool) {
			return listOf(fm.Globs), fm.AlwaysApply != nil && *fm.AlwaysApply
		})
	case Windsurf:
		if r.scope == ScopeUser {
			return r.blockRule(filepath.Join(r.env.Home, ".codeium", "windsurf", "memories", "global_rules.md"))
		}
		return r.ruleFiles(filepath.Join(windsurfProjectDir(r.env), "rules"), ".md", func(fm readFrontmatter) ([]string, bool) {
			return listOf(fm.Globs), fm.Trigger == "always_on" || fm.Trigger == ""
		})
	case Cline:
		dir := filepath.Join(r.env.Home, "Documents", "Cline", "Rules")
		if r.scope == ScopeProject {
			dir = filepath.Join(r.env.Project, ".clinerules")
		}
		return r.ruleFiles(dir, ".md", func(fm readFrontmatter) ([]string, bool) {
			g := listOf(fm.Paths)
			return g, len(g) == 0
		})
	}
	return nil
}

func (r *agentReader) pick(user, proj func(Env) string) string {
	if r.scope == ScopeUser {
		return user(r.env)
	}
	return proj(r.env)
}

// blockRule reads a single instructions file as one rule: everything outside
// zuano's own blocks.
func (r *agentReader) blockRule(path string) []Rule {
	data, _, ok := r.file(path)
	if !ok {
		return nil
	}
	text, balanced := stripBlocks(string(data))
	if !balanced {
		r.warn("%s has an unbalanced zuano block marker; read only the text before it", path)
	}
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return nil
	}
	return []Rule{{Name: string(r.agent) + "-instructions", Description: "Instructions from " + filepath.Base(path), Always: true, Body: text}}
}

func (r *agentReader) ruleFiles(dir, ext string, meta func(readFrontmatter) (globs []string, always bool)) []Rule {
	var out []Rule
	for _, path := range r.dir(dir, ext) {
		name := strings.TrimSuffix(filepath.Base(path), ext)
		data, _, ok := r.file(path)
		if !ok {
			continue
		}
		if zuanoOwned(data) {
			continue
		}
		if !safeName.MatchString(name) {
			r.warn("%s skipped: name must match %s", path, safeName)
			continue
		}
		fm, body, err := splitMarkdown(string(data))
		if err != nil {
			r.warn("%s skipped: %v", path, err)
			continue
		}
		globs, always := meta(fm)
		out = append(out, Rule{Name: name, Description: fm.Description, Globs: globs, Always: always, Body: body})
	}
	return out
}

// ---- commands ----

func (r *agentReader) commandDir() (dir, ext string, ok bool) {
	root := r.env.root(r.scope)
	switch r.agent {
	case Claude:
		return filepath.Join(root, ".claude", "commands"), ".md", true
	case Codex:
		if r.scope == ScopeProject {
			r.warn("Codex prompts are read from ~/.codex/prompts only")
			return "", "", false
		}
		return filepath.Join(r.env.Home, ".codex", "prompts"), ".md", true
	case Cursor:
		return filepath.Join(root, ".cursor", "commands"), ".md", true
	case OpenCode:
		if r.scope == ScopeUser {
			return filepath.Join(opencodeDir(r.env), "commands"), ".md", true
		}
		return filepath.Join(r.env.Project, ".opencode", "commands"), ".md", true
	case Windsurf:
		if r.scope == ScopeUser {
			return filepath.Join(r.env.Home, ".codeium", "windsurf", "global_workflows"), ".md", true
		}
		return filepath.Join(windsurfProjectDir(r.env), "workflows"), ".md", true
	case Copilot:
		if r.scope == ScopeUser {
			r.warn("prompt files are configured per repository")
			return "", "", false
		}
		return filepath.Join(r.env.Project, ".github", "prompts"), ".prompt.md", true
	case Gemini:
		return filepath.Join(root, ".gemini", "commands"), ".toml", true
	}
	r.warn("zuano has no command format for this agent")
	return "", "", false
}

func (r *agentReader) commands() []Command {
	dir, ext, ok := r.commandDir()
	if !ok {
		return nil
	}
	var out []Command
	for _, path := range r.dir(dir, ext) {
		name := strings.TrimSuffix(filepath.Base(path), ext)
		data, _, ok := r.file(path)
		if !ok || zuanoOwned(data) {
			continue
		}
		if !safeName.MatchString(name) {
			r.warn("%s skipped: name must match %s", path, safeName)
			continue
		}
		if ext == ".toml" {
			var doc struct {
				Description string `toml:"description"`
				Prompt      string `toml:"prompt"`
			}
			if err := toml.Unmarshal(data, &doc); err != nil {
				r.warn("%s skipped: %v", path, err)
				continue
			}
			out = append(out, Command{Name: name, Description: doc.Description, Body: strings.TrimSpace(strings.ReplaceAll(doc.Prompt, "{{args}}", "$ARGUMENTS"))})
			continue
		}
		fm, body, err := splitMarkdown(string(data))
		if err != nil {
			r.warn("%s skipped: %v", path, err)
			continue
		}
		out = append(out, Command{Name: name, Description: fm.Description, ArgumentHint: fm.ArgumentHint, Body: body})
	}
	return out
}

// ---- hooks ----

func invert(table map[string]string) map[string]string {
	out := map[string]string{}
	for neutral, native := range table {
		out[native] = neutral
	}
	return out
}

func (r *agentReader) hooks() []Hook {
	root := r.env.root(r.scope)
	switch r.agent {
	case Claude:
		return r.jsonHooks(filepath.Join(root, ".claude", "settings.json"), invert(claudeEvents), true, nil)
	case Codex:
		return r.jsonHooks(filepath.Join(root, ".codex", "hooks.json"), invert(claudeEvents), true, nil)
	case Gemini:
		return r.jsonHooks(filepath.Join(root, ".gemini", "settings.json"), invert(geminiEvents), true, invert(geminiTools))
	case Cursor:
		return r.jsonHooks(filepath.Join(root, ".cursor", "hooks.json"), invert(cursorEvents), false, nil)
	}
	r.warn("reading hooks is not supported for this agent (its hooks are not stored in a config file zuano can map back)")
	return nil
}

func numberOf(n *jnode) int {
	if n == nil || n.kind != jScalar {
		return 0
	}
	var v float64
	if json.Unmarshal(n.raw, &v) != nil {
		return 0
	}
	return int(v)
}

// jsonHooks reads {hooks: {Event: [...]}}. grouped is the
// {matcher, hooks:[{command}]} shape; otherwise each item is the command.
func (r *agentReader) jsonHooks(path string, events map[string]string, grouped bool, tools map[string]string) []Hook {
	data, real, ok := r.file(path)
	if !ok {
		return nil
	}
	root, err := parseJSON(data)
	if err != nil || root.kind != jObject {
		r.warn("%s skipped: not plain JSON", path)
		return nil
	}
	hooks := root.get("hooks")
	if hooks == nil || hooks.kind != jObject {
		return nil
	}
	led := r.ledgered(real)
	var out []Hook
	for _, native := range hooks.keys {
		arr := hooks.get(native)
		neutral, known := events[native]
		if arr.kind != jArray {
			continue
		}
		for _, item := range arr.arr {
			if _, mine := led["hook:"+native+":"+item.hash()]; mine {
				continue
			}
			if !known {
				r.warn("hook for event %s skipped: zuano has no neutral event for it", native)
				continue
			}
			matcher := item.get("matcher").str()
			if tools != nil && matcher != "" {
				parts := strings.Split(matcher, "|")
				for i, p := range parts {
					if t, ok := tools[strings.TrimSpace(p)]; ok {
						parts[i] = t
					}
				}
				matcher = strings.Join(parts, "|")
			}
			if !grouped {
				if c := item.get("command").str(); c != "" {
					out = append(out, Hook{Event: neutral, Matcher: matcher, Command: c, Timeout: numberOf(item.get("timeout"))})
				}
				continue
			}
			inner := item.get("hooks")
			if inner == nil || inner.kind != jArray {
				continue
			}
			for _, h := range inner.arr {
				if h.get("name").str() == brand.Name {
					continue
				}
				if t := h.get("type").str(); t != "" && t != "command" {
					r.warn("hook of type %q for %s skipped: only command hooks are carried over", t, native)
					continue
				}
				c := h.get("command").str()
				if c == "" {
					continue
				}
				timeout := numberOf(h.get("timeout"))
				if r.agent == Gemini {
					timeout /= 1000
				}
				out = append(out, Hook{Event: neutral, Matcher: matcher, Command: c, Timeout: timeout})
			}
		}
	}
	return out
}

// ---- MCP servers ----

var (
	bearerRef    = regexp.MustCompile(`^Bearer \$\{(?:env:)?([A-Z_][A-Z0-9_]*)\}$|^Bearer \{env:([A-Z_][A-Z0-9_]*)\}$`)
	envRefNative = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)
	envRefBare   = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)
	secretKey    = regexp.MustCompile(`(?i)key|token|secret|passw`)
)

// neutralRefs rewrites an agent's own environment reference syntax back to
// ${VAR}.
func neutralRefs(s string) string {
	s = envRefNative.ReplaceAllString(s, "$${$1}")
	return envRefBare.ReplaceAllString(s, "$${$1}")
}

func (r *agentReader) mcp() []MCPServer {
	root := r.env.root(r.scope)
	switch r.agent {
	case Claude:
		path := filepath.Join(r.env.Home, ".claude.json")
		if r.scope == ScopeProject {
			path = filepath.Join(r.env.Project, ".mcp.json")
		}
		return r.jsonMCP(path, "mcpServers")
	case Cursor:
		return r.jsonMCP(filepath.Join(root, ".cursor", "mcp.json"), "mcpServers")
	case Gemini:
		return r.jsonMCP(filepath.Join(root, ".gemini", "settings.json"), "mcpServers")
	case OpenCode:
		return r.jsonMCP(opencodeConfigPath(r.env, r.scope), "mcp")
	case Windsurf:
		if r.scope == ScopeProject {
			r.warn("Windsurf reads MCP servers from the user config only")
			return nil
		}
		return r.firstExisting(windsurfMCPPaths(r.env), "mcpServers")
	case Cline:
		if r.scope == ScopeProject {
			r.warn("Cline reads MCP servers from its user settings only")
			return nil
		}
		return r.firstExisting(clineSettingsPaths(r.env), "mcpServers")
	case Copilot:
		if r.scope == ScopeUser {
			r.warn("MCP servers are configured per workspace")
			return nil
		}
		return r.jsonMCP(filepath.Join(r.env.Project, ".vscode", "mcp.json"), "servers")
	case Codex:
		return r.codexMCP(filepath.Join(root, ".codex", "config.toml"))
	}
	return nil
}

func (r *agentReader) firstExisting(paths []string, key string) []MCPServer {
	for _, p := range paths {
		if fileExists(p) {
			return r.jsonMCP(p, key)
		}
	}
	return nil
}

func (r *agentReader) jsonMCP(path, key string) []MCPServer {
	data, real, ok := r.file(path)
	if !ok {
		return nil
	}
	root, err := parseJSON(data)
	if err != nil || root.kind != jObject {
		r.warn("%s skipped: not plain JSON", path)
		return nil
	}
	servers := root.get(key)
	if servers == nil || servers.kind != jObject {
		return nil
	}
	led := r.ledgered(real)
	var out []MCPServer
	for _, name := range servers.keys {
		if _, mine := led["mcp:"+name]; mine {
			continue
		}
		var raw map[string]any
		if json.Unmarshal(servers.get(name).marshal(""), &raw) != nil {
			continue
		}
		if s, ok := r.server(name, raw); ok {
			out = append(out, s)
		}
	}
	return out
}

func stringsOf(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, neutralRefs(s))
		}
	}
	return out
}

func envOf(v any) map[string]string {
	m, _ := v.(map[string]any)
	if len(m) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, e := range m {
		if s, ok := e.(string); ok {
			out[k] = neutralRefs(s)
		}
	}
	return out
}

func (r *agentReader) server(name string, raw map[string]any) (MCPServer, bool) {
	s := MCPServer{Name: name}
	for _, k := range []string{"url", "httpUrl", "serverUrl"} {
		if u, ok := raw[k].(string); ok && u != "" {
			s.URL = neutralRefs(u)
			break
		}
	}
	if headers := envOf(raw["headers"]); s.URL != "" && headers["Authorization"] != "" {
		if m := bearerRef.FindStringSubmatch(headers["Authorization"]); m != nil {
			s.BearerEnv = m[1] + m[2]
		} else {
			r.warn("MCP server %s skipped: its Authorization header is not an environment reference, so copying it would copy a credential", name)
			return s, false
		}
	}
	if s.URL == "" {
		switch cmd := raw["command"].(type) {
		case string:
			s.Command, s.Args = neutralRefs(cmd), stringsOf(raw["args"])
		case []any:
			parts := stringsOf(cmd)
			if len(parts) > 0 {
				s.Command, s.Args = parts[0], parts[1:]
			}
		}
		s.Env = envOf(raw["env"])
		if s.Env == nil {
			s.Env = envOf(raw["environment"])
		}
	}
	for k, v := range s.Env {
		if secretKey.MatchString(k) && !strings.Contains(v, "${") {
			r.warn("MCP server %s: %s is a literal value and is copied as is; use a ${VAR} reference to keep it out of other agents' files", name, k)
		}
	}
	if err := s.Validate(); err != nil {
		r.warn("MCP server %s skipped: %v", name, err)
		return s, false
	}
	return s, true
}

func (r *agentReader) codexMCP(path string) []MCPServer {
	data, _, ok := r.file(path)
	if !ok {
		return nil
	}
	text, balanced := stripBlocks(string(data))
	if !balanced {
		r.warn("%s has an unbalanced zuano block marker; read only the text before it", path)
	}
	var doc struct {
		Servers map[string]map[string]any `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal([]byte(text), &doc); err != nil {
		r.warn("%s skipped: not valid TOML", path)
		return nil
	}
	names := make([]string, 0, len(doc.Servers))
	for n := range doc.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []MCPServer
	for _, name := range names {
		raw := doc.Servers[name]
		if env, ok := raw["env"].(map[string]any); ok {
			raw["env"] = env
		}
		if v, ok := raw["bearer_token_env_var"].(string); ok && v != "" {
			raw["headers"] = map[string]any{"Authorization": "Bearer ${" + v + "}"}
		}
		if s, ok := r.server(name, raw); ok {
			out = append(out, s)
		}
	}
	return out
}
