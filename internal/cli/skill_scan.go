package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// SkillScanCmd returns the `relay skill scan <name>` cobra command — a
// thin wrapper over agentport.Scan.
func SkillScanCmd() *cobra.Command {
	var (
		fromFlag  string
		scopeFlag string
	)
	cmd := &cobra.Command{
		Use:   "scan <name>",
		Short: "Scan a skill for dangerous shell patterns and hardcoded secrets",
		Long: `Load <name> from --from's directory (in the given --scope) and run a
deterministic, local, no-network scan of its frontmatter, body, resource
files, and provider sidecars for dangerous shell patterns (curl|bash,
rm -rf, ...) and obvious hardcoded credentials. Prints findings, exactly
what was scanned, anything skipped and why (binary resources, excluded
files), and a heuristic quality score — not an execution-safety
certification. Exits non-zero if any finding has "high" severity.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillScan(cmd, args[0], fromFlag, scopeFlag)
		},
	}
	cmd.Flags().StringVar(&fromFlag, "from", "", "Provider to load from (required)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to search: user or project")
	return cmd
}

// SkillScoreCmd returns the `relay skill score <name>` cobra command — a
// thin wrapper over agentport.Scan's quality score, sharing the same
// loader as `relay skill scan`.
func SkillScoreCmd() *cobra.Command {
	var (
		fromFlag  string
		scopeFlag string
	)
	cmd := &cobra.Command{
		Use:   "score <name>",
		Short: "Print a skill's quality score",
		Long: `Load <name> from --from's directory (in the given --scope) and print its
deterministic 0-100 heuristic quality score (description/body completeness,
minus a penalty per scan finding). This is a lint score, not an
execution-safety or trust certification — see 'relay skill scan' for full
coverage accounting. Thin wrapper over the same scan engine as
'relay skill scan'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillScore(cmd, args[0], fromFlag, scopeFlag)
		},
	}
	cmd.Flags().StringVar(&fromFlag, "from", "", "Provider to load from (required)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to search: user or project")
	return cmd
}

// loadSkillFromProvider is shared by scan/score/uninstall-adjacent commands
// that just need to resolve+load one named skill from a provider's
// directory (no migration involved).
func loadSkillFromProvider(from, scopeStr, name string) (*agentport.Skill, error) {
	scope, err := parseScope(scopeStr)
	if err != nil {
		return nil, err
	}
	if from == "" {
		return nil, fmt.Errorf("--from is required (%s)", providerIDsOxford())
	}
	a, ok := agentport.AdapterByID(agentport.ProviderID(from))
	if !ok {
		return nil, fmt.Errorf("unknown --from provider %q", from)
	}
	skillPath, err := agentport.ResolveSkillPath(a, scope, name)
	if err != nil {
		return nil, err
	}
	return a.Load(skillPath)
}

func runSkillScan(cmd *cobra.Command, name, from, scopeStr string) error {
	s, err := loadSkillFromProvider(from, scopeStr, name)
	if err != nil {
		return err
	}
	result := agentport.Scan(s)
	out := cmd.OutOrStdout()

	if len(result.Findings) == 0 {
		fmt.Fprintln(out, "no findings (no configured heuristic pattern matched — not a safety or trust certification)")
	} else {
		fmt.Fprintln(out, "findings:")
		for _, f := range result.Findings {
			fmt.Fprintf(out, "  [%s] %s in %s: %s\n", f.Severity, f.Pattern, f.File, f.Excerpt)
		}
	}
	fmt.Fprintf(out, "scanned (%d): %s\n", len(result.Scanned), strings.Join(result.Scanned, ", "))
	if len(result.Skipped) == 0 {
		fmt.Fprintln(out, "skipped: none")
	} else {
		fmt.Fprintln(out, "skipped:")
		for _, sk := range result.Skipped {
			fmt.Fprintf(out, "  %s: %s\n", sk.File, sk.Reason)
		}
	}
	fmt.Fprintf(out, "score: %d/100 (%s)\n", result.Score, agentport.ScoreDisclaimer)

	for _, f := range result.Findings {
		if f.Severity == "high" {
			return fmt.Errorf("scan found a high-severity issue: %s in %s", f.Pattern, f.File)
		}
	}
	return nil
}

func runSkillScore(cmd *cobra.Command, name, from, scopeStr string) error {
	s, err := loadSkillFromProvider(from, scopeStr, name)
	if err != nil {
		return err
	}
	result := agentport.Scan(s)
	fmt.Fprintf(cmd.OutOrStdout(), "%d\n", result.Score)
	fmt.Fprintln(cmd.ErrOrStderr(), agentport.ScoreDisclaimer)
	return nil
}
