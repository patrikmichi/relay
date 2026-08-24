package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// SkillMigrateCmd returns the `relay skill migrate <name>` cobra command —
// the flagship agentport command. It loads a skill by name from --from's
// directory (in the given --scope), projects it to one or more --to
// providers, prints a fidelity-loss report for each, and (unless --dry-run)
// writes the projected files and records a manifest ledger entry.
func SkillMigrateCmd() *cobra.Command {
	var (
		fromFlag  string
		toFlags   []string
		scopeFlag string
		dryRun    bool
		strict    bool
	)

	cmd := &cobra.Command{
		Use:   "migrate <name>",
		Short: "Migrate a skill from one agent-skill provider to another",
		Long: `Load a skill by name from --from's directory (in the given --scope),
project it to one or more --to providers, print a fidelity-loss report, and
(unless --dry-run) write the projected files and record a manifest entry.

If --to is omitted, migrates to every provider detected as installed on this
machine (excluding --from).

Supported providers: claude, codex, opencode, cursor.

Examples:
  relay skill migrate my-skill --from claude --to codex
  relay skill migrate my-skill --from cursor --scope project --dry-run
  relay skill migrate my-skill --from claude --strict`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillMigrate(cmd, args[0], skillMigrateOpts{
				from:   fromFlag,
				to:     toFlags,
				scope:  scopeFlag,
				dryRun: dryRun,
				strict: strict,
			})
		},
	}

	cmd.Flags().StringVar(&fromFlag, "from", "", "Source provider: claude, codex, opencode, or cursor (required)")
	cmd.Flags().StringSliceVar(&toFlags, "to", nil, "Target provider(s); repeatable. Default: all detected providers except --from")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to search/write: user or project")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the fidelity report; do not write files")
	cmd.Flags().BoolVar(&strict, "strict", false, "Abort if any field would be dropped")

	return cmd
}

type skillMigrateOpts struct {
	from   string
	to     []string
	scope  string
	dryRun bool
	strict bool
}

func runSkillMigrate(cmd *cobra.Command, name string, opts skillMigrateOpts) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}

	if opts.from == "" {
		return fmt.Errorf("--from is required (claude, codex, opencode, or cursor)")
	}
	fromAdapter, ok := agentport.AdapterByID(agentport.ProviderID(opts.from))
	if !ok {
		return fmt.Errorf("unknown --from provider %q", opts.from)
	}

	skillPath, err := agentport.ResolveSkillPath(fromAdapter, scope, name)
	if err != nil {
		return err
	}
	src, err := fromAdapter.Load(skillPath)
	if err != nil {
		return fmt.Errorf("load %s from %s: %w", name, opts.from, err)
	}

	targets, err := resolveMigrateTargets(fromAdapter, opts.to)
	if err != nil {
		return err
	}

	return applyMigrationToTargets(cmd.OutOrStdout(), src, string(fromAdapter.ID()), targets, scope, opts.dryRun, opts.strict)
}

// applyMigrationToTargets projects src into each target (via
// agentport.Migrate), printing a fidelity-loss report for each, and
// (unless dryRun) writes the projected files and records a manifest entry.
// Shared by `relay skill migrate` and `relay skill install` — the only
// difference between the two commands is how src was loaded (a named skill
// from a provider's directory vs. an arbitrary local path). fromLabel is
// used only in the printed "== fromLabel -> target ==" header.
func applyMigrationToTargets(out io.Writer, src *agentport.Skill, fromLabel string, targets []agentport.Adapter, scope agentport.Scope, dryRun, strict bool) error {
	for _, target := range targets {
		plan, err := agentport.Migrate(src, target, scope)
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

		if plan.HasDropped() && isInteractiveTerminal(os.Stdin) {
			if !confirmPrompt(out, fmt.Sprintf("Proceed with %s -> %s despite the fidelity loss above?", fromLabel, target.ID())) {
				fmt.Fprintln(out, "skipped")
				continue
			}
		}

		if err := agentport.Write(plan); err != nil {
			return fmt.Errorf("write %s -> %s: %w", fromLabel, target.ID(), err)
		}
		fmt.Fprintf(out, "written: %s\n", plan.TargetPaths)
	}
	return nil
}

func parseScope(s string) (agentport.Scope, error) {
	switch s {
	case "", "user":
		return agentport.ScopeUser, nil
	case "project":
		return agentport.ScopeProject, nil
	default:
		return "", fmt.Errorf("invalid --scope %q: must be user or project", s)
	}
}

// resolveMigrateTargets resolves the --to flag values to adapters, or — if
// --to was omitted — every detected provider except from.
func resolveMigrateTargets(from agentport.Adapter, to []string) ([]agentport.Adapter, error) {
	if len(to) == 0 {
		var targets []agentport.Adapter
		for _, a := range agentport.DetectedProviders() {
			if a.ID() != from.ID() {
				targets = append(targets, a)
			}
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("no other providers detected on this machine — specify --to explicitly")
		}
		return targets, nil
	}

	targets := make([]agentport.Adapter, 0, len(to))
	for _, t := range to {
		a, ok := agentport.AdapterByID(agentport.ProviderID(t))
		if !ok {
			return nil, fmt.Errorf("unknown --to provider %q", t)
		}
		targets = append(targets, a)
	}
	return targets, nil
}

func printLossReport(out io.Writer, loss []agentport.LossItem) {
	if len(loss) == 0 {
		fmt.Fprintln(out, "fidelity: no loss — all fields preserved")
		return
	}
	fmt.Fprintln(out, "fidelity report:")
	for _, l := range loss {
		fmt.Fprintf(out, "  [%s] %s — %s\n", l.Kind, l.Field, l.Note)
	}
}

// isInteractiveTerminal (platform-specific implementation in
// tty_darwin.go/tty_linux.go/tty_fallback.go) reports whether f is attached
// to an interactive terminal — used to decide whether to prompt before
// writing a lossy migration.

// confirmPrompt asks a yes/no question on out/stdin. Defaults to "no" on any
// input other than y/yes.
func confirmPrompt(out io.Writer, prompt string) bool {
	fmt.Fprintf(out, "%s [y/N] ", prompt)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}
