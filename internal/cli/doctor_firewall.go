package cli

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// unwrappedServers lists the MCP servers a person configured in their agents
// that are not routed through the firewall, as "agent: name".
func unwrappedServers(where agentsync.Env, gw string) ([]string, error) {
	var out []string
	for _, id := range agentsync.Detected(where) {
		for _, scope := range []agentsync.Scope{agentsync.ScopeUser, agentsync.ScopeProject} {
			b, _, err := agentsync.ReadAgent(where, id, scope, []agentsync.Kind{agentsync.KindMCP})
			if err != nil {
				return nil, err
			}
			for _, s := range b.MCP {
				if s.Name == gatewayServerName || wrapped(s, gw) || strings.TrimSuffix(filepath.Base(s.Command), ".exe") == brand.Name {
					continue
				}
				out = append(out, fmt.Sprintf("%s: %s", id, s.Name))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// doctorFirewallRequired fails when the organisation requires MCP servers to be
// routed through the firewall and some are not.
func doctorFirewallRequired(ctx context.Context, c *client.Client, gateway string, add func(string, bool, string)) {
	var req struct {
		RequiredRouting bool `json:"requiredRouting"`
	}
	if _, err := gatewayJSON(ctx, c, http.MethodGet, "/api/fw/local/requirements", nil, &req); err != nil || !req.RequiredRouting {
		return
	}
	where, err := syncEnv()
	if err != nil {
		add("firewall-required", false, err.Error())
		return
	}
	list, err := unwrappedServers(where, strings.TrimRight(gateway, "/"))
	if err != nil {
		add("firewall-required", false, err.Error())
		return
	}
	if len(list) == 0 {
		add("firewall-required", true, "the organisation requires routing through the firewall and every MCP server is")
		return
	}
	add("firewall-required", false, fmt.Sprintf("the organisation requires MCP servers to go through the firewall; not routed: %s (`%s <name>` routes one)", textsafe.Clean(strings.Join(list, ", ")), brand.Cmd("firewall import-local")))
}
