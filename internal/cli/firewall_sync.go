package cli

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/keychain"
)

// firewallPath is where the gateway serves a remote server it fronts.
const firewallPath = "/api/fw/"

var (
	secretName   = regexp.MustCompile(`(?i)(key|token|secret|passw(or)?d|credential)`)
	slugInvalid  = regexp.MustCompile(`[^a-z0-9]+`)
	literalValue = regexp.MustCompile(`^[^$]+$`)
)

type secretMove struct{ server, name, value string }

// firewallOpts says how a sync routes servers through the firewall.
type firewallOpts struct {
	// moveSecrets stores literal secret-looking environment values in the
	// keychain and has the shim read them there.
	moveSecrets bool
	moves       []secretMove
}

func upstreamSlug(name string) string {
	s := strings.Trim(slugInvalid.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		return "server"
	}
	return s
}

// firewallEnabled is the saved choice, overridden by want when set.
func firewallEnabled(want *bool) (bool, error) {
	if want != nil {
		return *want, nil
	}
	s, err := config.SyncConfig()
	return s.Firewall, err
}

// wrapped reports a server the sync already routes through the firewall.
func wrapped(s agentsync.MCPServer, gw string) bool {
	if s.URL != "" {
		return gw != "" && strings.HasPrefix(s.URL, gw+firewallPath)
	}
	return len(s.Args) >= 2 && s.Args[0] == "mcp" && s.Args[1] == "wrap"
}

// firewallServers routes servers through the firewall: a local server runs
// under `zuano mcp wrap`, a remote one is reached through the gateway, which
// holds its credentials. The gateway's own entry and servers that run zuano
// are left alone.
func firewallServers(servers []agentsync.MCPServer, gw, installID string, fw *firewallOpts) ([]agentsync.MCPServer, []string) {
	var warns []string
	out := make([]agentsync.MCPServer, 0, len(servers))
	for _, s := range servers {
		if s.Name == gatewayServerName || wrapped(s, gw) || strings.TrimSuffix(filepath.Base(s.Command), ".exe") == brand.Name {
			out = append(out, s)
			continue
		}
		slug := upstreamSlug(s.Name)
		switch {
		case s.URL != "":
			if gw == "" {
				warns = append(warns, fmt.Sprintf("%s is not routed through the firewall: no gateway is configured", s.Name))
				out = append(out, s)
				continue
			}
			w := agentsync.MCPServer{Name: s.Name, URL: gw + firewallPath + slug + "/mcp", Headers: map[string]string{}}
			for k, v := range s.Headers {
				w.Headers[k] = v
			}
			if installID != "" {
				w.Headers[brand.InstallHeader] = installID
			}
			if s.BearerEnv != "" {
				warns = append(warns, fmt.Sprintf("%s: the token in $%s is not sent through the firewall; the gateway holds the credentials, connect it with `%s`", s.Name, s.BearerEnv, brand.Cmd("firewall connect "+slug)))
			}
			out = append(out, w)
		default:
			args := []string{"mcp", "wrap", "--upstream", slug}
			env := map[string]string{}
			for k, v := range s.Env {
				if fw != nil && fw.moveSecrets && secretName.MatchString(k) && literalValue.MatchString(v) {
					fw.moves = append(fw.moves, secretMove{slug, k, v})
					args = append(args, "--env-from-keychain", k)
					continue
				}
				env[k] = v
			}
			if len(env) == 0 {
				env = nil
			}
			args = append(append(args, "--", s.Command), s.Args...)
			out = append(out, agentsync.MCPServer{Name: s.Name, Command: brand.Name, Args: args, Env: env})
		}
	}
	return out, warns
}

// parseFirewallFlag reads --firewall: nil when it was not given.
func parseFirewallFlag(cmd *cobra.Command, value string) (*bool, error) {
	if !cmd.Flags().Changed("firewall") {
		return nil, nil
	}
	var on bool
	switch value {
	case "on", "true":
		on = true
	case "off", "false":
	default:
		return nil, fmt.Errorf("invalid --firewall %q: use on or off", value)
	}
	return &on, nil
}

// saveFirewallChoice remembers --firewall once the files were written.
func saveFirewallChoice(want *bool) error {
	if want == nil {
		return nil
	}
	return config.UpdateSync(func(s *config.SyncSettings) error {
		s.Firewall = *want
		return nil
	})
}

// storeMoves puts the moved secrets in the keychain.
func storeMoves(moves []secretMove) error {
	for _, m := range moves {
		if err := keychain.WriteSecret(m.server, m.name, m.value); err != nil {
			return err
		}
	}
	return nil
}

// firewallGateway is the gateway origin the firewall's servers are reached at.
func firewallGateway(override string) string {
	s, _, _, err := gatewayMCPServer("auto", override)
	if err != nil || s == nil {
		return ""
	}
	return strings.TrimSuffix(s.URL, "/api/mcp")
}

// doctorFirewall reports whether the sync routes servers through the firewall.
func doctorFirewall(add func(string, bool, string)) {
	on, err := firewallEnabled(nil)
	if err != nil {
		add("firewall", false, err.Error())
		return
	}
	if !on {
		add("firewall", true, "MCP servers are not routed through the firewall (`"+brand.Cmd("sync --firewall")+"` turns it on)")
		return
	}
	add("firewall", true, "MCP servers synced from zuano are routed through the firewall")
}
