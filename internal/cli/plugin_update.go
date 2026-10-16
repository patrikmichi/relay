package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/plugins"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// newLocalPluginSession is a session that reads and writes only local state;
// remove and verify work with no gateway.
func newLocalPluginSession(cmd *cobra.Command) (*pluginSession, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	s := &pluginSession{ctx: cmd.Context(), out: cmd.OutOrStdout(), err: cmd.ErrOrStderr(), home: home, ledgerPath: plugins.LedgerPath(home)}
	if s.ledger, err = plugins.LoadLedger(s.ledgerPath); err != nil {
		return nil, err
	}
	return s, nil
}

// installed returns the ledger's installs, optionally limited to one plugin
// and some agents, in a stable order.
type installRef struct {
	name, agent string
	in          plugins.Install
}

func (s *pluginSession) installed(name string, agents []string) []installRef {
	var out []installRef
	for _, n := range s.ledger.Plugins() {
		if name != "" && n != name {
			continue
		}
		var ids []string
		for id := range s.ledger.Installs[n] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if len(agents) > 0 && !containsFold(agents, id) {
				continue
			}
			out = append(out, installRef{n, id, s.ledger.Installs[n][id]})
		}
	}
	return out
}

func containsFold(list []string, v string) bool {
	for _, e := range list {
		if e == v || (v == "gemini-cli" && e == "gemini") {
			return true
		}
	}
	return false
}

func pluginUpdateCmd() *cobra.Command {
	var gatewayURL, channel string
	var to []string
	var opts installOpts
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "update [name]",
		Short: "Install newer versions of installed plugins and remove revoked ones",
		Long: `For each installed plugin (or the one named), looks in its channel's feed. A newer
version is verified and installed; a plugin the feed no longer lists was revoked
and is removed from the agent. A new version that runs different commands asks
for --allow-hooks again.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				var err error
				if name, _, err = parsePluginRef(args[0]); err != nil {
					return err
				}
			}
			s, err := newPluginSession(cmd, gatewayURL, orStable(channel), dryRun)
			if err != nil {
				return err
			}
			targets := s.installed(name, to)
			if len(targets) == 0 {
				fmt.Fprintln(s.out, "Nothing is installed.")
				return nil
			}
			if err := s.connect(); err != nil {
				return err
			}
			var results []pluginResult
			var firstErr error
			for _, t := range targets {
				ch := firstNonEmptyString(channel, t.in.Channel, "stable")
				a := pluginAgentByID(t.agent)
				if a == nil {
					continue
				}
				m, err := s.marketplace(platformClaude, ch)
				if err != nil {
					return err
				}
				entry, err := m.Find(t.name, "")
				switch {
				case errors.Is(err, plugins.ErrNotListed):
					res := pluginResult{Plugin: t.name, Agent: t.agent, Version: t.in.Version, Status: "revoked"}
					fmt.Fprintf(s.err, "%s %s is no longer in the %s channel (revoked); removing it from %s\n", textsafe.Clean(t.name), textsafe.Clean(t.in.Version), ch, t.agent)
					if !dryRun {
						if err := s.removeOne(a, t.name, t.in); err != nil {
							res.Status, res.Error = "failed", err.Error()
							fmt.Fprintf(s.err, "%s: %v\n", t.agent, err)
							firstErr = &ExitError{Code: 1}
						}
					}
					results = append(results, res)
				case err != nil:
					return err
				case entry.Version == t.in.Version:
					if !opts.jsonOut {
						fmt.Fprintf(s.out, "%s %s is up to date for %s\n", textsafe.Clean(t.name), textsafe.Clean(t.in.Version), t.agent)
					}
					results = append(results, pluginResult{Plugin: t.name, Agent: t.agent, Version: t.in.Version, Status: "current"})
				default:
					res, err := s.installFor(cmd, a, t.name, "", ch, opts)
					results = append(results, res)
					if err != nil {
						var ve *plugins.VerifyError
						if errors.As(err, &ve) {
							return s.finish(results, err, opts.jsonOut)
						}
						fmt.Fprintf(s.err, "%s: %v\n", t.agent, err)
						firstErr = &ExitError{Code: 1}
					}
				}
			}
			return s.finish(results, firstErr, opts.jsonOut)
		},
	}
	addPluginFlags(cmd, &gatewayURL, &channel, &to, "")
	cmd.Flags().BoolVar(&opts.allowHooks, "allow-hooks", false, "Allow a version that changes the commands the bundle runs")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would change")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "Print the outcome per agent as JSON")
	return cmd
}

func orStable(ch string) string {
	if ch == "" {
		return "stable"
	}
	return ch
}

// removeOne removes a plugin from one agent and forgets it.
func (s *pluginSession) removeOne(a *pluginAgent, name string, in plugins.Install) error {
	if err := a.remove(s, a, name, in); err != nil {
		return err
	}
	s.ledger.Delete(name, a.id)
	if err := s.saveLedger(); err != nil {
		return err
	}
	if s.client != nil && in.InstallID != "" {
		_, _ = gatewayJSON(s.ctx, s.client, http.MethodDelete, "/api/plugins/installs/"+url.PathEscape(in.InstallID), nil, nil)
	}
	return nil
}

func pluginRemoveCmd() *cobra.Command {
	var gatewayURL string
	var to []string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a plugin from coding agents",
		Long: `Removes a plugin the way it was installed: through the agent's own uninstall
command, or by deleting the files and entries zuano wrote (recorded in its
ledger). A file you changed since is kept.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _, err := parsePluginRef(args[0])
			if err != nil {
				return err
			}
			s, err := newLocalPluginSession(cmd)
			if err != nil {
				return err
			}
			if c, cerr := approvalsClient(gatewayURL); cerr == nil {
				s.client = c
			}
			targets := s.installed(name, to)
			if len(targets) == 0 {
				return fmt.Errorf("%s is not installed there", textsafe.Clean(name))
			}
			var results []pluginResult
			var firstErr error
			for _, t := range targets {
				res := pluginResult{Plugin: name, Agent: t.agent, Version: t.in.Version, Status: "removed"}
				a := pluginAgentByID(t.agent)
				if a == nil {
					s.ledger.Delete(name, t.agent)
					_ = s.saveLedger()
					results = append(results, res)
					continue
				}
				if err := s.removeOne(a, name, t.in); err != nil {
					res.Status, res.Error = "failed", err.Error()
					fmt.Fprintf(s.err, "%s: %v\n", t.agent, err)
					firstErr = &ExitError{Code: 1}
				} else if !jsonOut {
					fmt.Fprintf(s.out, "Removed %s from %s\n", textsafe.Clean(name), t.agent)
				}
				results = append(results, res)
			}
			return s.finish(results, firstErr, jsonOut)
		},
	}
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	cmd.Flags().StringSliceVar(&to, "to", nil, "Agents to remove it from (default: all it is installed in)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the outcome per agent as JSON")
	return cmd
}

func pluginListCmd() *cobra.Command {
	var gatewayURL, channel string
	var available, jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed plugins, or the ones the feed offers",
		Long: `Lists what zuano installed, per agent, with whether a newer version exists or the
version was revoked. With --available it lists what the channel's feed offers.
Without a reachable gateway only the local ledger is shown.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newPluginSession(cmd, gatewayURL, orStable(channel), false)
			online := err == nil && s.connect() == nil
			if !online {
				if s, err = newLocalPluginSession(cmd); err != nil {
					return err
				}
			}
			if available {
				if !online {
					return errors.New("--available needs the gateway: sign in and check your connection")
				}
				m, err := s.marketplace(platformClaude, orStable(channel))
				if err != nil {
					return err
				}
				if jsonOut {
					return json.NewEncoder(s.out).Encode(m)
				}
				tw := tabwriter.NewWriter(s.out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "PLUGIN\tVERSION\tDESCRIPTION")
				for _, p := range m.Plugins {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", textsafe.Clean(p.Name), textsafe.Clean(p.Version), textsafe.Clean(p.Description))
				}
				return tw.Flush()
			}
			type row struct {
				Plugin  string `json:"plugin"`
				Agent   string `json:"agent"`
				Version string `json:"version"`
				Method  string `json:"method"`
				Channel string `json:"channel"`
				Status  string `json:"status"`
			}
			var rows []row
			for _, t := range s.installed("", nil) {
				status := "unknown (offline)"
				if online {
					status = s.updateStatus(t)
				}
				rows = append(rows, row{t.name, t.agent, t.in.Version, t.in.Method, t.in.Channel, status})
			}
			if jsonOut {
				return json.NewEncoder(s.out).Encode(map[string]any{"installed": rows})
			}
			if len(rows) == 0 {
				fmt.Fprintln(s.out, "Nothing is installed.")
				return nil
			}
			tw := tabwriter.NewWriter(s.out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "PLUGIN\tAGENT\tVERSION\tMETHOD\tCHANNEL\tSTATUS")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", textsafe.Clean(r.Plugin), r.Agent, textsafe.Clean(r.Version), r.Method, textsafe.Clean(r.Channel), textsafe.Clean(r.Status))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	cmd.Flags().StringVar(&channel, "channel", "", "Channel for --available (default: stable)")
	cmd.Flags().BoolVar(&available, "available", false, "List what the feed offers instead of what is installed")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print JSON")
	return cmd
}

// updateStatus compares an install with its channel's feed.
func (s *pluginSession) updateStatus(t installRef) string {
	m, err := s.marketplace(platformClaude, firstNonEmptyString(t.in.Channel, "stable"))
	if err != nil {
		return "unknown"
	}
	e, err := m.Find(t.name, "")
	switch {
	case errors.Is(err, plugins.ErrNotListed):
		return "revoked"
	case err != nil:
		return "unknown"
	case e.Version != t.in.Version:
		return "update available (" + e.Version + ")"
	}
	return "current"
}

// ledgerDrift lists files an install wrote that are missing or changed.
func ledgerDrift(l *plugins.Ledger) []string {
	var out []string
	for _, name := range l.Plugins() {
		var agents []string
		for a := range l.Installs[name] {
			agents = append(agents, a)
		}
		sort.Strings(agents)
		for _, agent := range agents {
			for _, f := range l.Installs[name][agent].Files {
				cur, err := os.ReadFile(f.Path)
				switch {
				case errors.Is(err, os.ErrNotExist):
					out = append(out, fmt.Sprintf("%s (%s): %s is missing", name, agent, f.Path))
				case err != nil:
					out = append(out, fmt.Sprintf("%s (%s): %s cannot be read", name, agent, f.Path))
				case sha256Hex(cur) != f.SHA256:
					out = append(out, fmt.Sprintf("%s (%s): %s was changed", name, agent, f.Path))
				}
			}
		}
	}
	return out
}

func pluginVerifyCmd() *cobra.Command {
	var to []string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "verify <name>",
		Short: "Check that an installed plugin's files are as zuano wrote them",
		Long: `Re-hashes the files the ledger lists for a plugin and reports any that were
changed or deleted. Plugins an agent installed through its own system have no
files of zuano's to check. Exits 1 when anything drifted.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _, err := parsePluginRef(args[0])
			if err != nil {
				return err
			}
			s, err := newLocalPluginSession(cmd)
			if err != nil {
				return err
			}
			targets := s.installed(name, to)
			if len(targets) == 0 {
				return fmt.Errorf("%s is not installed", textsafe.Clean(name))
			}
			sub := &plugins.Ledger{Installs: map[string]map[string]plugins.Install{name: {}}}
			for _, t := range targets {
				sub.Installs[name][t.agent] = t.in
			}
			drift := ledgerDrift(sub)
			if jsonOut {
				if err := json.NewEncoder(s.out).Encode(map[string]any{"plugin": name, "drift": drift}); err != nil {
					return err
				}
			} else {
				for _, t := range targets {
					fmt.Fprintf(s.out, "%s on %s: version %s, %d files recorded (%s)\n", textsafe.Clean(name), t.agent, textsafe.Clean(t.in.Version), len(t.in.Files), t.in.Method)
				}
				for _, d := range drift {
					fmt.Fprintln(s.out, "  "+textsafe.Clean(d))
				}
				if len(drift) == 0 {
					fmt.Fprintln(s.out, "No drift.")
				}
			}
			if len(drift) > 0 {
				return &ExitError{Code: 1, Msg: fmt.Sprintf("%d file(s) differ from what zuano wrote; `%s` puts them back", len(drift), brand.Cmd("plugin install "+name))}
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&to, "to", nil, "Agents to check (default: all it is installed in)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print JSON")
	return cmd
}
