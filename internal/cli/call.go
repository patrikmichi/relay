// Package cli implements the cobra command definitions.
package cli

import (
	"github.com/spf13/cobra"
)

// CallCmd returns the `relay call <service> <tool> [--arg key=value...]` cobra command.
//
// Delegates to callTool (service_cmd.go) — the same execution path the
// dynamic `relay <service> <tool>` sub-commands use — which POSTs a JSON-RPC
// tools/call request to /api/<service>/mcp. Previously this command built
// its own REST POST /api/<service>/call request inline; that duplicated
// callTool's logic and used the REST surface instead of the canonical MCP
// one. See callTool's doc comment for the full execution-repoint rationale.
func CallCmd() *cobra.Command {
	var gatewayURL string
	var rawArgs []string

	cmd := &cobra.Command{
		Use:   "call <service> <tool>",
		Short: "Call a tool on a gateway service",
		Long: `Call a tool on a gateway service via the MCP JSON-RPC endpoint (POST /api/<service>/mcp, method "tools/call").

Arguments are passed as key=value pairs via --arg flags.

Example:
  relay call clockify list_time_entries
  relay call google-workspace search_emails --arg query="is:unread" --arg maxResults=10`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}

			c := resolveClient(gURL)
			return callTool(c, args[0], args[1], rawArgs)
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")
	cmd.Flags().StringArrayVar(&rawArgs, "arg", nil, "Tool argument as key=value (repeatable)")
	return cmd
}
