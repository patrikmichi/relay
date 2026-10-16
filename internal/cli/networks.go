package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const networksAPI = "/api/admin/networks"

type networkSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Status      string `json:"status"`
	Description string `json:"description"`
	DataClass   string `json:"data_class"`
}

// NetworksCmd returns `zuano networks`, the admin side of private networks:
// where connector agents inside a customer network are enrolled and where
// connectors and MCP servers are routed through them.
func NetworksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "networks",
		Short: "Manage private networks, their connector agents and what is routed through them",
		Long: `A private network lets connectors and MCP servers reach systems that are not on
the internet, through a ` + brand.Name + `-connector agent that runs inside your network and
dials out. These commands are for organisation administrators: they create
networks, mint enrollment tokens, approve and revoke agents, show what each
agent allows and route a connector or MCP server through a network.

Changes that loosen access need a passkey; the command opens the page or waits
for the confirmation. They are refused while ZUANO_AGENT is set (exit 10). A
call that reaches an offline network exits 34.`,
	}
	cmd.AddCommand(networksListCmd(), networksShowCmd(), networksCreateCmd(), networksDeleteCmd(), networksTokenCmd(),
		networksAgentsCmd(), networksAgentActionCmd("approve"), networksAgentActionCmd("rotate"), networksAgentActionCmd("revoke"),
		networksAllowlistCmd(), networksLogsCmd(), networksBindCmd(false), networksBindCmd(true), networksTestCmd())
	return cmd
}

// resolveNetwork turns a name, slug or id into the network's id.
func resolveNetwork(ctx context.Context, c *client.Client, ref string) (networkSummary, error) {
	var doc struct {
		Networks []networkSummary `json:"networks"`
	}
	if _, err := gatewayJSON(ctx, c, http.MethodGet, networksAPI, nil, &doc); err != nil {
		return networkSummary{}, err
	}
	for _, n := range doc.Networks {
		if n.ID == ref || n.Slug == ref || n.Name == ref {
			return n, nil
		}
	}
	return networkSummary{}, fmt.Errorf("no network named %q; see `%s`", textsafe.Clean(ref), brand.Cmd("networks list"))
}

func networkPath(n networkSummary, rest ...string) string {
	return networksAPI + "/" + url.PathEscape(n.ID) + strings.Join(rest, "")
}

func networksListCmd() *cobra.Command {
	return listCommand("list", "List the private networks", networksAPI, "networks", "No private networks yet. Create one with `"+brand.Cmd("networks create <name>")+"`.",
		[]column{{"NAME", "name"}, {"STATUS", "status"}, {"AGENTS", "agents_online"}, {"BINDINGS", "bindings"}, {"LAST SEEN", "last_seen_at"}}, nil)
}

func networksShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a network's status and settings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			n, err := resolveNetwork(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			var doc map[string]json.RawMessage
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, networkPath(n), nil, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			var obj map[string]any
			_ = json.Unmarshal(doc["network"], &obj)
			printObject(cmd.OutOrStdout(), obj)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func networksCreateCmd() *cobra.Command {
	var f approvalFlags
	var description, dataClass string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a private network",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("networks create")); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			body := map[string]any{"name": args[0]}
			if description != "" {
				body["description"] = description
			}
			if dataClass != "" {
				body["data_class"] = dataClass
			}
			var res struct {
				Network networkSummary `json:"network"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, networksAPI, body, &res)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created %s. Enroll an agent with `%s`.\n", textsafe.Clean(res.Network.Name), brand.Cmd("networks token create "+args[0]))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&description, "description", "", "What the network is")
	cmd.Flags().StringVar(&dataClass, "data-class", "", "Label policies can match, such as internal or confidential")
	return cmd
}

func networksDeleteCmd() *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a network: its bindings are removed and its agents revoked",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("networks delete")); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			n, err := resolveNetwork(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("Delete %s, remove its bindings and revoke its agents?", textsafe.Clean(n.Name))); err != nil {
				return err
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodDelete, networkPath(n), nil, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s.\n", textsafe.Clean(n.Name))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().BoolVar(&yes, "yes", false, "Delete without asking")
	return cmd
}

func networksTokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Enrollment tokens for connector agents"}
	var f approvalFlags
	var uses int
	var ttl time.Duration
	var requireApproval bool
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Mint an enrollment token and print it with the install commands",
		Long: `The token is shown once, here, and is useless after use or expiry. Treat it as
a secret: it is not exposed over MCP and this command is refused while
ZUANO_AGENT is set. A single-use token lasts an hour by default; a token with
--uses above 1 needs --require-approval unless you turn that off.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("networks token create")); err != nil {
				return err
			}
			if uses < 1 || uses > 10 {
				return errors.New("--uses must be between 1 and 10")
			}
			if ttl <= 0 || ttl > 24*time.Hour {
				return errors.New("--ttl must be between 1s and 24h")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			n, err := resolveNetwork(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			mode := "single"
			if uses > 1 {
				mode = "multi"
			}
			body := map[string]any{"mode": mode, "uses": uses, "ttl_seconds": int(ttl.Seconds())}
			if cmd.Flags().Changed("require-approval") {
				body["require_approval"] = requireApproval
			}
			var res struct {
				Token     string `json:"token"`
				ExpiresAt string `json:"expires_at"`
				Snippets  []struct {
					Title string `json:"title"`
					Text  string `json:"text"`
				} `json:"snippets"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, networkPath(n, "/enrollment-tokens"), body, &res)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if !strings.HasPrefix(res.Token, "nxt_") {
				return errors.New("the gateway did not return an enrollment token")
			}
			fmt.Fprintf(out, "Enrollment token (shown once, expires %s):\n  %s\n", textsafe.Clean(orDash(res.ExpiresAt)), textsafe.Clean(res.Token))
			for _, s := range res.Snippets {
				fmt.Fprintf(out, "\n%s\n%s\n", textsafe.Clean(s.Title), strings.TrimRight(textsafe.Clean(s.Text), "\n"))
			}
			return nil
		},
	}
	f.bind(create, "Print the response body unchanged")
	create.Flags().IntVar(&uses, "uses", 1, "How many agents may enroll with it (1 to 10)")
	create.Flags().DurationVar(&ttl, "ttl", time.Hour, "How long it stays valid (up to 24h)")
	create.Flags().BoolVar(&requireApproval, "require-approval", false, "New agents wait for approval (the default when --uses is above 1)")
	cmd.AddCommand(create)
	return cmd
}

func networksAgentsCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "agents <name>",
		Short: "List a network's connector agents with their live status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			n, err := resolveNetwork(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			var doc map[string]json.RawMessage
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, networkPath(n, "/agents"), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			var rows []map[string]any
			_ = json.Unmarshal(doc["agents"], &rows)
			if len(rows) == 0 {
				fmt.Fprintf(out, "No agents. Enroll one with `%s`.\n", brand.Cmd("networks token create "+args[0]))
				return nil
			}
			printTable(out, rows, []column{{"ID", "id"}, {"NAME", "name"}, {"STATUS", "status"}, {"CONNECTION", "connection"}, {"VERSION", "version"}, {"OS/ARCH", "platform"}, {"LAST SEEN", "last_seen_at"}, {"KEY AGE (D)", "key_age_days"}})
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func networksAgentActionCmd(action string) *cobra.Command {
	var f approvalFlags
	var yes bool
	short := map[string]string{
		"approve": "Let a pending agent receive requests",
		"rotate":  "Make an agent rotate its keys now",
		"revoke":  "Cut an agent off: its socket closes within seconds and its keys are dropped",
	}[action]
	cmd := &cobra.Command{
		Use:   action + " <name> <agent>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("networks " + action)); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			n, err := resolveNetwork(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			if action == "revoke" {
				if err := confirmOrYes(cmd, yes, fmt.Sprintf("Revoke agent %s of %s?", textsafe.Clean(args[1]), textsafe.Clean(n.Name))); err != nil {
					return err
				}
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, networkPath(n, "/agents/", url.PathEscape(args[1]), "/", action), map[string]any{}, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: agent %s.\n", map[string]string{"approve": "Approved", "rotate": "Rotation requested", "revoke": "Revoked"}[action], textsafe.Clean(args[1]))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	if action == "revoke" {
		cmd.Flags().BoolVar(&yes, "yes", false, "Revoke without asking")
	}
	return cmd
}

func networksAllowlistCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "allowlist <name>",
		Short: "Show what each agent advertises it will serve, and whether they differ",
		Long:  `The allowlist lives in each agent's own config file, on your side; this shows the target names, kinds, methods and path patterns the agents report. A difference between agents is configuration drift.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			n, err := resolveNetwork(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			var doc struct {
				Drift   bool `json:"drift"`
				Targets []struct {
					Name    string   `json:"name"`
					Kind    string   `json:"kind"`
					Methods []string `json:"methods"`
					Paths   []string `json:"path_globs"`
					Tools   []string `json:"tool_globs"`
					Agents  []string `json:"agents"`
					Bound   []string `json:"bound_to"`
				} `json:"targets"`
				Unserved []string `json:"unserved_bindings"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, networkPath(n, "/allowlist"), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Targets) == 0 {
				fmt.Fprintln(out, "No agent advertises a target.")
			}
			for _, t := range doc.Targets {
				fmt.Fprintf(out, "%s (%s)\n  agents:  %s\n", textsafe.Clean(t.Name), textsafe.Clean(t.Kind), textsafe.Clean(orDash(strings.Join(t.Agents, ", "))))
				if len(t.Methods) > 0 {
					fmt.Fprintf(out, "  methods: %s\n", textsafe.Clean(strings.Join(t.Methods, ", ")))
				}
				if len(t.Paths) > 0 {
					fmt.Fprintf(out, "  paths:   %s\n", textsafe.Clean(strings.Join(t.Paths, ", ")))
				}
				if len(t.Tools) > 0 {
					fmt.Fprintf(out, "  tools:   %s\n", textsafe.Clean(strings.Join(t.Tools, ", ")))
				}
				if len(t.Bound) > 0 {
					fmt.Fprintf(out, "  bound:   %s\n", textsafe.Clean(strings.Join(t.Bound, ", ")))
				}
			}
			if doc.Drift {
				fmt.Fprintln(out, "\nwarning: the agents advertise different allowlists; give them the same config.")
			}
			for _, u := range doc.Unserved {
				fmt.Fprintf(out, "warning: %s is bound to a target no agent advertises.\n", textsafe.Clean(u))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func networksLogsCmd() *cobra.Command {
	var f approvalFlags
	var agent, target, outcome, since string
	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Show the exchange logs the agents uploaded (metadata only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			n, err := resolveNetwork(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			q := url.Values{}
			for k, v := range map[string]string{"agent": agent, "target": target, "outcome": outcome} {
				if v != "" {
					q.Set(k, v)
				}
			}
			if since != "" {
				t, err := parseSince(since)
				if err != nil {
					return err
				}
				q.Set("since", t.UTC().Format(time.RFC3339))
			}
			path := networkPath(n, "/logs")
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var doc map[string]json.RawMessage
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			var rows []map[string]any
			_ = json.Unmarshal(doc["logs"], &rows)
			if len(rows) == 0 {
				fmt.Fprintln(out, "No log lines match. Agents upload what log.upload allows (errors by default).")
				return nil
			}
			printTable(out, rows, []column{{"TIME", "ts"}, {"AGENT", "agent"}, {"TARGET", "target"}, {"METHOD", "method"}, {"STATUS", "status"}, {"OUTCOME", "outcome"}, {"MS", "duration_ms"}, {"AUDIT", "audit_ref"}})
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&agent, "agent", "", "Only this agent")
	cmd.Flags().StringVar(&target, "target", "", "Only this target")
	cmd.Flags().StringVar(&outcome, "outcome", "", "Only this outcome (ok, offline, refused, upstream_error, timeout, protocol_error)")
	cmd.Flags().StringVar(&since, "since", "", "Only newer than this (a duration such as 24h or 7d)")
	return cmd
}

// bindingTarget splits "mcp:<server>" from a connector name.
func bindingTarget(arg string) (key, value string, err error) {
	if server, ok := strings.CutPrefix(arg, "mcp:"); ok {
		if server == "" {
			return "", "", errors.New("name the MCP server after mcp:")
		}
		return "mcp_server", server, nil
	}
	if arg == "" {
		return "", "", errors.New("name a connector, or mcp:<server>")
	}
	return "service", arg, nil
}

func networksBindCmd(unbind bool) *cobra.Command {
	var f approvalFlags
	var network, target, credentials string
	use, short := "bind <connector|mcp:server>", "Route a connector or an MCP server through a network target"
	if unbind {
		use, short = "unbind <connector|mcp:server>", "Stop routing a connector or an MCP server through a network"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: `Adding a binding sends that connector's traffic into your network, which loosens
access and needs a passkey. A bound connector never falls back to the public
internet: when the network cannot serve a call, the call fails. Built-in
services such as Google or Slack cannot be bound.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("networks " + strings.Fields(use)[0])); err != nil {
				return err
			}
			key, value, err := bindingTarget(args[0])
			if err != nil {
				return err
			}
			if network == "" {
				return errors.New("--network is required")
			}
			if !unbind {
				if target == "" {
					return errors.New("--target is required: the target name from the agent's config")
				}
				if credentials != "agent" && credentials != "gateway" {
					return fmt.Errorf("invalid --credentials %q: use agent or gateway", credentials)
				}
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			n, err := resolveNetwork(cmd.Context(), c, network)
			if err != nil {
				return err
			}
			var raw []byte
			if unbind {
				raw, err = gatewayWrite(cmd, c, f.jsonOut, http.MethodDelete, networkPath(n, "/bindings?", url.Values{key: {value}}.Encode()), nil, nil)
			} else {
				raw, err = gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, networkPath(n, "/bindings"), map[string]any{key: value, "target": target, "credentials_mode": credentials}, nil)
			}
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			verb := "Bound"
			if unbind {
				verb = "Unbound"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s on %s.\n", verb, textsafe.Clean(args[0]), textsafe.Clean(n.Name))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&network, "network", "", "Network name")
	if !unbind {
		cmd.Flags().StringVar(&target, "target", "", "Target name from the agent's config")
		cmd.Flags().StringVar(&credentials, "credentials", "gateway", "gateway injects the stored credentials; agent uses the ones held on the agent")
	}
	return cmd
}

func networksTestCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "test <name>",
		Short: "Send a ping through each online agent and show the round-trip time",
		Long:  "Exits 34 when no agent is online.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			n, err := resolveNetwork(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			var res struct {
				Results []struct {
					Agent string `json:"agent"`
					OK    bool   `json:"ok"`
					RTT   int    `json:"rtt_ms"`
					Error string `json:"error"`
				} `json:"results"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, networkPath(n, "/test"), map[string]any{}, &res)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
			} else {
				for _, r := range res.Results {
					if r.OK {
						fmt.Fprintf(out, "%s: ok, %d ms\n", textsafe.Clean(r.Agent), r.RTT)
					} else {
						fmt.Fprintf(out, "%s: failed: %s\n", textsafe.Clean(r.Agent), textsafe.Clean(r.Error))
					}
				}
			}
			if len(res.Results) == 0 {
				return &ExitError{Code: exitNetworkOffline, Msg: textsafe.Clean(n.Name) + " is offline: no connector agent is connected"}
			}
			for _, r := range res.Results {
				if r.OK {
					return nil
				}
			}
			return &ExitError{Code: 1, Msg: "no agent answered the ping"}
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}
