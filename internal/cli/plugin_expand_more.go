package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/patrikmichi/zuano/internal/agentport"
	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/plugins"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// expandSubagents installs a bundle's subagents (agents/<name>.md, Claude
// format) through the agentport agent engine, the same one `zuano agent
// install` uses.
func (s *pluginSession) expandSubagents(agentID string, vp *verifiedPackage, prior *plugins.Install, in *plugins.Install) error {
	var names []string
	for _, p := range vp.pkg.Paths() {
		if rest, ok := strings.CutPrefix(p, "agents/"); ok && !strings.Contains(rest, "/") && strings.HasSuffix(rest, ".md") {
			names = append(names, rest)
		}
	}
	if len(names) == 0 {
		return nil
	}
	dst, ok := agentport.AgentAdapterByID(agentport.ProviderID(agentID))
	if !ok {
		fmt.Fprintf(s.err, "note: %s has no subagent format zuano can write; the bundle's subagents are not expanded\n", agentID)
		return nil
	}
	claude, ok := agentport.AgentAdapterByID("claude")
	if !ok {
		return errors.New("the Claude subagent format is unavailable")
	}
	tmp, err := os.MkdirTemp("", "zuano-plugin-agents-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, file := range names {
		path := filepath.Join(tmp, file)
		if err := writeFileAtomic(path, vp.pkg.Files["agents/"+file], 0o600); err != nil {
			return err
		}
		src, err := claude.Load(path)
		if err != nil {
			fmt.Fprintf(s.err, "warning: subagent %s is not expanded: %v\n", textsafe.Clean(file), err)
			continue
		}
		plan, err := agentport.MigrateAgent(src, dst, agentport.ScopeUser)
		if err != nil {
			fmt.Fprintf(s.err, "warning: subagent %s is not expanded: %v\n", textsafe.Clean(file), err)
			continue
		}
		if agentport.HasSecurityLoss(plan.Loss) {
			fmt.Fprintf(s.err, "warning: subagent %s is not expanded: a permission field cannot be represented on %s\n", textsafe.Clean(file), agentID)
			continue
		}
		plan.Overwrite = prior != nil && slices.Contains(prior.Subagents, src.Name)
		if err := agentport.PreflightWriteAgent(plan); err != nil {
			fmt.Fprintf(s.err, "warning: subagent %s is not expanded: %v\n", textsafe.Clean(file), err)
			continue
		}
		if err := agentport.WriteAgent(plan); err != nil {
			return fmt.Errorf("write subagent %s: %w", file, err)
		}
		in.Subagents = append(in.Subagents, src.Name)
		for rel, data := range plan.Files {
			in.Files = append(in.Files, plugins.FileRef{Path: filepath.Join(plan.TargetPaths, filepath.FromSlash(rel)), SHA256: sha256Hex(data)})
		}
	}
	return nil
}

// claudeHookEvents maps the event names bundles use to zuano's.
var claudeHookEvents = map[string]string{
	"SessionStart": agentsync.EventSessionStart, "SessionEnd": agentsync.EventSessionEnd,
	"UserPromptSubmit": agentsync.EventUserPromptSubmit, "PreToolUse": agentsync.EventPreToolUse,
	"PostToolUse": agentsync.EventPostToolUse, "Stop": agentsync.EventStop,
	"SubagentStop": agentsync.EventSubagentStop, "PreCompact": agentsync.EventPreCompact,
	"Notification": agentsync.EventNotification,
}

// bundleHooks reads hooks/hooks.json (Claude's layout) into hooks for one
// agent. A command that starts with ./ or ${CLAUDE_PLUGIN_ROOT}/ names a file
// in the bundle, so it is pointed at the copy written under root.
func bundleHooks(pkg *plugins.Package, agentID, root string) ([]agentsync.Hook, []string) {
	raw, ok := pkg.Files["hooks/hooks.json"]
	if !ok {
		return nil, nil
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil, []string{"hooks/hooks.json is not valid JSON; its hooks are not added"}
	}
	events := make([]string, 0, len(doc.Hooks))
	for e := range doc.Hooks {
		events = append(events, e)
	}
	sort.Strings(events)
	var out []agentsync.Hook
	var warns []string
	for _, e := range events {
		neutral, known := claudeHookEvents[e]
		if !known {
			warns = append(warns, "hook event "+e+" is not one zuano knows; skipped")
			continue
		}
		for _, group := range doc.Hooks[e] {
			for _, h := range group.Hooks {
				if h.Type != "command" || strings.TrimSpace(h.Command) == "" {
					continue
				}
				out = append(out, agentsync.Hook{
					Event: neutral, Matcher: group.Matcher, Command: bundleCommandPath(h.Command, root),
					Timeout: h.Timeout, Agents: []agentsync.AgentID{agentsync.AgentID(agentID)},
				})
			}
		}
	}
	return out, warns
}

// bundleCommandPath rewrites a leading bundle-relative path in a hook command
// to the absolute, shell-quoted location of the copy zuano wrote.
func bundleCommandPath(cmd, root string) string {
	first, rest, _ := strings.Cut(cmd, " ")
	var rel string
	switch {
	case strings.HasPrefix(first, "${CLAUDE_PLUGIN_ROOT}/"):
		rel = strings.TrimPrefix(first, "${CLAUDE_PLUGIN_ROOT}/")
	case strings.HasPrefix(first, "./"):
		rel = strings.TrimPrefix(first, "./")
	default:
		return cmd
	}
	abs := "'" + strings.ReplaceAll(filepath.Join(root, filepath.FromSlash(rel)), "'", `'\''`) + "'"
	if rest == "" {
		return abs
	}
	return abs + " " + rest
}

func hookRef(h agentsync.Hook) plugins.HookRef {
	return plugins.HookRef{Event: h.Event, Matcher: h.Matcher, Command: h.Command}
}

// mergeHooks returns the hooks.json document with this plugin's earlier hooks
// (drop) replaced by add. Every other member of the file is left as it was.
func mergeHooks(before []byte, drop []plugins.HookRef, add []agentsync.Hook, agentID string) ([]byte, error) {
	doc := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(before))) > 0 {
		if err := json.Unmarshal(before, &doc); err != nil {
			return nil, fmt.Errorf("hooks.json is not valid JSON: %w", err)
		}
	}
	var entries []json.RawMessage
	if raw, ok := doc["hooks"]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("hooks.json: hooks is not a list: %w", err)
		}
	}
	kept := entries[:0:0]
	for _, e := range entries {
		var h agentsync.Hook
		if json.Unmarshal(e, &h) == nil && slices.Contains(drop, hookRef(h)) && slices.Contains(h.Agents, agentsync.AgentID(agentID)) {
			continue
		}
		kept = append(kept, e)
	}
	for _, h := range add {
		raw, err := json.Marshal(h)
		if err != nil {
			return nil, err
		}
		kept = append(kept, raw)
	}
	if len(kept) == 0 {
		delete(doc, "hooks")
	} else {
		raw, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		doc["hooks"] = raw
	}
	if len(doc) == 0 {
		return nil, nil
	}
	return json.MarshalIndent(doc, "", "  ")
}

func (s *pluginSession) hooksSourcePath() string {
	return filepath.Join(agentsync.SourceDir(agentsync.Env{Home: s.home, Project: s.home}, agentsync.ScopeUser), "hooks.json")
}

func (s *pluginSession) bundleFilesRoot(plugin string) string {
	return filepath.Join(s.home, ".config", "zuano", "plugins", "files", plugin)
}

// expandHooks adds a bundle's hooks to the zuano sync directory, limited to the
// one agent, and syncs them there. This runs commands on the machine, so it
// goes through the same confirmation as `zuano sync`: --allow-hooks, or a yes
// at a terminal. Declined, the sync directory is put back.
func (s *pluginSession) expandHooks(agentID string, vp *verifiedPackage, t *pluginTarget, in *plugins.Install) error {
	root := s.bundleFilesRoot(t.name)
	hooks, warns := bundleHooks(vp.pkg, agentID, root)
	for _, w := range warns {
		fmt.Fprintf(s.err, "warning: %s\n", textsafe.Clean(w))
	}
	var prior []plugins.HookRef
	if t.prior != nil {
		prior = t.prior.Hooks
	}
	if len(hooks) == 0 && len(prior) == 0 {
		return nil
	}

	srcPath := s.hooksSourcePath()
	before, err := os.ReadFile(srcPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	merged, err := mergeHooks(before, prior, hooks, agentID)
	if err != nil {
		return err
	}
	var scripts []plugins.FileRef
	restore := func() {
		for _, f := range scripts {
			_ = os.Remove(f.Path)
		}
		if before == nil {
			_ = os.Remove(srcPath)
		} else {
			_ = writeFileAtomic(srcPath, before, 0o600)
		}
	}
	if len(hooks) > 0 {
		for _, name := range vp.pkg.Paths() {
			if !strings.HasPrefix(name, "hooks/") || name == "hooks/hooks.json" {
				continue
			}
			target, err := safeJoin(root, name)
			if err != nil {
				restore()
				return err
			}
			mode := os.FileMode(0o644)
			if isExecutablePath(name) {
				mode = 0o755
			}
			if err := writeFileAtomic(target, vp.pkg.Files[name], mode); err != nil {
				restore()
				return err
			}
			scripts = append(scripts, plugins.FileRef{Path: target, SHA256: sha256Hex(vp.pkg.Files[name])})
		}
	}
	if merged == nil {
		err = os.Remove(srcPath)
	} else {
		err = writeFileAtomic(srcPath, merged, 0o600)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		restore()
		return err
	}

	run, err := planAgentSync(agentSyncOpts{to: []string{agentID}, scopes: []string{"user"}, kinds: []string{"hooks"}})
	if err == nil {
		err = confirmAgentWrite(s.cmd, run, "plugin install", t.opts.allowHooks)
	}
	if err == nil {
		err = run.apply(s.ctx)
	}
	if err != nil {
		restore()
		return fmt.Errorf("the bundle's hooks were not added to %s: %w", agentID, err)
	}
	in.Hooks = nil
	for _, h := range hooks {
		in.Hooks = append(in.Hooks, hookRef(h))
	}
	in.Files = append(in.Files, scripts...)
	return nil
}

// removeHooks takes a plugin's hooks out of the sync directory and the agent.
func (s *pluginSession) removeHooks(agentID string, in plugins.Install) error {
	if len(in.Hooks) == 0 {
		return nil
	}
	srcPath := s.hooksSourcePath()
	before, err := os.ReadFile(srcPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	merged, err := mergeHooks(before, in.Hooks, nil, agentID)
	if err != nil {
		return err
	}
	if merged == nil {
		err = os.Remove(srcPath)
	} else {
		err = writeFileAtomic(srcPath, merged, 0o600)
	}
	if err != nil {
		return err
	}
	return s.syncKinds(agentID, []string{"hooks"})
}
