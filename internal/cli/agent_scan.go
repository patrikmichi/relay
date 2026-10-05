package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

func AgentScanCmd() *cobra.Command {
	var (
		fromFlag  string
		scopeFlag string
	)
	cmd := &cobra.Command{
		Use:   "scan <name>",
		Short: "Scan an agent for dangerous shell patterns and hardcoded secrets",
		Long: fmt.Sprintf(`Load <name> from --from's directory (in the given --scope) and run a
deterministic, local, no-network scan of its frontmatter and body for
dangerous shell patterns (curl|bash, rm -rf, ...) and obvious hardcoded
credentials. Prints findings, exactly what was scanned, and a heuristic
quality score — not an execution-safety certification. Exits non-zero if
any finding has "high" severity.

Supported providers: %s.`, agentProviderIDsCSV()),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentScan(cmd, args[0], fromFlag, scopeFlag)
		},
	}
	cmd.Flags().StringVar(&fromFlag, "from", "", "Provider to load from (required)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to search: user or project")
	return cmd
}

// loadAgentFromProvider is shared by scan-adjacent commands that just need
// to resolve+load one named agent from a provider's directory (no
// migration involved) — the Agent-IR analogue of loadSkillFromProvider.
func loadAgentFromProvider(from, scopeStr, name string) (*agentport.Agent, error) {
	scope, err := parseScope(scopeStr)
	if err != nil {
		return nil, err
	}
	if from == "" {
		return nil, fmt.Errorf("--from is required (%s)", agentProviderIDsOxford())
	}
	a, ok := agentport.AgentAdapterByID(agentport.ProviderID(from))
	if !ok {
		return nil, fmt.Errorf("unknown --from agent provider %q", from)
	}
	agentPath, err := agentport.ResolveAgentPath(a, scope, name)
	if err != nil {
		return nil, err
	}
	return a.Load(agentPath)
}

func runAgentScan(cmd *cobra.Command, name, from, scopeStr string) error {
	a, err := loadAgentFromProvider(from, scopeStr, name)
	if err != nil {
		return err
	}
	result := agentport.AgentScan(a)
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
