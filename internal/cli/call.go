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
	var rawArgs, jsonArgs []string
	var jsonOut bool
	var wait string

	cmd := &cobra.Command{
		Use:   "call <service> <tool>",
		Short: "Call a tool on a gateway service",
		Long: `Call a tool on a gateway service via the MCP JSON-RPC endpoint (POST /api/<service>/mcp, method "tools/call").

Arguments are passed as key=value pairs via --arg flags; the value is always a
string, so an id such as 007 stays "007". Use --arg-json key=<json> for a
number, boolean, object or array.

A call the gateway holds for approval (an agent session's writes) prints the
approval id, risk and expiry and exits 12. With --wait[=SECONDS] the command
waits for the decision and prints the result instead; exit status as for
'relay approvals wait'.

Example:
  relay call clockify list_time_entries
  relay call google-workspace search_emails --arg query="is:unread" --arg-json maxResults=10`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := callOptsFor(cmd, wait)
			if err != nil {
				return err
			}
			opts.jsonArgs = jsonArgs
			gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}

			c, err := resolveClient(gURL)
			if err != nil {
				return err
			}
			return callToolWithContext(cmd.Context(), c, args[0], args[1], rawArgs, jsonOut, cmd.OutOrStdout(), opts)
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, then the config file)")
	cmd.Flags().StringArrayVar(&rawArgs, "arg", nil, "Tool argument as key=value; the value is always a string (repeatable)")
	cmd.Flags().StringArrayVar(&jsonArgs, "arg-json", nil, "Tool argument as key=<json>, for numbers, booleans, objects and arrays (repeatable)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the full raw JSON-RPC response instead of human-rendered output")
	addWaitFlag(cmd, &wait)
	return cmd
}
