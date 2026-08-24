// Package cli implements the cobra command definitions.
package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// HelpToolsCmd returns the `relay help-tools [service]` cobra command.
// Prints all available tools (and their descriptions) for one or all services.
//
// Repointed off the aggregate `gateway.catalog` MCP call (POST /api/mcp),
// which 503s whenever AGGREGATE_MCP_ENABLED != 'true' (default OFF). Now
// uses GET /api/integrations exclusively — a single always-on request
// returns the full service+tool catalog (including per-tool descriptions),
// so no additional per-service tools/list round trip is needed either.
func HelpToolsCmd() *cobra.Command {
	var gatewayURL string

	cmd := &cobra.Command{
		Use:   "help-tools [service]",
		Short: "List tools for a service (or all services)",
		Long: `Fetch and print all available tools from the gateway.

If no service is specified, prints tools for every accessible service.

Examples:
  relay help-tools                     # all services
  relay help-tools clockify            # only clockify tools
  relay help-tools google-workspace    # only Google Workspace tools`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}

			c := resolveClient(gURL)

			info, err := fetchIntegrations(c)
			if err != nil {
				return fmt.Errorf("fetch integrations: %w", err)
			}
			if info == nil {
				// Not authenticated / gateway unreachable — degrade gracefully
				// rather than hard-failing (mirrors fetchIntegrations' 401
				// contract: nil info, nil error).
				fmt.Println("No accessible services found.")
				fmt.Println("You can still use: relay services")
				return nil
			}

			var targets []DiscoveryService
			if len(args) == 1 {
				found := false
				for _, svc := range info.Services {
					if svc.ID == args[0] {
						targets = append(targets, svc)
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("unknown service: %s", args[0])
				}
			} else {
				for _, svc := range info.Services {
					if svc.Accessible {
						targets = append(targets, svc)
					}
				}
				// NOTE: /api/integrations returns the FULL service registry
				// (including services the caller cannot access), so
				// len(info.Services) == 0 practically never happens for a
				// logged-in caller — it is NOT a reliable "nothing available"
				// signal. A caller scoped to zero accessible services
				// (restricted API key / narrow OAuth grant) only shows up
				// here, in the filtered `targets` slice — so THIS is the
				// check that must gate the fallback message.
				if len(targets) == 0 {
					fmt.Println("No accessible services found.")
					fmt.Println("You can still use: relay services")
					return nil
				}
			}

			for _, svc := range targets {
				accessNote := ""
				if !svc.Accessible {
					accessNote = "  (no access)"
				}
				fmt.Printf("\n%s (%d tools)%s\n", svc.ID, len(svc.Tools), accessNote)
				fmt.Println(strings.Repeat("-", 40))
				for _, tool := range svc.Tools {
					if tool.Description != "" {
						fmt.Printf("  %-35s  %s\n", tool.Name, tool.Description)
					} else {
						fmt.Printf("  %s\n", tool.Name)
					}
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")
	return cmd
}
