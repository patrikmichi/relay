package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// ServicesCmd returns the `zuano services` cobra command.
func ServicesCmd() *cobra.Command {
	var (
		gatewayURL string
		noStatus   bool
	)

	cmd := &cobra.Command{
		Use:   "services",
		Short: "List available gateway services and whether your account is connected",
		Long: `Calls GET /api/integrations and prints the services accessible to the current session.

Each service that takes credentials is followed by its connection status:
connected, not connected, or not connected (admin) when an administrator
manages the shared credentials. Link your own account with 'zuano connect
<slug>'; browse everything on offer with 'zuano connectors'.`,
		RunE: func(cmd *cobra.Command, _args []string) error {
			gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}

			c, err := resolveClient(gURL)
			if err != nil {
				return err
			}

			info, err := fetchIntegrations(c, cmd.Context())
			if err != nil {
				return fmt.Errorf("services failed: %w", err)
			}
			if info == nil {
				return errTokenRevoked
			}

			var links map[string]serviceLink
			if !noStatus {
				links = fetchServiceLinks(cmd.Context(), c, info.Services)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Services (%d tools total):\n", info.ToolCount)
			for _, svc := range info.Services {
				marker := ""
				switch {
				case !svc.Accessible:
					marker = "  (no access)"
				case !noStatus && takesCredentials(svc):
					marker = "  " + linkLabel(svc, links)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %-20s %3d tools%s\n", svc.ID, svc.ToolCount, marker)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	cmd.Flags().BoolVar(&noStatus, "no-status", false, "Skip the per-service connection status probes")
	return cmd
}
