package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const (
	gatewayServerName = brand.Name
	gatewayAPIKeyEnv  = "ZUANO_API_KEY"
)

type agentSyncOpts struct {
	to     []string
	scopes []string
	kinds  []string
	from   string
	// settingsFrom names the agent whose own rules, commands, hooks and MCP
	// servers are copied, instead of the zuano sync directory.
	settingsFrom string
	mcpAuth      string
	gatewayURL   string
	dryRun       bool
	force        bool
	yes          bool
	acceptLoss   bool
	jsonOut      bool
	// mcpOverride replaces the saved MCP source for a scope, so install and
	// remove can preview a change before persisting it.
	mcpOverride map[agentsync.Scope]agentsync.MCPSource
	// optionalAgents lets a run with no detectable agents succeed quietly,
	// used when the caller has other work (the marketplace pull) to do.
	optionalAgents bool
	// extraRules are rules the gateway assigns, written to every synced scope
	// next to the ones from the sync directory.
	extraRules []agentsync.Rule
}

type syncKinds struct {
	file      []agentsync.Kind
	skills    bool
	subagents bool
}

func parseSyncKinds(values []string) (syncKinds, error) {
	var k syncKinds
	if len(values) == 0 {
		k.file, k.skills, k.subagents = agentsync.FileKinds, true, true
		return k, nil
	}
	for _, raw := range values {
		for _, v := range strings.Split(raw, ",") {
			switch v = strings.TrimSpace(v); v {
			case "":
			case "mcp", "env", "hooks", "rules", "commands":
				k.file = append(k.file, agentsync.Kind(v))
			case "skills":
				k.skills = true
			case "subagents", "agents":
				k.subagents = true
			default:
				return k, fmt.Errorf("unknown artifact kind %q (mcp, env, hooks, rules, commands, skills, subagents)", v)
			}
		}
	}
	return k, nil
}

func parseSyncScopes(values []string) ([]agentsync.Scope, error) {
	if len(values) == 0 {
		return []agentsync.Scope{agentsync.ScopeUser}, nil
	}
	var out []agentsync.Scope
	seen := map[agentsync.Scope]bool{}
	add := func(s agentsync.Scope) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, raw := range values {
		for _, v := range strings.Split(raw, ",") {
			switch strings.TrimSpace(v) {
			case "":
			case "user":
				add(agentsync.ScopeUser)
			case "project":
				add(agentsync.ScopeProject)
			case "all":
				add(agentsync.ScopeUser)
				add(agentsync.ScopeProject)
			default:
				return nil, fmt.Errorf("invalid --scope %q: use user, project or all", v)
			}
		}
	}
	return out, nil
}

func syncEnv() (agentsync.Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return agentsync.Env{}, fmt.Errorf("resolve home directory: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return agentsync.Env{}, fmt.Errorf("resolve project directory: %w", err)
	}
	return agentsync.Env{Home: home, Project: cwd}, nil
}

// gatewayMCPServer is the aggregate MCP server the gateway exposes. With
// an API key in the environment the agents read it by name; otherwise the
// entry carries no credentials and the agent signs in with OAuth.
//
// Its URL is the gateway saved in the config file, never $ZUANO_URL: an
// environment variable must not decide where every agent sends its traffic.
// override (an explicit --gateway-url) replaces it and is returned as changed
// when it differs from the saved one, so the caller can ask first. ignored
// is a $ZUANO_URL that was left out.
func gatewayMCPServer(mode, override string) (s *agentsync.MCPServer, changed, ignored string, err error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, "", "", fmt.Errorf("load config: %w", err)
	}
	var saved string
	if cfg.GatewayURL != "" {
		if saved, err = config.NormalizeGatewayURL(cfg.GatewayURL); err != nil {
			return nil, "", "", err
		}
	}
	gw := saved
	if override != "" {
		if gw, err = config.NormalizeGatewayURL(override); err != nil {
			return nil, "", "", err
		}
		if gw != saved {
			changed = gw
		}
	}
	if env := os.Getenv("ZUANO_URL"); env != "" && override == "" {
		if n, nerr := config.NormalizeGatewayURL(env); nerr != nil || n != gw {
			ignored = env
		}
	}
	if gw == "" {
		return nil, "", ignored, nil
	}
	s = &agentsync.MCPServer{Name: gatewayServerName, URL: strings.TrimRight(gw, "/") + "/api/mcp"}
	if id, idErr := config.InstallID(); idErr == nil {
		s.Headers = map[string]string{brand.InstallHeader: id}
	}
	switch mode {
	case "", "auto", "oauth":
		// The gateway marks every OAuth session as an agent's, so its writes
		// wait for approval; an API key carries no such marker.
	case "api-key", "apikey":
		s.BearerEnv = gatewayAPIKeyEnv
	default:
		return nil, "", "", fmt.Errorf("invalid --mcp-auth %q: use auto, oauth or api-key", mode)
	}
	return s, changed, ignored, nil
}

// agentEnvVar selects the agent profile in zuano itself: the CLI session
// marked as an agent's, held to approvals, instead of the person's own.
const agentEnvVar = "ZUANO_AGENT"

// markZuanoServers returns servers with ZUANO_AGENT=1 in the environment of
// every stdio server that runs zuano, so a zuano started by an agent uses the
// agent profile. The saved source is left as the user wrote it.
func markZuanoServers(servers []agentsync.MCPServer) []agentsync.MCPServer {
	out := make([]agentsync.MCPServer, len(servers))
	for i, s := range servers {
		out[i] = s
		name := strings.TrimSuffix(filepath.Base(s.Command), ".exe")
		if s.Command == "" || name != brand.Name {
			continue
		}
		if _, set := s.Env[agentEnvVar]; set {
			continue
		}
		env := map[string]string{agentEnvVar: "1"}
		maps.Copy(env, s.Env)
		out[i].Env = env
	}
	return out
}

// agentProfileWarnings names what would leave an agent outside the agent
// profile: a gateway entry that sends an unmarked API key, and environment
// settings that select an agent session which does not exist yet.
func agentProfileWarnings(user agentsync.Bundle, kinds syncKinds) []string {
	var warns []string
	if slices.Contains(kinds.file, agentsync.KindMCP) {
		for _, s := range user.MCP {
			if s.Name == gatewayServerName && s.BearerEnv != "" {
				warns = append(warns, fmt.Sprintf("the gateway MCP entry authenticates with the API key in $%s, which is not marked as an agent's: calls through it bypass approvals; use --mcp-auth oauth", s.BearerEnv))
			}
		}
	}
	if slices.Contains(kinds.file, agentsync.KindEnv) && len(user.Env) > 0 {
		if cfg, err := config.Load(); err == nil && cfg.AgentEmail == "" {
			warns = append(warns, "agents will run zuano as the agent profile (ZUANO_AGENT=1) but no agent session exists; run `zuano login --agent`")
		}
	}
	return warns
}

// gatewayNotes reports how the gateway entry's URL was chosen: changed is a
// gateway other than the saved one, which needs the person's confirmation;
// ignoredEnv is a $ZUANO_URL that was left out.
type gatewayNotes struct{ changed, ignoredEnv string }

func loadBundles(env agentsync.Env, scopes []agentsync.Scope, mcpAuth, gatewayURL string, override map[agentsync.Scope]agentsync.MCPSource) (bundles map[agentsync.Scope]agentsync.Bundle, warns []string, notes gatewayNotes, err error) {
	bundles = map[agentsync.Scope]agentsync.Bundle{}
	for _, scope := range scopes {
		b, w, err := agentsync.LoadBundle(agentsync.SourceDir(env, scope), agentsync.SourceCheck(env, scope))
		if err != nil {
			return nil, nil, notes, err
		}
		warns = append(warns, w...)
		src, ok := override[scope]
		if !ok {
			if src, err = agentsync.LoadMCPSource(env, scope); err != nil {
				return nil, nil, notes, err
			}
		}
		if scope == agentsync.ScopeUser && !src.GatewayDisabled {
			gw, changed, ignored, err := gatewayMCPServer(mcpAuth, gatewayURL)
			if err != nil {
				return nil, nil, notes, err
			}
			notes = gatewayNotes{changed, ignored}
			if gw != nil {
				b.MCP = append(b.MCP, *gw)
			}
		}
		b.MCP = append(b.MCP, markZuanoServers(src.Servers)...)
		if scope == agentsync.ScopeUser {
			b.Env = []agentsync.EnvVar{{Name: agentEnvVar, Value: "1"}}
		}
		bundles[scope] = b
	}
	return bundles, warns, notes, nil
}

// agentSyncRun is a computed sync: file edits plus skill and subagent
// writes, ready to print or apply.
type agentSyncRun struct {
	items []agentsync.Item
	// plans are the file edits; a routed sync has one per route.
	plans  []*agentsync.Plan
	writes []func() error
	warns  []string
	agents []agentsync.AgentID
	// routes summarises each saved route a routed sync followed.
	routes []routeSummary
	// stdio names the servers that run a local command.
	stdio map[string]bool
	// projectRemote names the remote servers a project directory declares.
	// A project is untrusted input: its servers can send the session to a
	// host it chose.
	projectRemote map[string]bool
	// gatewayURL is set when the gateway entry would point somewhere other
	// than the gateway saved in the config file.
	gatewayURL string
}

func planAgentSync(o agentSyncOpts) (*agentSyncRun, error) {
	env, err := syncEnv()
	if err != nil {
		return nil, err
	}
	scopes, err := parseSyncScopes(o.scopes)
	if err != nil {
		return nil, err
	}
	kinds, err := parseSyncKinds(o.kinds)
	if err != nil {
		return nil, err
	}
	agents, err := agentsync.ParseAgents(o.to)
	if err != nil {
		return nil, err
	}
	if len(agents) == 0 {
		agents = agentsync.Detected(env)
	}
	run := &agentSyncRun{agents: agents}
	if len(agents) == 0 {
		if o.optionalAgents {
			return run, nil
		}
		return nil, errors.New("no supported coding agent detected on this machine; name one with --to (claude, codex, cursor, opencode, gemini-cli, windsurf, cline, copilot) or --to all")
	}

	bundles, warns, gw, err := loadBundles(env, scopes, o.mcpAuth, o.gatewayURL, o.mcpOverride)
	if err != nil {
		return nil, err
	}
	if o.settingsFrom == "" && len(o.extraRules) > 0 {
		for scope, b := range bundles {
			b.Rules = append(b.Rules, o.extraRules...)
			bundles[scope] = b
		}
	}
	if o.settingsFrom != "" {
		from, err := agentsync.ParseAgents([]string{o.settingsFrom})
		if err != nil || len(from) != 1 {
			return nil, fmt.Errorf("unknown agent %q to copy settings from", o.settingsFrom)
		}
		gw = gatewayNotes{}
		for _, scope := range scopes {
			b, w, err := agentsync.ReadAgent(env, from[0], scope, kinds.file)
			if err != nil {
				return nil, err
			}
			b.MCP = markZuanoServers(b.MCP)
			bundles[scope] = b
			warns = append(warns, w...)
		}
	}
	run.gatewayURL = gw.changed
	if gw.ignoredEnv != "" {
		warns = append(warns, fmt.Sprintf("ignoring $ZUANO_URL (%s): agent configs use the gateway saved in the config file; pass --gateway-url to write another", textsafe.Clean(gw.ignoredEnv)))
	}
	warns = append(warns, agentProfileWarnings(bundles[agentsync.ScopeUser], kinds)...)
	run.warns = warns
	run.stdio = map[string]bool{}
	run.projectRemote = map[string]bool{}
	for scope, b := range bundles {
		for _, s := range b.MCP {
			if s.Command != "" {
				run.stdio[s.Name] = true
			}
			if s.URL != "" && scope == agentsync.ScopeProject {
				run.projectRemote[s.Name] = true
			}
		}
	}
	if len(kinds.file) > 0 {
		plan, err := agentsync.Build(env, bundles, agentsync.Options{Agents: agents, Scopes: scopes, Kinds: kinds.file, Force: o.force})
		if err != nil {
			return nil, err
		}
		run.plans = append(run.plans, plan)
		run.items = append(run.items, plan.Items...)
	}
	if kinds.skills {
		run.warns = append(run.warns, skillRequirementWarnings(o.from, scopes)...)
	}
	if kinds.skills || kinds.subagents {
		for _, scope := range scopes {
			items, writes, err := planSkillsAndSubagents(skillSyncOpts{
				from: o.from, targets: agents, scope: scope, skills: kinds.skills, subagents: kinds.subagents,
				acceptLoss: o.acceptLoss, dryRun: o.dryRun, force: o.force,
			})
			if err != nil {
				return nil, err
			}
			run.items = append(run.items, items...)
			run.writes = append(run.writes, writes...)
		}
	}
	return run, nil
}

// apply writes the planned files in one transaction, then installs skills
// and subagents one by one through the agentport engine.
func (r *agentSyncRun) apply(ctx context.Context) error {
	for _, p := range r.plans {
		if err := p.Apply(ctx); err != nil {
			return err
		}
	}
	for _, w := range r.writes {
		if err := w(); err != nil {
			return err
		}
	}
	return nil
}

func displayPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return p
}

func printSyncReport(out io.Writer, run *agentSyncRun, dryRun, jsonOut bool) error {
	if jsonOut {
		type edit struct {
			Path string `json:"path"`
			Diff string `json:"diff,omitempty"`
		}
		doc := struct {
			DryRun   bool             `json:"dryRun"`
			Agents   []string         `json:"agents"`
			Items    []agentsync.Item `json:"items"`
			Warnings []string         `json:"warnings,omitempty"`
			Edits    []edit           `json:"edits,omitempty"`
			Routes   []routeSummary   `json:"routes,omitempty"`
		}{DryRun: dryRun, Items: run.items, Warnings: run.warns}
		for _, a := range run.agents {
			doc.Agents = append(doc.Agents, string(a))
		}
		if dryRun {
			for _, e := range run.edits() {
				doc.Edits = append(doc.Edits, edit{e.Path, e.Diff()})
			}
		}
		doc.Routes = run.routes
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}

	for _, w := range run.warns {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	printRouteSummaries(out, run.routes)
	if len(run.items) == 0 {
		fmt.Fprintln(out, "nothing to sync: add rules, commands, hooks or MCP servers under the zuano sync directory, or configure a gateway")
		return nil
	}
	items := append([]agentsync.Item(nil), run.items...)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Scope != items[j].Scope {
			return items[i].Scope < items[j].Scope
		}
		return items[i].Agent < items[j].Agent
	})
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tKIND\tSCOPE\tSTATUS\tNAME\tDETAIL")
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Status]++
		detail := it.Detail
		if it.Path != "" {
			detail = strings.TrimSpace(displayPath(it.Path) + "  " + detail)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", it.Agent, it.Kind, it.Scope, it.Status, it.Name, detail)
	}
	_ = tw.Flush()

	var parts []string
	for _, s := range []string{agentsync.StatusAdded, agentsync.StatusUpdated, agentsync.StatusRemoved, agentsync.StatusUnchanged, agentsync.StatusConflict, agentsync.StatusSkipped, agentsync.StatusUnsupported, agentsync.StatusWarning} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	fmt.Fprintf(out, "\n%s\n", strings.Join(parts, ", "))
	if counts[agentsync.StatusConflict] > 0 {
		fmt.Fprintln(out, "Conflicts are entries zuano did not write; rerun with --force to replace them.")
	}

	if dryRun && len(run.plans) > 0 {
		for _, e := range run.edits() {
			fmt.Fprintf(out, "\n%s", e.Diff())
		}
		fmt.Fprintln(out, "\n[dry-run] no files written")
	}
	return nil
}

func printSyncMatrix(out io.Writer) error {
	env, err := syncEnv()
	if err != nil {
		return err
	}
	rows := agentsync.Matrix(env)
	type cell struct{ user, project string }
	grid := map[agentsync.AgentID]map[agentsync.Kind]*cell{}
	reasons := map[string]string{}
	for _, r := range rows {
		if grid[r.Agent] == nil {
			grid[r.Agent] = map[agentsync.Kind]*cell{}
		}
		c := grid[r.Agent][r.Kind]
		if c == nil {
			c = &cell{}
			grid[r.Agent][r.Kind] = c
		}
		mark := "yes"
		if !r.Supported {
			mark = "no"
			reasons[fmt.Sprintf("%s %s (%s)", r.Agent, r.Kind, r.Scope)] = r.Reason
		}
		if r.Scope == agentsync.ScopeUser {
			c.user = mark
		} else {
			c.project = mark
		}
	}
	kinds := []agentsync.Kind{agentsync.KindMCP, agentsync.KindEnv, agentsync.KindHooks, agentsync.KindRules, agentsync.KindCommands, agentsync.KindSkills, agentsync.KindSubagents}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	header := "AGENT"
	for _, k := range kinds {
		header += "\t" + strings.ToUpper(string(k))
	}
	fmt.Fprintln(tw, header+"\t(user/project)")
	for _, a := range agentsync.Agents() {
		line := string(a.ID)
		for _, k := range kinds {
			c := grid[a.ID][k]
			line += "\t" + c.user + "/" + c.project
		}
		fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintln(out, "\nNot supported:")
	for _, k := range keys {
		fmt.Fprintf(out, "  %s: %s\n", k, reasons[k])
	}
	return nil
}
