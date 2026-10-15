package agentsync

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

func hookLabel(h Hook) string {
	cmd := strings.Join(strings.Fields(h.Command), " ")
	if len(cmd) > 48 {
		cmd = cmd[:45] + "..."
	}
	return h.Event + ": " + cmd
}

// hookPlan maps neutral hooks onto one agent's event names. Hooks the agent
// cannot express are reported as unsupported instead of dropped silently.
type hookPlan struct {
	events []eventItems
	notes  []Op
}

func buildHooks(m meta, hooks []Hook, mapEvents func(Hook) ([]string, string), item func(Hook) *jnode) hookPlan {
	byEvent := map[string][]namedNode{}
	var plan hookPlan
	for _, h := range hooks {
		if !h.appliesTo(m.agent) {
			continue
		}
		events, why := mapEvents(h)
		if len(events) == 0 {
			plan.notes = append(plan.notes, unsupportedNote(m, hookLabel(h), why))
			continue
		}
		for _, ev := range events {
			byEvent[ev] = append(byEvent[ev], namedNode{hookLabel(h), item(h)})
		}
	}
	names := make([]string, 0, len(byEvent))
	for ev := range byEvent {
		names = append(names, ev)
	}
	sort.Strings(names)
	for _, ev := range names {
		plan.events = append(plan.events, eventItems{ev, byEvent[ev]})
	}
	return plan
}

func eventTable(table map[string]string, agent string) func(Hook) ([]string, string) {
	return func(h Hook) ([]string, string) {
		if ev, ok := table[h.Event]; ok {
			return []string{ev}, ""
		}
		return nil, fmt.Sprintf("%s has no %s event", agent, h.Event)
	}
}

func groupSignature(n *jnode) (string, []string) {
	var cmds []string
	if hooks := n.get("hooks"); hooks != nil {
		for _, h := range hooks.arr {
			if c := h.get("command").str(); c != "" {
				cmds = append(cmds, c)
			}
		}
	}
	return n.get("matcher").str(), cmds
}

func entrySignature(n *jnode) (string, []string) {
	return n.get("matcher").str(), []string{n.get("command").str()}
}

func hooksOps(m meta, path string, plan hookPlan, seed obj, sig func(*jnode) (string, []string)) []Op {
	ops := []Op{&jsonItemsOp{meta: m, path: path, mode: 0o644, objPath: []string{"hooks"}, events: plan.events, signature: sig, seed: seed}}
	return append(ops, plan.notes...)
}

var claudeEvents = map[string]string{
	EventSessionStart: "SessionStart", EventSessionEnd: "SessionEnd", EventUserPromptSubmit: "UserPromptSubmit",
	EventPreToolUse: "PreToolUse", EventPostToolUse: "PostToolUse", EventStop: "Stop",
	EventSubagentStop: "SubagentStop", EventPreCompact: "PreCompact", EventNotification: "Notification",
}

// commandGroup is the {matcher, hooks:[{type, command, timeout}]} shape
// shared by Claude Code, Codex and Gemini CLI.
func commandGroup(h Hook, matcher string, extra obj, timeout int) *jnode {
	cmd := obj{}
	cmd = append(cmd, extra...)
	cmd = append(cmd, kv{"type", "command"}, kv{"command", h.Command})
	if timeout > 0 {
		cmd = append(cmd, kv{"timeout", timeout})
	}
	g := obj{}
	if matcher != "" {
		g = append(g, kv{"matcher", matcher})
	}
	g = append(g, kv{"hooks", []any{cmd}})
	return toNode(g)
}

func claudeHooks(env Env, scope Scope, hooks []Hook) ([]Op, string) {
	m := meta{Claude, KindHooks, scope}
	path := filepath.Join(env.root(scope), ".claude", "settings.json")
	plan := buildHooks(m, hooks, eventTable(claudeEvents, "Claude Code"), func(h Hook) *jnode {
		return commandGroup(h, h.Matcher, nil, h.Timeout)
	})
	return hooksOps(m, path, plan, nil, groupSignature), ""
}

func codexHooks(env Env, scope Scope, hooks []Hook) ([]Op, string) {
	m := meta{Codex, KindHooks, scope}
	path := filepath.Join(env.root(scope), ".codex", "hooks.json")
	table := map[string]string{}
	for k, v := range claudeEvents {
		if k != EventNotification {
			table[k] = v
		}
	}
	plan := buildHooks(m, hooks, eventTable(table, "Codex"), func(h Hook) *jnode {
		return commandGroup(h, h.Matcher, nil, h.Timeout)
	})
	return hooksOps(m, path, plan, nil, groupSignature), ""
}

var geminiEvents = map[string]string{
	EventSessionStart: "SessionStart", EventSessionEnd: "SessionEnd", EventUserPromptSubmit: "BeforeAgent",
	EventPreToolUse: "BeforeTool", EventPostToolUse: "AfterTool", EventStop: "AfterAgent",
	EventPreCompact: "PreCompress", EventNotification: "Notification",
}

var geminiTools = map[string]string{"Bash": "run_shell_command", "Edit": "replace", "MultiEdit": "replace", "Write": "write_file", "Read": "read_file"}

func geminiMatcher(m string) string {
	parts := strings.Split(m, "|")
	for i, p := range parts {
		if t, ok := geminiTools[strings.TrimSpace(p)]; ok {
			parts[i] = t
		}
	}
	return strings.Join(parts, "|")
}

func geminiHooks(env Env, scope Scope, hooks []Hook) ([]Op, string) {
	m := meta{Gemini, KindHooks, scope}
	path := filepath.Join(env.root(scope), ".gemini", "settings.json")
	plan := buildHooks(m, hooks, eventTable(geminiEvents, "Gemini CLI"), func(h Hook) *jnode {
		return commandGroup(h, geminiMatcher(h.Matcher), obj{{"name", "relay"}}, h.Timeout*1000)
	})
	return hooksOps(m, path, plan, nil, groupSignature), ""
}

var cursorEvents = map[string]string{
	EventSessionStart: "sessionStart", EventSessionEnd: "sessionEnd", EventUserPromptSubmit: "beforeSubmitPrompt",
	EventPreToolUse: "preToolUse", EventPostToolUse: "postToolUse", EventStop: "stop",
	EventSubagentStop: "subagentStop", EventPreCompact: "preCompact",
}

func cursorHooks(env Env, scope Scope, hooks []Hook) ([]Op, string) {
	m := meta{Cursor, KindHooks, scope}
	path := filepath.Join(env.root(scope), ".cursor", "hooks.json")
	plan := buildHooks(m, hooks, eventTable(cursorEvents, "Cursor"), func(h Hook) *jnode {
		o := obj{{"command", h.Command}}
		if h.Timeout > 0 {
			o = append(o, kv{"timeout", h.Timeout})
		}
		if h.Matcher != "" {
			o = append(o, kv{"matcher", h.Matcher})
		}
		return toNode(o)
	})
	return hooksOps(m, path, plan, obj{{"version", 1}}, entrySignature), ""
}

var windsurfToolEvents = map[string]string{
	"Bash": "run_command", "Read": "read_code", "Grep": "read_code", "Glob": "read_code",
	"Write": "write_code", "Edit": "write_code", "MultiEdit": "write_code",
}

// windsurfToolHookEvents expands a tool matcher into Windsurf's per-action
// events. A hook without a matcher covers every action.
func windsurfToolHookEvents(prefix, matcher string) ([]string, string) {
	all := []string{"read_code", "write_code", "run_command", "mcp_tool_use"}
	var kinds []string
	if m := strings.TrimSpace(matcher); m == "" || m == "*" || m == ".*" {
		kinds = all
	} else {
		seen := map[string]bool{}
		for _, tok := range strings.Split(m, "|") {
			tok = strings.Trim(strings.TrimSpace(tok), "^$()")
			k, ok := windsurfToolEvents[tok]
			if !ok && strings.HasPrefix(tok, "mcp__") {
				k, ok = "mcp_tool_use", true
			}
			if !ok {
				return nil, fmt.Sprintf("Windsurf has no event for tool matcher %q", tok)
			}
			if !seen[k] {
				seen[k] = true
				kinds = append(kinds, k)
			}
		}
	}
	events := make([]string, len(kinds))
	for i, k := range kinds {
		events[i] = prefix + "_" + k
	}
	return events, ""
}

func windsurfEvents(h Hook) ([]string, string) {
	switch h.Event {
	case EventPreToolUse:
		return windsurfToolHookEvents("pre", h.Matcher)
	case EventPostToolUse:
		return windsurfToolHookEvents("post", h.Matcher)
	case EventUserPromptSubmit:
		return []string{"pre_user_prompt"}, ""
	case EventStop:
		return []string{"post_cascade_response"}, ""
	}
	return nil, "Windsurf has no " + h.Event + " event"
}

func windsurfHooks(env Env, scope Scope, hooks []Hook) ([]Op, string) {
	m := meta{Windsurf, KindHooks, scope}
	path := filepath.Join(env.Home, ".codeium", "windsurf", "hooks.json")
	if scope == ScopeProject {
		dir := ".windsurf"
		if dirExists(filepath.Join(env.Project, ".devin")) && !dirExists(filepath.Join(env.Project, ".windsurf")) {
			dir = ".devin"
		}
		path = filepath.Join(env.Project, dir, "hooks.json")
	}
	plan := buildHooks(m, hooks, windsurfEvents, func(h Hook) *jnode {
		return toNode(obj{{"command", h.Command}, {"show_output", false}})
	})
	return hooksOps(m, path, plan, nil, func(n *jnode) (string, []string) { return "", []string{n.get("command").str()} }), ""
}

func clineHooks(Env, Scope, []Hook) ([]Op, string) {
	return nil, "Cline hooks are SDK plugins with no declarative config file"
}

func copilotHooks(Env, Scope, []Hook) ([]Op, string) {
	return nil, "GitHub Copilot hooks are not supported by relay"
}

var opencodeEvents = map[string]bool{
	EventSessionStart: true, EventSessionEnd: true, EventStop: true,
	EventPreToolUse: true, EventPostToolUse: true, EventPreCompact: true,
}

const opencodePlugin = `// ` + Marker + `: generated by ` + "`relay sync`" + `, edit hooks.json in the relay sync directory instead.
import { spawnSync } from "node:child_process";

const HOOKS = __HOOKS__;

function run(hook, payload) {
  return spawnSync(hook.command, {
    shell: true,
    input: JSON.stringify(payload),
    encoding: "utf8",
    timeout: (hook.timeout || 60) * 1000,
  });
}

function select(event, tool) {
  return HOOKS.filter((h) => h.event === event && (!h.matcher || !tool || new RegExp(h.matcher, "i").test(tool)));
}

export const RelayHooks = async ({ directory }) => ({
  "tool.execute.before": async (input, output) => {
    for (const hook of select("pre-tool-use", input.tool)) {
      const r = run(hook, { hook_event_name: "PreToolUse", tool_name: input.tool, tool_input: output.args, cwd: directory });
      if (r.status === 2) throw new Error((r.stderr || "blocked by a relay hook").trim());
    }
  },
  "tool.execute.after": async (input, output) => {
    for (const hook of select("post-tool-use", input.tool)) {
      run(hook, { hook_event_name: "PostToolUse", tool_name: input.tool, tool_input: output.args, cwd: directory });
    }
  },
  "experimental.session.compacting": async () => {
    for (const hook of select("pre-compact")) run(hook, { hook_event_name: "PreCompact", cwd: directory });
  },
  event: async ({ event }) => {
    const names = { "session.created": "session-start", "session.idle": "stop", "session.deleted": "session-end" };
    const name = names[event.type];
    if (!name) return;
    for (const hook of select(name)) run(hook, { hook_event_name: name, cwd: directory });
  },
});
`

func opencodeHooks(env Env, scope Scope, hooks []Hook) ([]Op, string) {
	m := meta{OpenCode, KindHooks, scope}
	dir := filepath.Join(env.Project, ".opencode", "plugins")
	if scope == ScopeUser {
		dir = filepath.Join(env.Home, ".config", "opencode", "plugins")
	}
	type entry struct {
		Event   string `json:"event"`
		Matcher string `json:"matcher,omitempty"`
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	}
	var selected []entry
	var ops []Op
	for _, h := range hooks {
		if !h.appliesTo(OpenCode) {
			continue
		}
		if !opencodeEvents[h.Event] {
			ops = append(ops, unsupportedNote(m, hookLabel(h), "OpenCode plugins have no "+h.Event+" hook"))
			continue
		}
		selected = append(selected, entry{h.Event, h.Matcher, h.Command, h.Timeout})
	}
	var files []ownedFile
	if len(selected) > 0 {
		raw, _ := json.Marshal(selected)
		files = append(files, ownedFile{"relay-hooks", []byte(strings.Replace(opencodePlugin, "__HOOKS__", string(raw), 1))})
	}
	return append([]Op{&ownedFilesOp{meta: m, dir: dir, ext: ".js", files: files, mode: 0o644}}, ops...), ""
}
