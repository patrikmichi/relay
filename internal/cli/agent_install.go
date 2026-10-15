package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
	"github.com/patrikmichi/relay/internal/catalog"
	"github.com/patrikmichi/relay/internal/client"
)

// AgentInstallCmd installs a local Claude agent file or a governed catalog bundle.
func AgentInstallCmd() *cobra.Command {
	var (
		toFlags    []string
		scopeFlag  string
		dryRun     bool
		strict     bool
		acceptLoss bool
		version    string
		channel    string
		gatewayURL string
	)

	cmd := &cobra.Command{
		Use:   "install <path|catalog-id>",
		Short: "Install an agent from a local file or the gateway catalog",
		Long: `Install a local Claude Markdown agent file or a governed catalog agent bundle.
Local files work offline. Catalog bundles must contain a single root Markdown agent.
All targets use migration fidelity checks, overwrite protection, history and rollback.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentInstall(cmd, args[0], agentInstallOpts{
				to:         toFlags,
				scope:      scopeFlag,
				dryRun:     dryRun,
				strict:     strict,
				acceptLoss: acceptLoss,
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
	cmd.Flags().BoolVar(&acceptLoss, "accept-loss", false, "Accept non-security fidelity loss")
	cmd.Flags().StringVar(&version, "version", "", "Exact semver (default: latest on --channel)")
	cmd.Flags().StringVar(&channel, "channel", "", "stable or beta (default: gateway's default channel)")
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, then the config file)")

	return cmd
}

type agentInstallOpts struct {
	to         []string
	scope      string
	dryRun     bool
	strict     bool
	acceptLoss bool
	version    string
	channel    string
	gatewayURL string
}

func runAgentInstall(cmd *cobra.Command, catalogID string, opts agentInstallOpts) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}

	var src *agentport.Agent
	label := "gateway"
	local, err := classifyInstallSource(catalogID)
	if err != nil {
		return err
	}
	if local {
		label = "local"
		if opts.version != "" || opts.channel != "" {
			return fmt.Errorf("--version and --channel apply only to catalog installs")
		}
		adapter, ok := agentport.AgentAdapterByID("claude")
		if !ok {
			return fmt.Errorf("claude agent adapter unavailable")
		}
		src, err = adapter.Load(catalogID)
	} else {
		src, err = fetchAgentFromGateway(catalogID, opts)
	}
	if err != nil {
		return err
	}

	targets, err := resolveAgentInstallTargets(opts.to)
	if err != nil {
		return err
	}

	return applyAgentMigrationToTargets(cmd.OutOrStdout(), src, label, targets, scope, opts.dryRun, opts.strict, opts.acceptLoss)
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
			return nil, fmt.Errorf("unsupported agent provider %q", t)
		}
		targets = append(targets, a)
	}
	return targets, nil
}
