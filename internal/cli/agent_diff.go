package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// AgentDiffCmd returns the `relay agent diff <name>` cobra command — a
// preview of `relay agent migrate` that writes nothing. The Agent-IR
// analogue of SkillDiffCmd.
func AgentDiffCmd() *cobra.Command {
	var (
		fromFlag  string
		toFlag    string
		scopeFlag string
	)

	cmd := &cobra.Command{
		Use:   "diff <name>",
		Short: "Preview an agent migration: field-level differences + fidelity-loss report, without writing",
		Long: `Load <name> from --from's directory (in the given --scope), project it
to --to, and print the field-level differences plus the fidelity-loss
report — without writing anything to --to's real agent directory or
recording a manifest entry. This is a preview of 'relay agent migrate'.

Supported providers: claude, opencode.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentDiff(cmd, args[0], agentDiffOpts{from: fromFlag, to: toFlag, scope: scopeFlag})
		},
	}

	cmd.Flags().StringVar(&fromFlag, "from", "", "Source provider (required)")
	cmd.Flags().StringVar(&toFlag, "to", "", "Target provider (required)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to search: user or project")

	return cmd
}

type agentDiffOpts struct {
	from  string
	to    string
	scope string
}

func runAgentDiff(cmd *cobra.Command, name string, opts agentDiffOpts) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}
	if opts.from == "" || opts.to == "" {
		return fmt.Errorf("both --from and --to are required")
	}

	fromAdapter, ok := agentport.AgentAdapterByID(agentport.ProviderID(opts.from))
	if !ok {
		return fmt.Errorf("unknown --from agent provider %q", opts.from)
	}
	toAdapter, ok := agentport.AgentAdapterByID(agentport.ProviderID(opts.to))
	if !ok {
		return fmt.Errorf("unknown --to agent provider %q", opts.to)
	}

	agentPath, err := agentport.ResolveAgentPath(fromAdapter, scope, name)
	if err != nil {
		return err
	}
	src, err := fromAdapter.Load(agentPath)
	if err != nil {
		return fmt.Errorf("load %s from %s: %w", name, opts.from, err)
	}

	plan, err := agentport.MigrateAgent(src, toAdapter, scope)
	if err != nil {
		return fmt.Errorf("migrate to %s: %w", opts.to, err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "== %s -> %s (%s scope, preview only) ==\n", fromAdapter.ID(), toAdapter.ID(), scope)
	fmt.Fprintf(out, "would write to: %s\n", plan.TargetPaths)
	printLossReport(out, plan.Loss)

	// Project into a scratch temp dir (never plan.TargetPaths) so we can
	// re-Load it and report field-level differences without writing
	// anything under the real target directory or touching the manifest.
	tmp, err := os.MkdirTemp("", "relay-agent-diff-*")
	if err != nil {
		return fmt.Errorf("create scratch dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := writeScratchFiles(tmp, plan.Files); err != nil {
		return fmt.Errorf("write scratch files: %w", err)
	}

	projected, err := toAdapter.Load(filepath.Join(tmp, src.Name+".md"))
	if err != nil {
		return fmt.Errorf("re-load projected agent: %w", err)
	}

	diffs := agentport.AgentFieldDiff(src, projected)
	if len(diffs) == 0 {
		fmt.Fprintln(out, "field diff: no differences")
		return nil
	}
	fmt.Fprintln(out, "field diff:")
	for _, d := range diffs {
		fmt.Fprintf(out, "  %s\n", d)
	}
	return nil
}
