// Package agentsync writes relay-managed MCP servers, hooks, rules and
// commands into the config files of each supported coding agent without
// touching entries the user owns.
package agentsync

import (
	"fmt"
	"sort"
	"strings"
)

type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

type Kind string

const (
	KindMCP       Kind = "mcp"
	KindEnv       Kind = "env"
	KindHooks     Kind = "hooks"
	KindRules     Kind = "rules"
	KindCommands  Kind = "commands"
	KindSkills    Kind = "skills"
	KindSubagents Kind = "subagents"
)

// FileKinds are the kinds this package writes itself; skills and subagents
// go through the agentport engine.
var FileKinds = []Kind{KindMCP, KindEnv, KindHooks, KindRules, KindCommands}

type AgentID string

const (
	Claude   AgentID = "claude"
	Codex    AgentID = "codex"
	Cursor   AgentID = "cursor"
	OpenCode AgentID = "opencode"
	Gemini   AgentID = "gemini-cli"
	Windsurf AgentID = "windsurf"
	Cline    AgentID = "cline"
	Copilot  AgentID = "copilot"
)

// Marker identifies files and blocks relay wrote. It must appear in the
// first kilobyte of an owned file.
const Marker = "relay-managed"

// Status values reported per managed entry.
const (
	StatusAdded       = "added"
	StatusUpdated     = "updated"
	StatusUnchanged   = "unchanged"
	StatusRemoved     = "removed"
	StatusConflict    = "conflict"
	StatusSkipped     = "skipped"
	StatusWarning     = "warning"
	StatusUnsupported = "unsupported"
)

// Item is the outcome for one managed entry of one agent.
type Item struct {
	Agent  AgentID `json:"agent"`
	Kind   Kind    `json:"kind"`
	Scope  Scope   `json:"scope"`
	Name   string  `json:"name"`
	Path   string  `json:"path,omitempty"`
	Status string  `json:"status"`
	Detail string  `json:"detail,omitempty"`
}

func (i Item) String() string {
	s := fmt.Sprintf("%-10s %-8s %-8s %-12s %s", i.Agent, i.Kind, i.Scope, i.Status, i.Name)
	if i.Path != "" {
		s += "  " + i.Path
	}
	if i.Detail != "" {
		s += "  (" + i.Detail + ")"
	}
	return s
}

// MCPServer is the agent-neutral description of one MCP server. Remote
// servers set URL; local stdio servers set Command.
type MCPServer struct {
	Name      string            `json:"name"`
	URL       string            `json:"url,omitempty"`
	BearerEnv string            `json:"bearerEnv,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

func (s MCPServer) remote() bool { return s.URL != "" }

// Hook events in the agent-neutral vocabulary.
const (
	EventSessionStart     = "session-start"
	EventSessionEnd       = "session-end"
	EventUserPromptSubmit = "user-prompt-submit"
	EventPreToolUse       = "pre-tool-use"
	EventPostToolUse      = "post-tool-use"
	EventStop             = "stop"
	EventSubagentStop     = "subagent-stop"
	EventPreCompact       = "pre-compact"
	EventNotification     = "notification"
)

var hookEvents = map[string]bool{
	EventSessionStart: true, EventSessionEnd: true, EventUserPromptSubmit: true,
	EventPreToolUse: true, EventPostToolUse: true, EventStop: true,
	EventSubagentStop: true, EventPreCompact: true, EventNotification: true,
}

// Hook runs Command when Event fires. Matcher is a tool-name regex for tool
// events and is passed through to agents unchanged except where an agent
// names tools differently. Timeout is in seconds. Agents limits the hook to
// the listed agents when non-empty.
type Hook struct {
	Event   string    `json:"event"`
	Matcher string    `json:"matcher,omitempty"`
	Command string    `json:"command"`
	Timeout int       `json:"timeout,omitempty"`
	Agents  []AgentID `json:"agents,omitempty"`
}

func (h Hook) appliesTo(a AgentID) bool {
	if len(h.Agents) == 0 {
		return true
	}
	for _, x := range h.Agents {
		if x == a {
			return true
		}
	}
	return false
}

// Rule is a persistent instruction. Rules with Globs apply to matching
// files where the agent supports scoping; Always rules load every session.
type Rule struct {
	Name        string
	Description string
	Globs       []string
	Always      bool
	Body        string
}

// Command is a reusable slash command / prompt.
type Command struct {
	Name         string
	Description  string
	ArgumentHint string
	Body         string
}

// Bundle is everything to sync for one scope.
type Bundle struct {
	MCP      []MCPServer
	Env      []EnvVar
	Hooks    []Hook
	Rules    []Rule
	Commands []Command
}

func (b Bundle) count(k Kind) int {
	switch k {
	case KindMCP:
		return len(b.MCP)
	case KindEnv:
		return len(b.Env)
	case KindHooks:
		return len(b.Hooks)
	case KindRules:
		return len(b.Rules)
	case KindCommands:
		return len(b.Commands)
	}
	return 0
}

// ParseAgents resolves a comma-separated agent list. "all" expands to every
// supported agent.
func ParseAgents(values []string) ([]AgentID, error) {
	var out []AgentID
	seen := map[AgentID]bool{}
	for _, raw := range values {
		for _, v := range strings.Split(raw, ",") {
			v = strings.ToLower(strings.TrimSpace(v))
			if v == "" {
				continue
			}
			if v == "all" {
				for _, a := range Agents() {
					if !seen[a.ID] {
						seen[a.ID] = true
						out = append(out, a.ID)
					}
				}
				continue
			}
			if v == "gemini" {
				v = string(Gemini)
			}
			id := AgentID(v)
			if _, ok := agentByID(id); !ok {
				return nil, fmt.Errorf("unknown agent %q (supported: %s, all)", v, strings.Join(agentIDStrings(), ", "))
			}
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out, nil
}

func agentIDStrings() []string {
	var ids []string
	for _, a := range Agents() {
		ids = append(ids, string(a.ID))
	}
	sort.Strings(ids)
	return ids
}
