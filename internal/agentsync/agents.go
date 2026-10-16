package agentsync

import (
	"os"
	"path/filepath"
)

type builder[T any] func(env Env, scope Scope, items []T) (ops []Op, unsupported string)

// Agent describes how one coding agent stores each artifact kind. A nil
// builder, or one returning a reason, means the kind is unsupported there.
type Agent struct {
	ID       AgentID
	Name     string
	Detect   func(Env) bool
	MCP      builder[MCPServer]
	Env      builder[EnvVar]
	Hooks    builder[Hook]
	Rules    builder[Rule]
	Commands builder[Command]
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Agents lists every supported agent in a stable order.
func Agents() []Agent {
	return []Agent{
		{
			ID: Claude, Name: "Claude Code",
			Detect: func(e Env) bool {
				return dirExists(filepath.Join(e.Home, ".claude")) || fileExists(filepath.Join(e.Home, ".claude.json"))
			},
			MCP: refAware(Claude, claudeMCP), Env: claudeEnv, Hooks: claudeHooks, Rules: claudeRules, Commands: claudeCommands,
		},
		{
			ID: Codex, Name: "Codex",
			Detect: func(e Env) bool { return dirExists(filepath.Join(e.Home, ".codex")) },
			MCP:    refAware(Codex, codexMCP), Env: codexEnv, Hooks: codexHooks, Rules: codexRules, Commands: codexCommands,
		},
		{
			ID: Cursor, Name: "Cursor",
			Detect: func(e Env) bool { return dirExists(filepath.Join(e.Home, ".cursor")) },
			MCP:    refAware(Cursor, cursorMCP), Hooks: cursorHooks, Rules: cursorRules, Commands: cursorCommands,
		},
		{
			ID: OpenCode, Name: "OpenCode",
			Detect: func(e Env) bool { return dirExists(opencodeDir(e)) },
			MCP:    refAware(OpenCode, opencodeMCP), Hooks: opencodeHooks, Rules: opencodeRules, Commands: opencodeCommands,
		},
		{
			ID: Gemini, Name: "Gemini CLI",
			Detect: func(e Env) bool { return dirExists(filepath.Join(e.Home, ".gemini")) },
			MCP:    refAware(Gemini, geminiMCP), Hooks: geminiHooks, Rules: geminiRules, Commands: geminiCommands,
		},
		{
			ID: Windsurf, Name: "Windsurf",
			Detect: func(e Env) bool {
				return dirExists(filepath.Join(e.Home, ".codeium", "windsurf")) || dirExists(filepath.Join(e.Home, ".windsurf"))
			},
			MCP: refAware(Windsurf, windsurfMCP), Hooks: windsurfHooks, Rules: windsurfRules, Commands: windsurfCommands,
		},
		{
			ID: Cline, Name: "Cline",
			Detect: func(e Env) bool {
				return dirExists(filepath.Join(e.Home, ".cline")) || dirExists(filepath.Join(e.Home, "Documents", "Cline"))
			},
			MCP: refAware(Cline, clineMCP), Hooks: clineHooks, Rules: clineRules, Commands: clineCommands,
		},
		{
			ID: Copilot, Name: "GitHub Copilot",
			Detect: func(e Env) bool {
				return e.Project != "" && (fileExists(filepath.Join(e.Project, ".github", "copilot-instructions.md")) || fileExists(filepath.Join(e.Project, ".vscode", "mcp.json")))
			},
			MCP: refAware(Copilot, copilotMCP), Hooks: copilotHooks, Rules: copilotRules, Commands: copilotCommands,
		},
	}
}

func agentByID(id AgentID) (Agent, bool) {
	for _, a := range Agents() {
		if a.ID == id {
			return a, true
		}
	}
	return Agent{}, false
}

// Detected returns the agents installed for env.
func Detected(env Env) []AgentID {
	var out []AgentID
	for _, a := range Agents() {
		if a.Detect(env) {
			out = append(out, a.ID)
		}
	}
	return out
}

// Support reports why kind is unsupported for agent at scope, or "" when
// zuano can write it.
func Support(env Env, id AgentID, kind Kind, scope Scope) string {
	a, ok := agentByID(id)
	if !ok {
		return "unknown agent"
	}
	var reason string
	switch kind {
	case KindMCP:
		reason = probe(a.MCP, env, scope)
	case KindEnv:
		reason = probe(a.Env, env, scope)
	case KindHooks:
		reason = probe(a.Hooks, env, scope)
	case KindRules:
		reason = probe(a.Rules, env, scope)
	case KindCommands:
		reason = probe(a.Commands, env, scope)
	}
	return reason
}

func probe[T any](b builder[T], env Env, scope Scope) string {
	if b == nil {
		return "not supported by zuano"
	}
	_, reason := b(env, scope, nil)
	return reason
}

// noteOp reports a fixed outcome, used for entries an agent cannot take.
type noteOp struct{ item Item }

func (o noteOp) Run(*Ctx) ([]Item, error) { return []Item{o.item}, nil }

func unsupportedNote(m meta, name, reason string) Op {
	return noteOp{Item{Agent: m.agent, Kind: m.kind, Scope: m.scope, Name: name, Status: StatusUnsupported, Detail: reason}}
}
