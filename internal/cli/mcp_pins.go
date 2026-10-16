package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/mcpwrap"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// McpPinsCmd returns `zuano mcp pins`, the tool definitions approved for
// servers run with `zuano mcp wrap --offline`.
func McpPinsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pins",
		Short: "List, approve and reject the tool definitions of local servers run offline",
		Long: `A server run with 'zuano mcp wrap --offline' shows the agent only the tool
definitions you approved. A tool the server adds, or one whose definition
changes later, waits here until you approve it. With a gateway the same review
happens in the browser: see 'zuano firewall changes'.`,
	}
	cmd.AddCommand(mcpPinsListCmd(), mcpPinsDecideCmd(true), mcpPinsDecideCmd(false))
	return cmd
}

type pinRow struct {
	Server   string   `json:"server"`
	Tool     string   `json:"tool"`
	State    string   `json:"state"`
	Hash     string   `json:"hash"`
	Findings []string `json:"findings,omitempty"`
}

func loadPinRows(servers []string) ([]pinRow, error) {
	store, _, _, err := mcpDirs()
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		if servers, err = store.Names(); err != nil {
			return nil, err
		}
	}
	var rows []pinRow
	for _, s := range servers {
		set, err := store.Load(s)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(set.Tools))
		for n := range set.Tools {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := set.Tools[n]
			def, hash := p.Def, p.Hash
			if p.SeenDef != nil {
				def, hash = p.SeenDef, p.Seen
			}
			row := pinRow{Server: s, Tool: n, State: p.State, Hash: hash}
			for _, f := range mcpwrap.ScreenDefinition(def) {
				row.Findings = append(row.Findings, f.String())
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func mcpPinsListCmd() *cobra.Command {
	var jsonOut, check, changesOnly bool
	cmd := &cobra.Command{
		Use:   "list [server]",
		Short: "List the tools each local server has shown and their state",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rows, err := loadPinRows(args)
			if err != nil {
				return err
			}
			if changesOnly {
				kept := rows[:0]
				for _, r := range rows {
					if r.State != mcpwrap.StatePinned && r.State != mcpwrap.StateRejected && r.State != mcpwrap.StateRemoved {
						kept = append(kept, r)
					}
				}
				rows = kept
			}
			out := cmd.OutOrStdout()
			switch {
			case jsonOut:
				if err := json.NewEncoder(out).Encode(map[string]any{"pins": rows}); err != nil {
					return err
				}
			case len(rows) == 0:
				fmt.Fprintln(out, "No tools to show. A server run with `"+brand.Cmd("mcp wrap --offline")+" -- <command>` records the tools it lists.")
			default:
				maps := make([]map[string]any, len(rows))
				for i, r := range rows {
					maps[i] = map[string]any{"server": r.Server, "tool": r.Tool, "state": r.State, "hash": shortHash(r.Hash), "findings": strings.Join(r.Findings, "; ")}
				}
				printTable(out, maps, []column{{"SERVER", "server"}, {"TOOL", "tool"}, {"STATE", "state"}, {"HASH", "hash"}, {"FINDINGS", "findings"}})
			}
			if check {
				for _, r := range rows {
					if r.State == mcpwrap.StateQuarantined {
						return &ExitError{Code: exitToolChanged, Msg: fmt.Sprintf("%s changed the definition of %s", textsafe.Clean(r.Server), textsafe.Clean(r.Tool))}
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the tools as JSON")
	cmd.Flags().BoolVar(&check, "check", false, "Exit with status 43 when a server changed a tool it was approved with")
	cmd.Flags().BoolVar(&changesOnly, "changes", false, "Only tools that wait for a decision")
	return cmd
}

func mcpPinsDecideCmd(approve bool) *cobra.Command {
	verb, summary := "reject", "Keep a tool away from the agent"
	if approve {
		verb, summary = "approve", "Let the agent see the definition a local server showed"
	}
	var all, yes bool
	cmd := &cobra.Command{
		Use:   verb + " <server> [tool...]",
		Short: summary,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("mcp pins " + verb)); err != nil {
				return err
			}
			server, tools := args[0], args[1:]
			store, _, _, err := mcpDirs()
			if err != nil {
				return err
			}
			set, err := store.Load(server)
			if err != nil {
				return err
			}
			if all {
				for n, p := range set.Tools {
					if p.State == mcpwrap.StatePending || p.State == mcpwrap.StateQuarantined {
						tools = append(tools, n)
					}
				}
				sort.Strings(tools)
			}
			if len(tools) == 0 {
				return errors.New("name the tools to " + verb + ", or use --all for every tool that waits")
			}
			out := cmd.OutOrStdout()
			for _, t := range tools {
				p := set.Tools[t]
				if p == nil {
					return fmt.Errorf("%s has not shown a tool named %q", textsafe.Clean(server), textsafe.Clean(t))
				}
				def := p.Def
				if p.SeenDef != nil {
					def = p.SeenDef
				}
				fmt.Fprintf(out, "%s: %s\n", textsafe.Clean(t), textsafe.Clean(fmt.Sprint(def["description"])))
				for _, f := range mcpwrap.ScreenDefinition(def) {
					fmt.Fprintf(out, "  finding: %s\n", f)
				}
			}
			if approve {
				if err := confirmOrYes(cmd, yes, fmt.Sprintf("Let the agent use these %d tool(s) from %s?", len(tools), textsafe.Clean(server))); err != nil {
					return err
				}
			}
			for _, t := range tools {
				var err error
				if approve {
					err = set.Approve(t)
				} else {
					err = set.Reject(t)
				}
				if err != nil {
					return err
				}
			}
			if err := store.Save(set); err != nil {
				return err
			}
			fmt.Fprintf(out, "%sd %d tool(s).\n", verb, len(tools))
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Every tool of the server that waits for a decision")
	if approve {
		cmd.Flags().BoolVar(&yes, "yes", false, "Approve without asking (refused when ZUANO_AGENT is set)")
	}
	return cmd
}

// McpChangesCmd returns `zuano mcp changes`: the local servers' tools that
// wait for a decision.
func McpChangesCmd() *cobra.Command {
	cmd := mcpPinsListCmd()
	cmd.Use = "changes [server]"
	cmd.Short = "Show tools of local servers that were added or changed and wait for a decision"
	_ = cmd.Flags().Set("changes", "true")
	_ = cmd.Flags().MarkHidden("changes")
	return cmd
}
