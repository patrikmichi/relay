package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// isInteractiveTerminalFn is a test seam for isInteractiveTerminal (the
// per-platform termios probe in tty_*.go): the agent-manager write verbs
// call it through this package-level variable instead of the function
// directly, so tests can override it to exercise the interactive
// confirmation-prompt branch without an actual pty attached to the test
// process's stdin.
var isInteractiveTerminalFn = isInteractiveTerminal

// AgentMigrateCmd returns the `relay agent migrate <name>` cobra command —
// the Agent-IR analogue of SkillMigrateCmd. It loads an agent by name from
// --from's directory (in the given --scope), projects it to one or more
// --to providers, prints a fidelity-loss report for each, and (unless
// --dry-run) writes the projected file and records a manifest ledger entry
// with Kind: agent.
func AgentMigrateCmd() *cobra.Command {
	var (
		fromFlag  string
		toFlags   []string
		scopeFlag string
		dryRun    bool
		strict    bool
	)

	cmd := &cobra.Command{
		Use:   "migrate <name>",
		Short: "Migrate an agent from one provider to another",
		Long: `Load an agent by name from --from's directory (in the given --scope),
project it to one or more --to providers, print a fidelity-loss report, and
(unless --dry-run) write the projected file and record a manifest entry.

If --to is omitted, migrates to every agent provider detected as installed
on this machine (excluding --from).

Supported providers: claude, opencode. Codex has no single agent-file
primitive (TOML profile != md agent) and Cursor has no subagent primitive —
neither is supported as an agent migration target.

Examples:
  relay agent migrate reviewer --from claude --to opencode
  relay agent migrate reviewer --from opencode --scope project --dry-run
  relay agent migrate reviewer --from claude --strict`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentMigrate(cmd, args[0], agentMigrateOpts{
				from:   fromFlag,
				to:     toFlags,
				scope:  scopeFlag,
				dryRun: dryRun,
				strict: strict,
			})
		},
	}

	cmd.Flags().StringVar(&fromFlag, "from", "", "Source provider: claude or opencode (required)")
	cmd.Flags().StringSliceVar(&toFlags, "to", nil, "Target provider(s); repeatable. Default: all detected agent providers except --from")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to search/write: user or project")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the fidelity report; do not write the file")
	cmd.Flags().BoolVar(&strict, "strict", false, "Abort if any field would be dropped")

	return cmd
}

type agentMigrateOpts struct {
	from   string
	to     []string
	scope  string
	dryRun bool
	strict bool
}

func runAgentMigrate(cmd *cobra.Command, name string, opts agentMigrateOpts) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}

	if opts.from == "" {
		return fmt.Errorf("--from is required (claude or opencode)")
	}
	fromAdapter, ok := agentport.AgentAdapterByID(agentport.ProviderID(opts.from))
	if !ok {
		return fmt.Errorf("unknown --from agent provider %q", opts.from)
	}

	agentPath, err := agentport.ResolveAgentPath(fromAdapter, scope, name)
	if err != nil {
		return err
	}
	src, err := fromAdapter.Load(agentPath)
	if err != nil {
		return fmt.Errorf("load %s from %s: %w", name, opts.from, err)
	}

	targets, err := resolveAgentMigrateTargets(fromAdapter, opts.to)
	if err != nil {
		return err
	}

	return applyAgentMigrationToTargets(cmd.OutOrStdout(), src, string(fromAdapter.ID()), targets, scope, opts.dryRun, opts.strict)
}

// applyAgentMigrationToTargets projects src into each target (via
// agentport.MigrateAgent), printing a fidelity-loss report for each, and
// (unless dryRun) writes the projected file and records a manifest entry —
// the Agent-IR analogue of applyMigrationToTargets.
func applyAgentMigrationToTargets(out io.Writer, src *agentport.Agent, fromLabel string, targets []agentport.AgentAdapter, scope agentport.Scope, dryRun, strict bool) error {
	for _, target := range targets {
		plan, err := agentport.MigrateAgent(src, target, scope)
		if err != nil {
			return fmt.Errorf("migrate to %s: %w", target.ID(), err)
		}

		fmt.Fprintf(out, "\n== %s -> %s (%s scope) ==\n", fromLabel, target.ID(), scope)
		fmt.Fprintf(out, "target: %s\n", plan.TargetPaths)
		printLossReport(out, plan.Loss)

		if strict && plan.HasDropped() {
			return fmt.Errorf("aborting (--strict): %s -> %s would drop one or more fields", fromLabel, target.ID())
		}

		if dryRun {
			fmt.Fprintln(out, "[dry-run] no files written")
			continue
		}

		if plan.HasDropped() && isInteractiveTerminalFn(os.Stdin) {
			if !confirmPrompt(out, fmt.Sprintf("Proceed with %s -> %s despite the fidelity loss above?", fromLabel, target.ID())) {
				fmt.Fprintln(out, "skipped")
				continue
			}
		}

		if err := agentport.WriteAgent(plan); err != nil {
			return fmt.Errorf("write %s -> %s: %w", fromLabel, target.ID(), err)
		}
		fmt.Fprintf(out, "written: %s\n", plan.TargetPaths)
	}
	return nil
}

// resolveAgentMigrateTargets resolves the --to flag values to agent
// adapters, or — if --to was omitted — every detected agent provider
// except from — the Agent-IR analogue of resolveMigrateTargets.
func resolveAgentMigrateTargets(from agentport.AgentAdapter, to []string) ([]agentport.AgentAdapter, error) {
	if len(to) == 0 {
		var targets []agentport.AgentAdapter
		for _, a := range agentport.AgentDetectedProviders() {
			if a.ID() != from.ID() {
				targets = append(targets, a)
			}
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("no other agent providers detected on this machine — specify --to explicitly")
		}
		return targets, nil
	}

	targets := make([]agentport.AgentAdapter, 0, len(to))
	for _, t := range to {
		a, ok := agentport.AgentAdapterByID(agentport.ProviderID(t))
		if !ok {
			return nil, fmt.Errorf("unknown --to agent provider %q", t)
		}
		targets = append(targets, a)
	}
	return targets, nil
}
