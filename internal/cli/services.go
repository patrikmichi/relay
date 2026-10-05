package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// ServicesCmd returns the `relay services` cobra command.
func ServicesCmd() *cobra.Command {
	var gatewayURL string

	cmd := &cobra.Command{
		Use:   "services",
		Short: "List available gateway services",
		Long:  "Calls GET /api/integrations and prints the services accessible to the current session.",
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
				return errors.New("token expired or revoked — run `relay login` to re-authenticate")
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Services (%d tools total):\n", info.ToolCount)
			for _, svc := range info.Services {
				marker := ""
				if !svc.Accessible {
					marker = "  (no access)"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %-20s %3d tools%s\n", svc.ID, svc.ToolCount, marker)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, then the config file)")
	return cmd
}
