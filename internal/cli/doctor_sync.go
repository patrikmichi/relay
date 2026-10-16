package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/patrikmichi/zuano/internal/agentsync"
)

// brokenMarkers are the reasons a sync cannot touch a file at all; they
// need a human, unlike ordinary drift.
var brokenMarkers = []string{"not plain JSON", "not valid TOML", "unbalanced", "is not an object", "is not an array"}

func isBroken(it agentsync.Item) bool {
	if it.Status != agentsync.StatusSkipped {
		return false
	}
	for _, m := range brokenMarkers {
		if strings.Contains(it.Detail, m) {
			return true
		}
	}
	return false
}

type syncGroup struct {
	scope agentsync.Scope
	agent agentsync.AgentID
	kind  agentsync.Kind
}

// classifySyncItems turns a dry-run sync plan into one doctor check per
// agent, scope and artifact kind.
func classifySyncItems(items []agentsync.Item) []doctorCheck {
	groups := map[syncGroup][]agentsync.Item{}
	for _, it := range items {
		g := syncGroup{it.Scope, it.Agent, it.Kind}
		groups[g] = append(groups[g], it)
	}
	keys := make([]syncGroup, 0, len(groups))
	for g := range groups {
		keys = append(keys, g)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.scope != b.scope {
			return a.scope < b.scope
		}
		if a.agent != b.agent {
			return a.agent < b.agent
		}
		return a.kind < b.kind
	})

	var checks []doctorCheck
	for _, g := range keys {
		name := fmt.Sprintf("sync:%s:%s:%s", g.scope, g.agent, g.kind)
		counts := map[string]int{}
		var broken, edited, unsupported, owned []string
		for _, it := range groups[g] {
			counts[it.Status]++
			switch {
			case isBroken(it):
				broken = append(broken, fmt.Sprintf("%s %s", displayPath(it.Path), it.Detail))
			case it.Status == agentsync.StatusConflict && strings.HasPrefix(it.Detail, "edited since"):
				edited = append(edited, it.Name)
			case it.Status == agentsync.StatusConflict:
				owned = append(owned, it.Name)
			case it.Status == agentsync.StatusUnsupported:
				unsupported = append(unsupported, it.Detail)
			}
		}
		drift := counts[agentsync.StatusUpdated] + counts[agentsync.StatusRemoved]
		fresh := counts[agentsync.StatusAdded]
		inSync := counts[agentsync.StatusUnchanged]

		switch {
		case len(broken) > 0:
			checks = append(checks, doctorCheck{name, false, "cannot sync: " + strings.Join(broken, "; ")})
		case len(edited) > 0:
			checks = append(checks, doctorCheck{name, false, "entries edited since zuano wrote them: " + strings.Join(edited, ", ") + "; zuano sync --force restores them"})
		case drift > 0 || (fresh > 0 && inSync > 0):
			checks = append(checks, doctorCheck{name, false, fmt.Sprintf("out of sync (%d pending); run `zuano sync`", drift+fresh)})
		case fresh > 0:
			checks = append(checks, doctorCheck{name, true, fmt.Sprintf("not synced yet (%d pending); run `zuano sync`", fresh)})
		case len(unsupported) > 0 && inSync == 0:
			checks = append(checks, doctorCheck{name, true, "unsupported: " + unsupported[0]})
		default:
			detail := fmt.Sprintf("in sync (%d entries)", inSync)
			if len(owned) > 0 {
				detail += fmt.Sprintf("; %d of yours block zuano entries: %s", len(owned), strings.Join(owned, ", "))
			}
			checks = append(checks, doctorCheck{name, true, detail})
		}
	}
	return checks
}

// doctorSyncChecks covers MCP servers, hooks, rules and commands for every
// detected agent, plus the sync sources and the credentials MCP entries
// read from the environment.
func doctorSyncChecks(add func(name string, ok bool, detail string)) {
	env, err := syncEnv()
	if err != nil {
		add("sync", false, err.Error())
		return
	}
	doctorRouteChecks(add)
	scopes := []agentsync.Scope{agentsync.ScopeUser, agentsync.ScopeProject}
	if _, err := agentsync.LoadLedger(env.Home); err != nil {
		add("sync-ledger", false, "sync ledger cannot be read: "+err.Error())
		return
	}
	var fw *firewallOpts
	if on, _ := firewallEnabled(nil); on {
		fw = &firewallOpts{}
	}
	doctorFirewall(add)
	bundles, warns, _, err := loadBundles(env, scopes, "auto", "", nil, fw)
	if err != nil {
		add("sync-source", false, err.Error())
		return
	}
	if len(warns) > 0 {
		add("sync-source", false, warns[0])
	} else {
		add("sync-source", true, "rules, commands, hooks and MCP sources parse")
	}

	envVars := map[string]bool{}
	for _, b := range bundles {
		for _, s := range b.MCP {
			if s.BearerEnv != "" {
				envVars[s.BearerEnv] = true
			}
		}
	}
	names := make([]string, 0, len(envVars))
	for v := range envVars {
		names = append(names, v)
	}
	sort.Strings(names)
	for _, v := range names {
		set := os.Getenv(v) != ""
		detail := v + " is set"
		if !set {
			detail = v + " is not set in this shell; agents started from it cannot authenticate to the MCP servers that read it"
		}
		add("mcp-env:"+v, set, detail)
	}

	agents := agentsync.Detected(env)
	if len(agents) == 0 {
		add("sync", true, "no supported coding agent detected")
		return
	}
	plan, err := agentsync.Build(env, bundles, agentsync.Options{Agents: agents, Scopes: scopes})
	if err != nil {
		add("sync", false, err.Error())
		return
	}
	for _, c := range classifySyncItems(plan.Items) {
		add(c.Name, c.OK, c.Detail)
	}
}
