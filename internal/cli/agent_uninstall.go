package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// AgentUninstallCmd returns the `relay agent uninstall <name>` cobra
// command — removes an agent's flat "<name>.md" file from a provider's
// directory and drops any matching Kind: agent manifest ledger entries.
// The Agent-IR analogue of SkillUninstallCmd.
func AgentUninstallCmd() *cobra.Command {
	var (
		fromFlag  string
		scopeFlag string
		yesFlag   bool
	)
	cmd := &cobra.Command{
		Use:   "uninstall <name>",
		Short: "Remove an installed agent from a provider's directory",
		Long: `Remove <name>'s flat "<name>.md" file from --from's directory in the
given --scope, and drop any matching Kind: agent manifest ledger entries.
Refuses if the agent isn't found. Prompts for confirmation on an
interactive terminal unless --yes is set.

Supported providers: claude, opencode.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentUninstall(cmd, args[0], agentUninstallOpts{from: fromFlag, scope: scopeFlag, yes: yesFlag})
		},
	}
	cmd.Flags().StringVar(&fromFlag, "from", "", "Provider to remove from (required)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope: user or project")
	cmd.Flags().BoolVar(&yesFlag, "yes", false, "Skip the confirmation prompt")
	return cmd
}

type agentUninstallOpts struct {
	from  string
	scope string
	yes   bool
}

func runAgentUninstall(cmd *cobra.Command, name string, opts agentUninstallOpts) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}
	if opts.from == "" {
		return fmt.Errorf("--from is required (claude or opencode)")
	}
	a, ok := agentport.AgentAdapterByID(agentport.ProviderID(opts.from))
	if !ok {
		return fmt.Errorf("unknown --from agent provider %q", opts.from)
	}

	// Uninstall is destructive, so it MUST resolve only within the
	// provider's own writable directory — see ResolveOwnAgentPath's doc
	// comment (mirrors ResolveOwnSkillPath's rationale exactly).
	agentPath, err := agentport.ResolveOwnAgentPath(a, scope, name)
	if err != nil {
		return fmt.Errorf("uninstall %s: %w", name, err)
	}

	out := cmd.OutOrStdout()
	if !opts.yes && isInteractiveTerminalFn(os.Stdin) {
		if !confirmPrompt(out, fmt.Sprintf("Remove %s (%s, %s scope) at %s?", name, a.ID(), scope, agentPath)) {
			fmt.Fprintln(out, "skipped")
			return nil
		}
	}

	if err := os.Remove(agentPath); err != nil {
		return fmt.Errorf("remove %s: %w", agentPath, err)
	}

	removed, err := agentport.RemoveEntriesFor(name, a.ID(), scope, agentport.KindAgent)
	if err != nil {
		return fmt.Errorf("update manifest: %w", err)
	}

	fmt.Fprintf(out, "removed: %s\n", agentPath)
	if removed > 0 {
		fmt.Fprintf(out, "manifest: dropped %d entr%s\n", removed, plural(removed))
	}
	return nil
}
