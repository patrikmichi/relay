package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/patrikmichi/zuano/internal/agentport"
	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/plugins"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

var expandedName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// expandInto writes the parts of a verified bundle that an agent has no plugin
// system for into the agent's own configuration: skills through the same
// engine as `zuano skill install`, and commands, rules and MCP servers through
// the zuano sync directory, followed by a sync of those kinds to this agent.
// Subagents and hooks are not expanded; the bundle's native package carries
// them where the agent supports it. Everything written is recorded in in.
func (s *pluginSession) expandInto(agentID string, t *pluginTarget, in *plugins.Install) error {
	vp, err := s.verified(t.name, t.version, t.channel, platformAgentPlugins)
	if err != nil {
		return err
	}
	env := agentsync.Env{Home: s.home, Project: s.home}
	srcDir := agentsync.SourceDir(env, agentsync.ScopeUser)
	owned := map[string]string{}
	if t.prior != nil {
		for _, f := range t.prior.Files {
			owned[f.Path] = f.SHA256
		}
	}
	var kinds []string

	// Skills.
	skillNames := topLevelNames(vp.pkg, "skills/")
	if len(skillNames) > 0 {
		in.Skills, in.Files = nil, nil
		if err := s.expandSkills(agentID, vp, skillNames, t.prior, in); err != nil {
			return err
		}
	}

	// Commands and rules.
	for _, kind := range []struct{ dir, kind string }{{"commands", "commands"}, {"rules", "rules"}} {
		wrote := false
		for _, name := range vp.pkg.Paths() {
			rest, ok := strings.CutPrefix(name, kind.dir+"/")
			if !ok || strings.Contains(rest, "/") {
				continue
			}
			base := strings.TrimSuffix(strings.TrimSuffix(rest, ".md"), ".mdc")
			if base == rest {
				continue
			}
			file := t.name + "--" + base
			if !expandedName.MatchString(file) {
				fmt.Fprintf(s.err, "warning: not expanding %s: the name is too long or has unusual characters\n", textsafe.Clean(name))
				continue
			}
			target := filepath.Join(srcDir, kind.dir, file+".md")
			data := vp.pkg.Files[name]
			if cur, err := os.ReadFile(target); err == nil && sha256Hex(cur) != sha256Hex(data) {
				if want, ok := owned[target]; !ok || sha256Hex(cur) != want {
					fmt.Fprintf(s.err, "warning: kept %s, which is not zuano's to replace\n", textsafe.Clean(target))
					continue
				}
			}
			if err := writeFileAtomic(target, data, 0o600); err != nil {
				return err
			}
			in.Files = append(in.Files, plugins.FileRef{Path: target, SHA256: sha256Hex(data)})
			wrote = true
		}
		if wrote {
			kinds = append(kinds, kind.kind)
		}
	}

	// MCP servers.
	servers, warns := bundleMCPServers(vp.pkg, t.name, t.opts.allowHooks)
	for _, w := range warns {
		fmt.Fprintf(s.err, "warning: %s\n", textsafe.Clean(w))
	}
	if len(servers) > 0 || (t.prior != nil && len(t.prior.MCP) > 0) {
		src, err := agentsync.LoadMCPSource(env, agentsync.ScopeUser)
		if err != nil {
			return err
		}
		var prior []string
		if t.prior != nil {
			prior = t.prior.MCP
		}
		src.Servers = slices.DeleteFunc(src.Servers, func(sv agentsync.MCPServer) bool { return slices.Contains(prior, sv.Name) })
		in.MCP = nil
		for _, sv := range servers {
			if slices.ContainsFunc(src.Servers, func(o agentsync.MCPServer) bool { return o.Name == sv.Name }) {
				fmt.Fprintf(s.err, "warning: not adding MCP server %s: you already have one with that name\n", textsafe.Clean(sv.Name))
				continue
			}
			src.Servers = append(src.Servers, sv)
			in.MCP = append(in.MCP, sv.Name)
		}
		if err := agentsync.SaveMCPSource(env, agentsync.ScopeUser, src); err != nil {
			return err
		}
		kinds = append(kinds, "mcp")
	}

	if dropped := notExpanded(vp.pkg); dropped != "" {
		fmt.Fprintf(s.err, "note: %s are not expanded into %s\n", dropped, agentID)
	}
	return s.syncKinds(agentID, kinds)
}

// syncKinds fans the given kinds of the zuano sync directory out to one agent.
func (s *pluginSession) syncKinds(agentID string, kinds []string) error {
	if len(kinds) == 0 {
		return nil
	}
	run, err := planAgentSync(agentSyncOpts{to: []string{agentID}, scopes: []string{"user"}, kinds: kinds})
	if err != nil {
		return fmt.Errorf("sync to %s: %w", agentID, err)
	}
	if err := run.apply(s.ctx); err != nil {
		return fmt.Errorf("sync to %s: %w", agentID, err)
	}
	for _, it := range run.items {
		if it.Status == agentsync.StatusConflict || it.Status == agentsync.StatusUnsupported || it.Status == agentsync.StatusSkipped {
			fmt.Fprintf(s.err, "  %s\n", textsafe.Clean(it.String()))
		}
	}
	return nil
}

func (s *pluginSession) expandSkills(agentID string, vp *verifiedPackage, names []string, prior *plugins.Install, in *plugins.Install) error {
	adapter, ok := agentport.AdapterByID(agentport.ProviderID(agentID))
	if !ok {
		fmt.Fprintf(s.err, "note: %s has no skill directory zuano can write; the bundle's skills are not expanded\n", agentID)
		return nil
	}
	tmp, err := os.MkdirTemp("", "zuano-plugin-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, name := range names {
		for _, p := range vp.pkg.Paths() {
			if rest, ok := strings.CutPrefix(p, "skills/"+name+"/"); ok {
				target, err := safeJoin(filepath.Join(tmp, name), rest)
				if err != nil {
					return err
				}
				if err := writeFileAtomic(target, vp.pkg.Files[p], 0o600); err != nil {
					return err
				}
			}
		}
		skill, err := agentport.LoadGenericSkill(filepath.Join(tmp, name))
		if err != nil {
			fmt.Fprintf(s.err, "warning: skill %s is not expanded: %v\n", textsafe.Clean(name), err)
			continue
		}
		plan, err := agentport.Migrate(skill, adapter, agentport.ScopeUser)
		if err != nil {
			fmt.Fprintf(s.err, "warning: skill %s is not expanded: %v\n", textsafe.Clean(name), err)
			continue
		}
		if agentport.HasSecurityLoss(plan.Loss) {
			fmt.Fprintf(s.err, "warning: skill %s is not expanded: a permission field cannot be represented on %s\n", textsafe.Clean(name), agentID)
			continue
		}
		if plan.HasDropped() {
			fmt.Fprintf(s.err, "note: skill %s loses %s on %s\n", textsafe.Clean(name), lossSummary(plan.Loss), agentID)
		}
		plan.Overwrite = prior != nil && slices.Contains(prior.Skills, skill.Name)
		if err := agentport.PreflightWrite(plan); err != nil {
			fmt.Fprintf(s.err, "warning: skill %s is not expanded: %v\n", textsafe.Clean(name), err)
			continue
		}
		if err := agentport.Write(plan); err != nil {
			return fmt.Errorf("write skill %s: %w", name, err)
		}
		in.Skills = append(in.Skills, skill.Name)
		for rel, data := range plan.Files {
			in.Files = append(in.Files, plugins.FileRef{Path: filepath.Join(plan.TargetPaths, filepath.FromSlash(rel)), SHA256: sha256Hex(data)})
		}
	}
	return nil
}

// topLevelNames lists the directories directly under prefix in a package.
func topLevelNames(pkg *plugins.Package, prefix string) []string {
	seen := map[string]bool{}
	for p := range pkg.Files {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			if name, _, found := strings.Cut(rest, "/"); found && name != "" {
				seen[name] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// notExpanded names the parts of a package that expansion leaves alone.
func notExpanded(pkg *plugins.Package) string {
	var parts []string
	for _, dir := range []string{"agents", "hooks"} {
		for p := range pkg.Files {
			if strings.HasPrefix(p, dir+"/") {
				parts = append(parts, dir)
				break
			}
		}
	}
	return strings.Join(parts, " and ")
}

// bundleMCPServers reads the MCP servers a package declares. A server that
// runs a local command is taken only with --allow-hooks.
func bundleMCPServers(pkg *plugins.Package, plugin string, allowCommands bool) ([]agentsync.MCPServer, []string) {
	var servers []agentsync.MCPServer
	var warns []string
	for _, file := range []string{".mcp.json", "mcp.json"} {
		data, ok := pkg.Files[file]
		if !ok {
			continue
		}
		var doc struct {
			McpServers map[string]rawMCP `json:"mcpServers"`
			Servers    map[string]rawMCP `json:"servers"`
		}
		if json.Unmarshal(data, &doc) != nil {
			warns = append(warns, file+" is not valid JSON; its MCP servers are not added")
			continue
		}
		all := doc.McpServers
		if len(all) == 0 {
			all = doc.Servers
		}
		names := make([]string, 0, len(all))
		for n := range all {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			r := all[n]
			sv := agentsync.MCPServer{Name: plugin + "-" + n, URL: r.URL, Command: r.Command, Args: r.Args, Env: r.Env}
			switch {
			case sv.Command != "" && !allowCommands:
				warns = append(warns, "MCP server "+sv.Name+" runs a local command; add it with --allow-hooks")
				continue
			case sv.Validate() != nil:
				warns = append(warns, "MCP server "+sv.Name+" is not added: "+sv.Validate().Error())
				continue
			}
			servers = append(servers, sv)
		}
		break
	}
	return servers, warns
}

type rawMCP struct {
	URL     string            `json:"url"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// removeExpanded takes out what an expansion wrote: its skills, its files in
// the sync directory (unless changed since) and its MCP servers, then syncs
// the change to the agent.
func removeExpanded(s *pluginSession, a *pluginAgent, _ string, in plugins.Install) error {
	if err := removeLedgerFiles(s, a, "", in); err != nil {
		return err
	}
	if adapter, ok := agentport.AdapterByID(agentport.ProviderID(a.id)); ok {
		for _, skill := range in.Skills {
			if _, err := agentport.RemoveEntriesFor(skill, adapter.ID(), agentport.ScopeUser, agentport.KindSkill); err != nil {
				return fmt.Errorf("update the skill manifest: %w", err)
			}
		}
	}
	var kinds []string
	for _, f := range in.Files {
		switch {
		case strings.Contains(filepath.ToSlash(f.Path), "/sync/commands/"):
			kinds = append(kinds, "commands")
		case strings.Contains(filepath.ToSlash(f.Path), "/sync/rules/"):
			kinds = append(kinds, "rules")
		}
	}
	if len(in.MCP) > 0 {
		env := agentsync.Env{Home: s.home, Project: s.home}
		src, err := agentsync.LoadMCPSource(env, agentsync.ScopeUser)
		if err != nil {
			return err
		}
		src.Servers = slices.DeleteFunc(src.Servers, func(sv agentsync.MCPServer) bool { return slices.Contains(in.MCP, sv.Name) })
		if err := agentsync.SaveMCPSource(env, agentsync.ScopeUser, src); err != nil {
			return err
		}
		kinds = append(kinds, "mcp")
	}
	slices.Sort(kinds)
	return s.syncKinds(a.id, slices.Compact(kinds))
}
