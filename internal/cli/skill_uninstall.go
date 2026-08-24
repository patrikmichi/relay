package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// SkillUninstallCmd returns the `relay skill uninstall <name>` cobra
// command — removes a skill's files from a provider's directory and drops
// any matching manifest ledger entries.
func SkillUninstallCmd() *cobra.Command {
	var (
		fromFlag  string
		scopeFlag string
		yesFlag   bool
	)
	cmd := &cobra.Command{
		Use:   "uninstall <name>",
		Short: "Remove an installed skill from a provider's directory",
		Long: `Remove <name>'s files (the <name>/ directory, or a legacy flat <name>.md
command file) from --from's directory in the given --scope, and drop any
matching manifest ledger entries. Refuses if the skill isn't found. Prompts
for confirmation on an interactive terminal unless --yes is set.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillUninstall(cmd, args[0], skillUninstallOpts{from: fromFlag, scope: scopeFlag, yes: yesFlag})
		},
	}
	cmd.Flags().StringVar(&fromFlag, "from", "", "Provider to remove from (required)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope: user or project")
	cmd.Flags().BoolVar(&yesFlag, "yes", false, "Skip the confirmation prompt")
	return cmd
}

type skillUninstallOpts struct {
	from  string
	scope string
	yes   bool
}

func runSkillUninstall(cmd *cobra.Command, name string, opts skillUninstallOpts) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}
	if opts.from == "" {
		return fmt.Errorf("--from is required (claude, codex, opencode, or cursor)")
	}
	a, ok := agentport.AdapterByID(agentport.ProviderID(opts.from))
	if !ok {
		return fmt.Errorf("unknown --from provider %q", opts.from)
	}

	// Uninstall is destructive, so it MUST resolve only within the
	// provider's own writable directory (never another provider's
	// compatibility-read fallback, and never a read-only admin directory
	// like Codex's /etc/codex/skills) — see ResolveOwnSkillPath's doc
	// comment for why ResolveSkillPath is unsafe here.
	skillPath, err := agentport.ResolveOwnSkillPath(a, scope, name)
	if err != nil {
		return fmt.Errorf("uninstall %s: %w", name, err)
	}

	out := cmd.OutOrStdout()
	if !opts.yes && isInteractiveTerminal(os.Stdin) {
		if !confirmPrompt(out, fmt.Sprintf("Remove %s (%s, %s scope) at %s?", name, a.ID(), scope, skillPath)) {
			fmt.Fprintln(out, "skipped")
			return nil
		}
	}

	fi, err := os.Stat(skillPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", skillPath, err)
	}
	if fi.IsDir() {
		if err := os.RemoveAll(skillPath); err != nil {
			return fmt.Errorf("remove %s: %w", skillPath, err)
		}
	} else if err := os.Remove(skillPath); err != nil {
		return fmt.Errorf("remove %s: %w", skillPath, err)
	}

	removed, err := agentport.RemoveEntriesFor(name, a.ID(), scope, agentport.KindSkill)
	if err != nil {
		return fmt.Errorf("update manifest: %w", err)
	}

	fmt.Fprintf(out, "removed: %s\n", skillPath)
	if removed > 0 {
		fmt.Fprintf(out, "manifest: dropped %d entr%s\n", removed, plural(removed))
	}
	return nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
