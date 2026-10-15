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
		fromFlag   string
		toFlags    []string
		scopeFlag  string
		dryRun     bool
		strict     bool
		acceptLoss bool
	)

	cmd := &cobra.Command{
		Use:   "migrate <name>",
		Short: "Migrate an agent from one provider to another",
		Long: fmt.Sprintf(`Load an agent by name from --from's directory (in the given --scope),
project it to one or more --to providers, print a fidelity-loss report, and
(unless --dry-run) write the projected file and record a manifest entry.

If --to is omitted, migrates to every agent provider detected as installed
on this machine (excluding --from).

Supported providers: %s.

Examples:
  relay agent migrate reviewer --from claude --to opencode
  relay agent migrate reviewer --from opencode --scope project --dry-run
  relay agent migrate reviewer --from claude --strict`, agentProviderIDsCSV()),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentMigrate(cmd, args[0], agentMigrateOpts{
				from:       fromFlag,
				to:         toFlags,
				scope:      scopeFlag,
				dryRun:     dryRun,
				strict:     strict,
				acceptLoss: acceptLoss,
			})
		},
	}

	cmd.Flags().StringVar(&fromFlag, "from", "", fmt.Sprintf("Source provider: %s (required)", agentProviderIDsOxford()))
	cmd.Flags().StringSliceVar(&toFlags, "to", nil, "Target provider(s); repeatable. Default: all detected agent providers except --from")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to search/write: user or project")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the fidelity report; do not write the file")
	cmd.Flags().BoolVar(&strict, "strict", false, "Abort if any field would be dropped")
	cmd.Flags().BoolVar(&acceptLoss, "accept-loss", false, "Proceed despite non-security fidelity loss without an interactive prompt")

	return cmd
}

type agentMigrateOpts struct {
	from       string
	to         []string
	scope      string
	dryRun     bool
	strict     bool
	acceptLoss bool
}

func runAgentMigrate(cmd *cobra.Command, name string, opts agentMigrateOpts) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}

	if opts.from == "" {
		return fmt.Errorf("--from is required (%s)", agentProviderIDsOxford())
	}
	fromAdapter, ok := agentport.AgentAdapterByID(agentport.ProviderID(opts.from))
	if !ok {
		return unsupportedAgentProviderError(opts.from)
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

	return applyAgentMigrationToTargets(cmd.OutOrStdout(), src, string(fromAdapter.ID()), targets, scope, opts.dryRun, opts.strict, opts.acceptLoss)
}

func applyAgentMigrationToTargets(out io.Writer, src *agentport.Agent, fromLabel string, targets []agentport.AgentAdapter, scope agentport.Scope, dryRun, strict, acceptLoss bool) error {
	var toWrite []*agentport.AgentPlan

	for _, target := range targets {
		plan, err := agentport.MigrateAgent(src, target, scope)
		if err != nil {
			return fmt.Errorf("migrate to %s: %w", target.ID(), err)
		}

		fmt.Fprintf(out, "\n== %s -> %s (%s scope) ==\n", fromLabel, target.ID(), scope)
		fmt.Fprintf(out, "target: %s\n", plan.TargetPaths)
		printLossReport(out, plan.Loss)

		if agentport.HasSecurityLoss(plan.Loss) {
			return fmt.Errorf("refusing %s -> %s: a permission/restriction field would be lost or degraded and cannot be represented on the target (see [security] items above) — this is not overridable by --strict, --dry-run, or an interactive prompt; migrate to a provider that fully supports it or wait for provider-fidelity mapping", fromLabel, target.ID())
		}

		if strict && plan.HasDropped() {
			return fmt.Errorf("aborting (--strict): %s -> %s would drop one or more fields", fromLabel, target.ID())
		}

		if dryRun {
			fmt.Fprintln(out, "[dry-run] no files written")
			continue
		}

		if plan.HasDropped() && !acceptLoss {
			if !isInteractiveTerminalFn(os.Stdin) {
				return fmt.Errorf("refusing %s -> %s: fidelity loss above requires --accept-loss (or an interactive terminal) to proceed noninteractively — the absence of a terminal is never treated as consent", fromLabel, target.ID())
			}
			if !confirmPrompt(out, fmt.Sprintf("Proceed with %s -> %s despite the fidelity loss above?", fromLabel, target.ID())) {
				fmt.Fprintln(out, "skipped")
				continue
			}
		}

		toWrite = append(toWrite, plan)
	}

	for _, plan := range toWrite {
		if err := agentport.PreflightWriteAgent(plan); err != nil {
			return fmt.Errorf("preflight %s -> %s: %w", fromLabel, plan.Target.ID(), err)
		}
	}
	for _, plan := range toWrite {
		if err := agentport.WriteAgent(plan); err != nil {
			return fmt.Errorf("write %s -> %s: %w", fromLabel, plan.Target.ID(), err)
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
			return nil, unsupportedAgentProviderError(t)
		}
		targets = append(targets, a)
	}
	return targets, nil
}

// unsupportedAgentProviderError names why a provider id has no agent
// adapter — distinguishing "a real skill provider with no agent-management
// support" (cline, windsurf) from "not a provider id at all" — and lists
// the valid agent targets (agentProviderIDsOxford(), a live-derived
// helper) either way, rather than a bare cobra usage dump.
func unsupportedAgentProviderError(id string) error {
	if _, isSkillProvider := agentport.AdapterByID(agentport.ProviderID(id)); isSkillProvider {
		return fmt.Errorf("provider %q supports skills but has no agent-management support; supported agent providers: %s", id, agentProviderIDsOxford())
	}
	return fmt.Errorf("unknown provider %q; supported agent providers: %s", id, agentProviderIDsOxford())
}
