package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentport"
	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/config"
)

// zuanoSource is the "from" of a route that writes settings (mcp, env, hooks,
// rules, commands): those come from the zuano sync directory, not from
// another agent's files.
const zuanoSource = brand.Name

var (
	settingKinds = []string{"mcp", "env", "hooks", "rules", "commands"}
	routeKinds   = append(slices.Clone(settingKinds), "skills", "subagents")
)

func isSettingKind(k string) bool { return slices.Contains(settingKinds, k) }

// routeSummary is what one saved route did in a routed sync.
type routeSummary struct {
	Route  string         `json:"route"`
	Counts map[string]int `json:"counts"`
}

func (r *agentSyncRun) edits() []agentsync.Edit {
	var out []agentsync.Edit
	for _, p := range r.plans {
		out = append(out, p.Edits...)
	}
	return out
}

func printRouteSummaries(out io.Writer, routes []routeSummary) {
	if len(routes) == 0 {
		return
	}
	fmt.Fprintln(out, "Routes:")
	for _, r := range routes {
		var parts []string
		for _, s := range []string{agentsync.StatusAdded, agentsync.StatusUpdated, agentsync.StatusRemoved, agentsync.StatusUnchanged, agentsync.StatusConflict, agentsync.StatusSkipped, agentsync.StatusUnsupported} {
			if r.Counts[s] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", r.Counts[s], s))
			}
		}
		if len(parts) == 0 {
			parts = []string{"nothing to do"}
		}
		fmt.Fprintf(out, "  %s: %s\n", r.Route, strings.Join(parts, ", "))
	}
	fmt.Fprintln(out)
}

func splitList(values []string) []string {
	var out []string
	for _, raw := range values {
		for _, v := range strings.Split(raw, ",") {
			if v = strings.ToLower(strings.TrimSpace(v)); v != "" && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

func normalizeKinds(values []string) ([]string, error) {
	kinds := splitList(values)
	for i, k := range kinds {
		if k == "agents" {
			kinds[i] = "subagents"
			k = "subagents"
		}
		if !slices.Contains(routeKinds, k) {
			return nil, fmt.Errorf("unknown artifact kind %q (%s)", k, strings.Join(routeKinds, ", "))
		}
	}
	if len(kinds) == 0 {
		return nil, fmt.Errorf("a route needs at least one kind (%s)", strings.Join(routeKinds, ", "))
	}
	return slices.Compact(kinds), nil
}

func normalizeTargets(values []string) ([]string, error) {
	ids, err := agentsync.ParseAgents(values)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("name at least one target agent with --to")
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out, nil
}

// normalizeRoute validates a route and puts it in canonical form. A route
// copies either skills and subagents from a provider, or settings from the
// zuano sync directory; the two read from different places, so mixing them
// in one route would give "from" two meanings.
func normalizeRoute(r config.SyncRoute) (config.SyncRoute, error) {
	kinds, err := normalizeKinds(r.Kinds)
	if err != nil {
		return r, err
	}
	to, err := normalizeTargets(r.To)
	if err != nil {
		return r, err
	}
	var skill, setting bool
	for _, k := range kinds {
		if isSettingKind(k) {
			setting = true
		} else {
			skill = true
		}
	}
	from := strings.ToLower(strings.TrimSpace(r.From))
	if skill {
		if from == "" {
			return r, fmt.Errorf("a route for skills or subagents needs --from, the provider to copy them from (%s)", providerIDsOxford())
		}
		if _, ok := agentport.AdapterByID(agentport.ProviderID(from)); !ok {
			return r, fmt.Errorf("unknown --from provider %q (%s)", from, providerIDsOxford())
		}
	}
	if setting {
		if from == "" || from == zuanoSource {
			if !skill {
				from = zuanoSource
			}
		} else {
			ids, err := agentsync.ParseAgents([]string{from})
			if err != nil || len(ids) != 1 {
				return r, fmt.Errorf("unknown --from agent %q for settings (%s)", from, strings.Join(agentIDNames(), ", "))
			}
			if slices.Contains(kinds, "env") {
				return r, fmt.Errorf("env is zuano's agent profile, not something copied between agents; route it without --from")
			}
			if slices.Contains(to, string(ids[0])) {
				return r, fmt.Errorf("a route cannot copy %s to itself", ids[0])
			}
			if !skill {
				from = string(ids[0])
			}
		}
	}
	return config.SyncRoute{From: from, To: to, Kinds: kinds}, nil
}

func agentIDNames() []string {
	var out []string
	for _, a := range agentsync.Agents() {
		out = append(out, string(a.ID))
	}
	return out
}

// routeCycle reports routes that copy a kind around in a circle (a to b and b
// back to a). Zuano-written content is never read back, so a cycle would not
// loop, but it is almost certainly a mistake: each side would push the other's
// own files to it.
func routeCycle(routes []config.SyncRoute) error {
	for _, kind := range settingKinds {
		edges := map[string][]string{}
		for _, r := range routes {
			if r.From == zuanoSource || !slices.Contains(r.Kinds, kind) {
				continue
			}
			from := r.From
			if ids, err := agentsync.ParseAgents([]string{from}); err == nil && len(ids) == 1 {
				from = string(ids[0])
			}
			edges[from] = append(edges[from], r.To...)
		}
		state := map[string]int{}
		var path []string
		var visit func(string) []string
		visit = func(n string) []string {
			state[n] = 1
			path = append(path, n)
			for _, next := range edges[n] {
				switch state[next] {
				case 1:
					i := slices.Index(path, next)
					return append(slices.Clone(path[i:]), next)
				case 0:
					if c := visit(next); c != nil {
						return c
					}
				}
			}
			path = path[:len(path)-1]
			state[n] = 2
			return nil
		}
		keys := make([]string, 0, len(edges))
		for k := range edges {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if state[k] == 0 {
				if c := visit(k); c != nil {
					return fmt.Errorf("routes copy %s in a cycle: %s", kind, strings.Join(c, " -> "))
				}
			}
		}
	}
	return nil
}

func routeLabel(r config.SyncRoute) string {
	return fmt.Sprintf("%s -> %s (%s)", r.From, strings.Join(r.To, ","), strings.Join(r.Kinds, ", "))
}

type routeOpt struct {
	label string
	opts  agentSyncOpts
}

// routeOptions turns saved settings into one sync per route, then one per
// distinct default target list for the setting kinds no route covers. It
// also returns the kinds nothing covers.
func routeOptions(s config.SyncSettings, base agentSyncOpts) ([]routeOpt, []string) {
	var out []routeOpt
	covered := map[string]bool{}
	for _, r := range s.Routes {
		var setting, skill []string
		for _, k := range r.Kinds {
			covered[k] = true
			if isSettingKind(k) {
				setting = append(setting, k)
			} else {
				skill = append(skill, k)
			}
		}
		// Settings are read from an agent (or the zuano sync directory),
		// skills and subagents from a provider; one route may carry both.
		if len(setting) > 0 {
			o := base
			o.to, o.kinds, o.from = r.To, setting, ""
			if r.From != zuanoSource {
				o.settingsFrom = r.From
			}
			out = append(out, routeOpt{routeLabel(config.SyncRoute{From: r.From, To: r.To, Kinds: setting}), o})
		}
		if len(skill) > 0 {
			o := base
			o.to, o.kinds, o.from = r.To, skill, r.From
			out = append(out, routeOpt{routeLabel(config.SyncRoute{From: r.From, To: r.To, Kinds: skill}), o})
		}
	}

	groups := map[string][]string{}
	targets := map[string][]string{}
	for _, k := range settingKinds {
		t, ok := s.Defaults[k]
		if covered[k] || !ok || len(t) == 0 {
			continue
		}
		covered[k] = true
		key := strings.Join(t, ",")
		groups[key] = append(groups[key], k)
		targets[key] = t
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		o := base
		o.to, o.kinds, o.from = targets[key], groups[key], ""
		out = append(out, routeOpt{routeLabel(config.SyncRoute{From: zuanoSource, To: targets[key], Kinds: groups[key]}) + " [default]", o})
	}

	var uncovered []string
	for _, k := range routeKinds {
		if !covered[k] {
			uncovered = append(uncovered, k)
		}
	}
	return out, uncovered
}

// planRoutedSync plans every saved route and merges them into one run, so
// the report, the confirmation and the apply step see a single sync. Two
// routes may not write the same file: each plan is computed from the file as
// it is now, so the second write would be refused as a conflict.
func planRoutedSync(base agentSyncOpts, s config.SyncSettings) (*agentSyncRun, error) {
	if err := routeCycle(s.Routes); err != nil {
		return nil, err
	}
	opts, uncovered := routeOptions(s, base)
	merged := &agentSyncRun{stdio: map[string]bool{}, projectRemote: map[string]bool{}}
	writer := map[string]string{}
	for _, ro := range opts {
		run, err := planAgentSync(ro.opts)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", ro.label, err)
		}
		for _, e := range run.edits() {
			if prior, ok := writer[e.Path]; ok {
				return nil, fmt.Errorf("routes %s and %s both write %s; combine them into one route", prior, ro.label, e.Path)
			}
			writer[e.Path] = ro.label
		}
		counts := map[string]int{}
		for _, it := range run.items {
			counts[it.Status]++
		}
		merged.routes = append(merged.routes, routeSummary{ro.label, counts})
		merged.items = append(merged.items, run.items...)
		merged.plans = append(merged.plans, run.plans...)
		merged.writes = append(merged.writes, run.writes...)
		for _, w := range run.warns {
			if !slices.Contains(merged.warns, w) {
				merged.warns = append(merged.warns, w)
			}
		}
		for _, a := range run.agents {
			if !slices.Contains(merged.agents, a) {
				merged.agents = append(merged.agents, a)
			}
		}
		for n := range run.stdio {
			merged.stdio[n] = true
		}
		for n := range run.projectRemote {
			merged.projectRemote[n] = true
		}
		if merged.gatewayURL == "" {
			merged.gatewayURL = run.gatewayURL
		}
	}
	if len(uncovered) > 0 {
		merged.warns = append(merged.warns, "not synced, no route or default covers: "+strings.Join(uncovered, ", ")+" (see `zuano sync routes list`; --to/--from/--kind run one sync without the saved routes)")
	}
	return merged, nil
}

// SyncRoutesCmd returns `zuano sync routes`.
func SyncRoutesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "routes",
		Short: "Show and change the saved routes zuano sync follows",
		Long: `Saved routes tell a plain 'zuano sync' what to copy where. They live in the
"sync" key of ~/.config/zuano/config.json.

  zuano sync routes add --from claude --to cursor --kind skills,subagents
  zuano sync routes add --kind hooks,rules --to claude,codex
  zuano sync routes list
  zuano sync routes remove 1

A route with --from copies from that source: skills and subagents from a
provider, and settings (mcp, hooks, rules, commands) from an agent's own
config, reading only what you wrote there. Without --from, --kind names
settings and --to the default agents that receive them when no route covers
the kind; their source is the zuano sync directory. With saved routes, a plain 'zuano sync' follows
them and syncs nothing else; --to, --from or --kind run one ordinary sync
instead. 'zuano sync --dry-run' shows the plan per route.`,
	}
	cmd.AddCommand(syncRoutesListCmd(), syncRoutesAddCmd(), syncRoutesRemoveCmd())
	return cmd
}

func syncRoutesListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the saved routes and default targets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := config.SyncConfig()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(s)
			}
			if len(s.Routes) == 0 && len(s.Defaults) == 0 {
				fmt.Fprintln(out, "No saved routes: zuano sync writes every kind to every detected agent. Add one with `zuano sync routes add`.")
				return nil
			}
			for i, r := range s.Routes {
				fmt.Fprintf(out, "%d. %s\n", i+1, routeLabel(r))
			}
			for _, k := range settingKinds {
				if t := s.Defaults[k]; len(t) > 0 {
					fmt.Fprintf(out, "default %s -> %s\n", k, strings.Join(t, ","))
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the settings as JSON")
	return cmd
}

func syncRoutesAddCmd() *cobra.Command {
	var from string
	var to, kinds []string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a route, or default targets for settings kinds",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseInAgentMode(brand.Cmd("sync routes add")); err != nil {
				return err
			}
			r, err := normalizeRoute(config.SyncRoute{From: from, To: to, Kinds: kinds})
			if err != nil {
				return err
			}
			asDefault := from == "" && r.From == zuanoSource
			err = config.UpdateSync(func(s *config.SyncSettings) error {
				if asDefault {
					if s.Defaults == nil {
						s.Defaults = map[string][]string{}
					}
					for _, k := range r.Kinds {
						s.Defaults[k] = r.To
					}
					return nil
				}
				for _, existing := range s.Routes {
					if routeLabel(existing) == routeLabel(r) {
						return fmt.Errorf("route %s already exists", routeLabel(r))
					}
				}
				s.Routes = append(s.Routes, r)
				return routeCycle(s.Routes)
			})
			if err != nil {
				return err
			}
			if asDefault {
				fmt.Fprintf(cmd.OutOrStdout(), "default targets for %s: %s\n", strings.Join(r.Kinds, ", "), strings.Join(r.To, ","))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added route %s\n", routeLabel(r))
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&from, "from", "", "Provider (skills, subagents) or agent (settings) to copy from; omit for settings from the zuano sync directory")
	fl.StringSliceVar(&to, "to", nil, "Agents that receive the kinds")
	fl.StringSliceVar(&kinds, "kind", nil, "mcp, env, hooks, rules, commands, skills, subagents")
	return cmd
}

func syncRoutesRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <number|kind>",
		Short: "Remove a route by its number, or the default targets of a kind",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseInAgentMode(brand.Cmd("sync routes remove")); err != nil {
				return err
			}
			arg := strings.ToLower(strings.TrimSpace(args[0]))
			var removed string
			err := config.UpdateSync(func(s *config.SyncSettings) error {
				if n, convErr := strconv.Atoi(arg); convErr == nil {
					if n < 1 || n > len(s.Routes) {
						return fmt.Errorf("no route number %d (there are %d)", n, len(s.Routes))
					}
					removed = "route " + routeLabel(s.Routes[n-1])
					s.Routes = slices.Delete(s.Routes, n-1, n)
					return nil
				}
				if _, ok := s.Defaults[arg]; !ok {
					return fmt.Errorf("%q is neither a route number nor a kind with default targets", args[0])
				}
				removed = "default targets for " + arg
				delete(s.Defaults, arg)
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", removed)
			return nil
		},
	}
}

// doctorRouteChecks verifies each saved route and default target list is
// valid and names agents installed on this machine.
func doctorRouteChecks(add func(name string, ok bool, detail string)) {
	s, err := config.SyncConfig()
	if err != nil {
		add("sync-routes", false, "saved routes cannot be read: "+err.Error())
		return
	}
	if len(s.Routes) == 0 && len(s.Defaults) == 0 {
		return
	}
	env, err := syncEnv()
	if err != nil {
		add("sync-routes", false, err.Error())
		return
	}
	installed := map[string]bool{}
	for _, id := range agentsync.Detected(env) {
		installed[string(id)] = true
	}
	known := map[string]bool{}
	for _, a := range agentsync.Agents() {
		known[string(a.ID)] = true
	}
	missing := func(targets []string, from string) []string {
		var out []string
		for _, t := range targets {
			if !installed[t] {
				out = append(out, t)
			}
		}
		if from != "" && from != zuanoSource && known[from] && !installed[from] {
			out = append(out, from+" (source)")
		}
		return out
	}
	check := func(name string, r config.SyncRoute) {
		if _, err := normalizeRoute(r); err != nil {
			add(name, false, err.Error())
			return
		}
		if m := missing(r.To, r.From); len(m) > 0 {
			add(name, false, "not installed on this machine: "+strings.Join(m, ", "))
			return
		}
		add(name, true, "targets are installed")
	}
	for i, r := range s.Routes {
		check(fmt.Sprintf("sync-route:%d", i+1), r)
	}
	for _, k := range settingKinds {
		if t := s.Defaults[k]; len(t) > 0 {
			check("sync-default:"+k, config.SyncRoute{From: zuanoSource, To: t, Kinds: []string{k}})
		}
	}
}
