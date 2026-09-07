package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
	"github.com/patrikmichi/relay/internal/catalog"
	"github.com/patrikmichi/relay/internal/client"
)

// AgentInstallCmd returns the `relay agent install <catalog-id>` cobra
// command — the Agent-IR analogue of SkillInstallCmd. Unlike skill install,
// there is no local-path source: an agent install ALWAYS resolves
// <catalog-id> against the gateway catalog (catalog.FetchAgent, targeting
// the generalized /api/catalog/resources/<id>/download endpoint added by
// gateway D1/D2), then projects the result into one or more --to providers
// via the same agentport.MigrateAgent/WriteAgent engine `relay agent
// migrate` uses — so a Kind: "agent" manifest entry is recorded per target
// and `relay agent rollback` works unmodified.
//
// If --to is omitted, installs to every agent provider detected as
// installed on this machine.
func AgentInstallCmd() *cobra.Command {
	var (
		toFlags    []string
		scopeFlag  string
		dryRun     bool
		strict     bool
		version    string
		channel    string
		gatewayURL string
	)

	cmd := &cobra.Command{
		Use:   "install <catalog-id>",
		Short: "Install an agent from the gateway catalog",
		Long: `Fetch an agent definition from the gateway catalog by resource id (res_…)
or slug, and project it into one or more --to agent providers, printing a
fidelity-loss report for each. Unless --dry-run, files are written and a
Kind: "agent" manifest entry is recorded per target — the same engine as
'relay agent migrate', so 'relay agent rollback' can undo it.

Requires a reachable, authenticated gateway (GATEWAY_URL/GATEWAY_API_KEY env
vars, or a prior 'relay login') and fails closed — no partial writes — on:
no gateway configured / --offline, a content-hash mismatch, a missing/failed
scan verdict, or a malformed bundle.

If --to is omitted, installs to every agent provider detected as installed
on this machine.

Examples:
  relay agent install res_abc123 --to claude
  relay agent install pr-reviewer --to claude --to opencode
  relay agent install pr-reviewer --version 1.2.0 --channel beta --to claude`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentInstall(cmd, args[0], agentInstallOpts{
				to:         toFlags,
				scope:      scopeFlag,
				dryRun:     dryRun,
				strict:     strict,
				version:    version,
				channel:    channel,
				gatewayURL: gatewayURL,
			})
		},
	}

	cmd.Flags().StringSliceVar(&toFlags, "to", nil, "Target agent provider(s); repeatable. Default: all detected agent providers")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to write to: user or project")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the fidelity report; do not write files")
	cmd.Flags().BoolVar(&strict, "strict", false, "Abort if any field would be dropped")
	cmd.Flags().StringVar(&version, "version", "", "Exact semver (default: latest on --channel)")
	cmd.Flags().StringVar(&channel, "channel", "", "stable or beta (default: gateway's default channel)")
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")

	return cmd
}

type agentInstallOpts struct {
	to         []string
	scope      string
	dryRun     bool
	strict     bool
	version    string
	channel    string
	gatewayURL string
}

func runAgentInstall(cmd *cobra.Command, catalogID string, opts agentInstallOpts) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}

	src, err := fetchAgentFromGateway(catalogID, opts)
	if err != nil {
		return err
	}

	targets, err := resolveAgentInstallTargets(opts.to)
	if err != nil {
		return err
	}

	return applyAgentMigrationToTargets(cmd.OutOrStdout(), src, "gateway", targets, scope, opts.dryRun, opts.strict)
}

// fetchAgentFromGateway resolves an authenticated gateway client and fetches
// catalogID via catalog.FetchAgent, translating auth/offline/gateway-error
// failures into the same fail-closed degradation guidance
// fetchFromGateway (skill_install.go) uses. No files are ever written on
// any of these paths. resolveGatewayURLOrFailClosed itself checks the root
// --offline flag BEFORE this ever reaches client.Resolve.
func fetchAgentFromGateway(catalogID string, opts agentInstallOpts) (*agentport.Agent, error) {
	gURL, err := resolveGatewayURLOrFailClosed(opts.gatewayURL)
	if err != nil {
		return nil, err
	}

	c, err := client.Resolve(gURL)
	if err != nil {
		if errors.Is(err, client.ErrNotLoggedIn) {
			return nil, fmt.Errorf("%s", offlineGuidance)
		}
		return nil, err
	}

	agent, err := catalog.FetchAgent(c, catalogID, opts.version, opts.channel)
	if err != nil {
		return nil, translateGatewayFetchError(catalogID, err)
	}
	return agent, nil
}

// resolveAgentInstallTargets resolves the --to flag values to agent
// adapters, or — if --to was omitted — every detected agent provider.
// Unlike resolveAgentMigrateTargets, there's no --from adapter to exclude:
// an install's source is always the gateway catalog, never one of the
// loaded agent providers itself — the Agent-IR analogue of
// resolveInstallTargets.
func resolveAgentInstallTargets(to []string) ([]agentport.AgentAdapter, error) {
	if len(to) == 0 {
		targets := agentport.AgentDetectedProviders()
		if len(targets) == 0 {
			return nil, fmt.Errorf("no agent providers detected on this machine — specify --to explicitly")
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
