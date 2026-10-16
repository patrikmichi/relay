package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/plugins"
)

// doctorPlugins checks the plugin ledger: files that drifted since zuano wrote
// them, and the credential helper each Claude Code install depends on.
func doctorPlugins(add func(string, bool, string)) *plugins.Ledger {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	l, err := plugins.LoadLedger(plugins.LedgerPath(home))
	if err != nil {
		add("plugins", false, "the plugin ledger cannot be read: "+err.Error())
		return nil
	}
	installs := 0
	for _, n := range l.Plugins() {
		installs += len(l.Installs[n])
	}
	if installs == 0 {
		add("plugins", true, "no plugins installed")
		return l
	}
	if drift := ledgerDrift(l); len(drift) > 0 {
		add("plugins", false, fmt.Sprintf("%d installed file(s) differ from what zuano wrote (first: %s); `%s` restores them", len(drift), drift[0], brand.Cmd("plugin install <name>")))
	} else {
		add("plugins", true, fmt.Sprintf("%d plugin install(s), no drift", installs))
	}
	for _, name := range l.Plugins() {
		in, ok := l.Get(name, "claude")
		if !ok || in.Marketplace == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
		if err != nil {
			add("plugin-helper:claude", false, "Claude Code's settings.json cannot be read; run `"+brand.Cmd("plugin feeds setup --to claude")+"`")
			continue
		}
		v, found, _ := agentsync.ReadJSONValue(data, []string{"extraKnownMarketplaces", in.Marketplace}, "headersHelper")
		if s, _ := v.(string); !found || !strings.Contains(s, "auth header") {
			add("plugin-helper:claude", false, "marketplace "+in.Marketplace+" has no zuano credential helper; run `"+brand.Cmd("plugin feeds setup --to claude")+"`")
		} else {
			add("plugin-helper:claude", true, "marketplace "+in.Marketplace+" authenticates through `"+brand.Cmd("auth header")+"`")
		}
		break
	}
	return l
}

// doctorPluginFeed checks that the gateway still serves the plugin feed to
// someone who has plugins installed from it.
func doctorPluginFeed(ctx context.Context, c *client.Client, l *plugins.Ledger, add func(string, bool, string)) {
	if l == nil || len(l.Plugins()) == 0 {
		return
	}
	var doc struct {
		Feed pluginFeed `json:"feed"`
	}
	if _, err := gatewayJSON(ctx, c, http.MethodGet, "/api/org/plugin-feeds", nil, &doc); err != nil {
		add("plugin-feed", false, "the plugin feed is not available: "+err.Error())
		return
	}
	add("plugin-feed", true, "feed "+doc.Feed.Slug+" is available")
}
