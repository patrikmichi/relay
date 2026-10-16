package cli

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
)

// deviceReporter sends the device report after a sync; a variable so tests
// that sync against an unreachable gateway never dial it.
var deviceReporter = reportDevice

type deviceMCP struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Host      string `json:"host,omitempty"`
	Command   string `json:"command,omitempty"`
	Firewall  bool   `json:"firewall"`
}

type deviceAgent struct {
	ID         string      `json:"id"`
	Version    string      `json:"version,omitempty"`
	GatewayMCP bool        `json:"gatewayMcp"`
	MCPServers []deviceMCP `json:"mcpServers"`
}

type deviceReport struct {
	InstallID  string        `json:"installId"`
	CLIVersion string        `json:"cliVersion"`
	OS         string        `json:"os"`
	Arch       string        `json:"arch"`
	HostLabel  string        `json:"hostLabel"`
	Agents     []deviceAgent `json:"agents"`
}

// buildDeviceReport describes this machine: which coding agents are installed,
// whether each has the gateway's MCP server, and which MCP servers each has.
// It carries names, transports, the host of a remote URL and the program name
// of a local command; never arguments, environment values or headers.
func buildDeviceReport(env agentsync.Env, gatewayURL, hostLabel string) (deviceReport, error) {
	in, err := config.LoadInstall()
	if err != nil {
		return deviceReport{}, err
	}
	rep := deviceReport{InstallID: in.InstallID, CLIVersion: client.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, HostLabel: hostLabel, Agents: []deviceAgent{}}
	gateway, _, _, _ := gatewayMCPServer("auto", gatewayURL)
	for _, id := range agentsync.Detected(env) {
		a := deviceAgent{ID: string(id), MCPServers: []deviceMCP{}, GatewayMCP: hasGatewayEntry(env, id, gateway)}
		// ReadAgent lists only what the person wrote; the entry zuano wrote is
		// found by planning it and seeing nothing would change.
		bundle, _, err := agentsync.ReadAgent(env, id, agentsync.ScopeUser, []agentsync.Kind{agentsync.KindMCP})
		if err == nil {
			for _, s := range bundle.MCP {
				m := deviceMCP{Name: s.Name}
				if s.URL != "" {
					m.Transport = "http"
					if u, err := url.Parse(s.URL); err == nil {
						m.Host = u.Host
					}
				} else {
					m.Transport = "stdio"
					m.Command = strings.TrimSuffix(filepath.Base(s.Command), ".exe")
				}
				a.MCPServers = append(a.MCPServers, m)
			}
		}
		rep.Agents = append(rep.Agents, a)
	}
	return rep, nil
}

// hasGatewayEntry reports whether the agent's config already holds the
// gateway's MCP server exactly as sync would write it.
func hasGatewayEntry(env agentsync.Env, id agentsync.AgentID, gateway *agentsync.MCPServer) bool {
	if gateway == nil {
		return false
	}
	plan, err := agentsync.Build(env, map[agentsync.Scope]agentsync.Bundle{agentsync.ScopeUser: {MCP: []agentsync.MCPServer{*gateway}}},
		agentsync.Options{Agents: []agentsync.AgentID{id}, Scopes: []agentsync.Scope{agentsync.ScopeUser}, Kinds: []agentsync.Kind{agentsync.KindMCP}})
	if err != nil {
		return false
	}
	for _, it := range plan.Items {
		if it.Server == gatewayServerName && it.Status == agentsync.StatusUnchanged {
			return true
		}
	}
	return false
}

// hostLabel is the label reports carry: the one set with --host-label
// (possibly blank), else the machine's name.
func hostLabel(in config.Install) string {
	if in.HostLabelSet {
		return in.HostLabel
	}
	name, _ := os.Hostname()
	return name
}

// reportDevice posts the device report with the person's own session. An
// agent session never reports: an injected agent must not be able to invent
// a device inventory.
func reportDevice(cmd *cobra.Command, gatewayURL string, label *string) {
	if Offline() || config.AgentMode() {
		return
	}
	gw, err := config.GatewayURL()
	if err != nil || gw == "" {
		return
	}
	if gatewayURL != "" {
		if gw, err = config.NormalizeGatewayURL(gatewayURL); err != nil {
			return
		}
	}
	c, err := client.Resolve(gw)
	if err != nil {
		return
	}
	in, err := config.LoadInstall()
	if err != nil {
		return
	}
	if label != nil {
		in.HostLabel, in.HostLabelSet = *label, true
		if err := config.SaveInstall(in); err != nil {
			return
		}
	}
	env, err := syncEnv()
	if err != nil {
		return
	}
	rep, err := buildDeviceReport(env, gw, hostLabel(in))
	if err != nil {
		return
	}
	if _, err := gatewayJSON(cmd.Context(), c, http.MethodPost, "/api/agents/devices/report", rep, nil); err != nil {
		cmd.PrintErrf("warning: the device report was not sent: %v\n", err)
		return
	}
	_ = config.RecordReport(len(rep.Agents))
}

// doctorDevice says what this installation is known as and when it last
// reported.
func doctorDevice(add func(string, bool, string)) {
	in, err := config.LoadInstall()
	if err != nil {
		add("install", false, "install.json cannot be read: "+err.Error())
		return
	}
	last := "never reported"
	if in.LastReportAt != "" {
		last = "last device report " + in.LastReportAt
	}
	add("install", true, "install id "+in.InstallID+"; "+last)
}
