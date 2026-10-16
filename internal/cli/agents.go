package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

type agentRow struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`
	Owner        string            `json:"owner"`
	Status       string            `json:"status"`
	LastActivity string            `json:"lastActivity"`
	Exposure     string            `json:"exposure"`
	Labels       map[string]string `json:"labels"`
	ReviewDue    string            `json:"reviewDue"`
}

// AgentsCmd returns `zuano agents`, the client for the gateway's inventory of
// agents, keys and other non-human identities.
func AgentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "See every agent, key and client that can act in your organisation, and stop one",
		Long: `The gateway keeps an inventory of every identity that can call tools: MCP
clients, CLI sessions, API keys, triggers and CI workflows, with the person who
owns each, what it can reach and how much risk that is. These commands read it
and let you stop an identity.

suspend, resume and revoke change what an identity may do. They are for a
person, refused while ZUANO_AGENT is set (exit 10), and resume and revoke ask
for a passkey confirmation (exit 14 when nobody can give it). A suspended,
revoked or expired identity's own requests end with exit 33.`,
	}
	cmd.AddCommand(agentsListCmd(), agentsShowCmd(), agentsActivityCmd(), agentsStateCmd("suspend"), agentsStateCmd("resume"), agentsStateCmd("revoke"), agentsDevicesCmd(), agentsShadowCmd(), agentsQuarantineCmd())
	return cmd
}

func refuseAgentChange(what string) error {
	if config.AgentMode() {
		return &ExitError{Code: exitDenied, Msg: fmt.Sprintf("%s changes what an identity may do and is for a person: it is refused while ZUANO_AGENT is set", what)}
	}
	return nil
}

func agentsListCmd() *cobra.Command {
	var f approvalFlags
	var kind, status, owner, exposure, can, risk, mode string
	var labels []string
	var stale bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List identities, with filters such as who can reach a tool",
		Example: `  zuano agents list --exposure high
  zuano agents list --can stripe.create_refund --mode allowed`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			for k, v := range map[string]string{"kind": kind, "status": status, "owner": owner, "exposure": exposure, "can": can, "risk": risk, "mode": mode} {
				if v != "" {
					q.Set(k, v)
				}
			}
			for _, l := range labels {
				if !strings.Contains(l, "=") {
					return fmt.Errorf("--label %q must be key=value", textsafe.Clean(l))
				}
				q.Add("label", l)
			}
			if stale {
				q.Set("stale", "true")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			path := "/api/agents"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var doc struct {
				Agents []agentRow `json:"agents"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Agents) == 0 {
				fmt.Fprintln(out, "No identities match.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tKIND\tOWNER\tSTATUS\tLAST ACTIVITY\tEXPOSURE")
			for _, a := range doc.Agents {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", textsafe.Clean(a.ID), textsafe.Clean(a.Name), textsafe.Clean(a.Kind), orDash(textsafe.Clean(a.Owner)), textsafe.Clean(a.Status), orDash(textsafe.Clean(a.LastActivity)), orDash(textsafe.Clean(a.Exposure)))
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	fl := cmd.Flags()
	fl.StringVar(&kind, "kind", "", "Only this kind: mcp_client, cli_agent, cli_personal, api_key_agent, trigger, workload, coding_agent")
	fl.StringVar(&status, "status", "", "Only this status: active, suspended, revoked, expired")
	fl.StringVar(&owner, "owner", "", "Only identities owned by this member (email)")
	fl.StringArrayVar(&labels, "label", nil, "Only identities with this label, as key=value; repeatable")
	fl.BoolVar(&stale, "stale", false, "Only identities with no recent activity")
	fl.StringVar(&exposure, "exposure", "", "Only this exposure: low, medium or high")
	fl.StringVar(&can, "can", "", "Only identities that can reach this service or service.tool")
	fl.StringVar(&risk, "risk", "", "With --can: at this risk class")
	fl.StringVar(&mode, "mode", "", "With --can: in this mode, such as allowed or approval")
	return cmd
}

func agentsShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one identity: credentials, device, what it can reach and why it is exposed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Agent struct {
					agentRow
					Credentials []struct {
						Kind      string `json:"kind"`
						CreatedAt string `json:"createdAt"`
						ExpiresAt string `json:"expiresAt"`
					} `json:"credentials"`
					Device *struct {
						HostLabel string `json:"hostLabel"`
						OS        string `json:"os"`
					} `json:"device"`
					Access []struct {
						Service string `json:"service"`
						Risk    string `json:"risk"`
						Mode    string `json:"mode"`
						Source  string `json:"source"`
					} `json:"access"`
					ExposureFactors []string `json:"exposureFactors"`
				} `json:"agent"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/agents/"+url.PathEscape(args[0]), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			a := doc.Agent
			fmt.Fprintf(out, "%s %s (%s, %s)\n", textsafe.Clean(a.ID), textsafe.Clean(a.Name), textsafe.Clean(a.Kind), textsafe.Clean(a.Status))
			fmt.Fprintf(out, "  owner: %s\n  exposure: %s\n", orDash(textsafe.Clean(a.Owner)), orDash(textsafe.Clean(a.Exposure)))
			if a.Device != nil {
				fmt.Fprintf(out, "  device: %s (%s)\n", textsafe.Clean(a.Device.HostLabel), textsafe.Clean(a.Device.OS))
			}
			for _, cr := range a.Credentials {
				fmt.Fprintf(out, "  credential %s, created %s, expires %s\n", textsafe.Clean(cr.Kind), orDash(textsafe.Clean(cr.CreatedAt)), orDash(textsafe.Clean(cr.ExpiresAt)))
			}
			for _, x := range a.Access {
				fmt.Fprintf(out, "  can reach %-12s %-12s %-10s decided by %s\n", textsafe.Clean(x.Service), textsafe.Clean(x.Risk), textsafe.Clean(x.Mode), orDash(textsafe.Clean(x.Source)))
			}
			if len(a.ExposureFactors) > 0 {
				fmt.Fprintf(out, "  why: %s\n", textsafe.Clean(strings.Join(a.ExposureFactors, "; ")))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func agentsActivityCmd() *cobra.Command {
	var f approvalFlags
	var from, to string
	cmd := &cobra.Command{
		Use:   "activity <id>",
		Short: "Show what an identity did: calls per service, approvals and refusals",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := url.Values{}
			if from != "" {
				q.Set("from", from)
			}
			if to != "" {
				q.Set("to", to)
			}
			path := "/api/agents/" + url.PathEscape(args[0]) + "/activity"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var doc struct {
				Activity struct {
					Calls []struct {
						Service string `json:"service"`
						Count   int    `json:"count"`
					} `json:"calls"`
					Approvals int `json:"approvals"`
					Refusals  int `json:"refusals"`
				} `json:"activity"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			for _, call := range doc.Activity.Calls {
				fmt.Fprintf(out, "  %-16s %d calls\n", textsafe.Clean(call.Service), call.Count)
			}
			fmt.Fprintf(out, "%d approvals, %d refusals\n", doc.Activity.Approvals, doc.Activity.Refusals)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&from, "from", "", "First day, YYYY-MM-DD")
	cmd.Flags().StringVar(&to, "to", "", "Last day, YYYY-MM-DD")
	return cmd
}

func agentsStateCmd(verb string) *cobra.Command {
	var f approvalFlags
	var reason string
	var yes bool
	short := map[string]string{
		"suspend": "Stop an identity from acting until it is resumed",
		"resume":  "Let a suspended identity act again (needs a passkey confirmation)",
		"revoke":  "End an identity for good, with its credentials (needs a passkey confirmation)",
	}[verb]
	cmd := &cobra.Command{
		Use:   verb + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("agents " + verb)); err != nil {
				return err
			}
			if verb != "resume" && strings.TrimSpace(reason) == "" {
				return fmt.Errorf("--reason is required to %s an identity", verb)
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if verb == "revoke" {
				if err := confirmOrYes(cmd, yes, fmt.Sprintf("Revoke %s? Its credentials stop working, its pending approvals are cancelled and its running work is stopped.", textsafe.Clean(args[0]))); err != nil {
					return err
				}
			}
			body := map[string]any{}
			if verb != "resume" {
				body["reason"] = reason
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/agents/"+url.PathEscape(args[0])+"/"+verb, body, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s done.\n", textsafe.Clean(args[0]), verb)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	if verb != "resume" {
		cmd.Flags().StringVar(&reason, "reason", "", "Why; recorded in the audit log")
	}
	if verb == "revoke" {
		cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Revoke without asking")
	}
	return cmd
}

func agentsDevicesCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "List machines that reported their coding agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Devices []struct {
					HostLabel    string `json:"hostLabel"`
					OS           string `json:"os"`
					CLIVersion   string `json:"cliVersion"`
					Owner        string `json:"owner"`
					LastReportAt string `json:"lastReportAt"`
					Agents       []struct {
						ID         string `json:"id"`
						GatewayMCP bool   `json:"gatewayMcp"`
					} `json:"agents"`
				} `json:"devices"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/agents/devices", nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "HOST\tOWNER\tOS\tCLI\tLAST REPORT\tCODING AGENTS")
			for _, d := range doc.Devices {
				var names []string
				for _, a := range d.Agents {
					n := textsafe.Clean(a.ID)
					if a.GatewayMCP {
						n += "*"
					}
					names = append(names, n)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", orDash(textsafe.Clean(d.HostLabel)), orDash(textsafe.Clean(d.Owner)), textsafe.Clean(d.OS), textsafe.Clean(d.CLIVersion), orDash(textsafe.Clean(d.LastReportAt)), strings.Join(names, ", "))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintln(out, "* has the gateway's MCP server")
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func agentsShadowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "shadow-mcp",
		Short: "List MCP servers on devices that are neither connectors nor firewall upstreams",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Servers []struct {
					Name      string `json:"name"`
					Transport string `json:"transport"`
					Host      string `json:"host"`
					Command   string `json:"command"`
					Devices   int    `json:"devices"`
				} `json:"servers"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/agents/shadow-mcp", nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Servers) == 0 {
				fmt.Fprintln(out, "No unmanaged MCP servers reported.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVER\tTRANSPORT\tWHERE\tDEVICES")
			for _, s := range doc.Servers {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", textsafe.Clean(s.Name), textsafe.Clean(s.Transport), orDash(textsafe.Clean(firstNonEmptyString(s.Host, s.Command))), s.Devices)
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}
