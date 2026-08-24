package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// AgentScanCmd returns the `relay agent scan <name>` cobra command — a
// thin wrapper over agentport.AgentScan, the Agent-IR analogue of
// SkillScanCmd. There is deliberately no `relay agent score` companion
// command (unlike skills' scan/score pair) — see relay-standalone P1.9.
func AgentScanCmd() *cobra.Command {
	var (
		fromFlag  string
		scopeFlag string
	)
	cmd := &cobra.Command{
		Use:   "scan <name>",
		Short: "Scan an agent for dangerous shell patterns and hardcoded secrets",
		Long: `Load <name> from --from's directory (in the given --scope) and run a
deterministic, local, no-network scan of its body for dangerous shell
patterns (curl|bash, rm -rf, ...) and obvious hardcoded credentials. Prints
findings and a quality score; exits non-zero if any finding has "high"
severity.

Supported providers: claude, opencode.`,
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
		return nil, fmt.Errorf("--from is required (claude or opencode)")
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
		fmt.Fprintln(out, "no findings")
	} else {
		fmt.Fprintln(out, "findings:")
		for _, f := range result.Findings {
			fmt.Fprintf(out, "  [%s] %s in %s: %s\n", f.Severity, f.Pattern, f.File, f.Excerpt)
		}
	}
	fmt.Fprintf(out, "score: %d/100\n", result.Score)

	for _, f := range result.Findings {
		if f.Severity == "high" {
			return fmt.Errorf("scan found a high-severity issue: %s in %s", f.Pattern, f.File)
		}
	}
	return nil
}
