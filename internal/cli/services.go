package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// ServicesCmd returns the `relay services` cobra command.
// Lists available services and their tool counts from GET /api/integrations.
//
// Repointed off GET /api/mcp (aggregate MCP endpoint), which 503s whenever
// AGGREGATE_MCP_ENABLED != 'true' (default OFF — see gateway
// app/api/mcp/route.ts, an intentional kill-switch). /api/integrations is a
// dedicated, always-on, read-only discovery endpoint (auth: verifyMcpAuth,
// same as every MCP surface) — see gateway app/api/integrations/route.ts.
//
// Reuses the shared fetchIntegrations helper (service_cmd.go) instead of
// building its own HTTP request/decode logic, so there is a single client
// for GET /api/integrations to maintain. fetchIntegrations returns
// (nil, nil) specifically (and only) on a 401 — every other failure mode
// returns a non-nil error — so `info == nil` here is an unambiguous signal
// to print the "re-authenticate" prompt and exit(1), matching the previous
// inline behavior.
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

			c := resolveClient(gURL)

			info, err := fetchIntegrations(c)
			if err != nil {
				return fmt.Errorf("services failed: %w", err)
			}
			if info == nil {
				fmt.Fprintln(os.Stderr, "Token expired or revoked — run `relay login` to re-authenticate.")
				os.Exit(1)
			}

			fmt.Printf("Services (%d tools total):\n", info.ToolCount)
			for _, svc := range info.Services {
				marker := ""
				if !svc.Accessible {
					marker = "  (no access)"
				}
				fmt.Printf("  %-20s %3d tools%s\n", svc.ID, svc.ToolCount, marker)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")
	return cmd
}
