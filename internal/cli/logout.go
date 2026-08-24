package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/auth"
	"github.com/patrikmichi/relay/internal/keychain"
)

// LogoutCmd returns the `relay logout` cobra command.
//
// logout has two halves: deleting the local OS keychain entry (no network,
// always safe) and revoking the token family server-side (auth.Logout's
// POST /api/cli/logout, gateway-touching). Unlike every OTHER
// gateway-touching command in this package (which fail closed entirely
// under --offline via resolveGatewayURLOrFailClosed), only logout's
// server-side half is gated on offline/fail-closed resolution here — a
// user should always be able to clear a stale local session (e.g. after a
// laptop loses network, or the gateway is decommissioned) even with no
// gateway reachable, so the local half runs unconditionally once we know
// which email's keychain entry to delete. `relay tokens revoke`
// (tokens.go) is the other command surface for this exact operation and
// intentionally mirrors this same two-halves design.
func LogoutCmd() *cobra.Command {
	var gatewayURL string

	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Revoke the current session and remove the stored token",
		RunE: func(cmd *cobra.Command, _args []string) error {
			// Building the client never dials the network — it only reads
			// GATEWAY_API_KEY / the OS keychain — so this is safe to do
			// before deciding whether the network half may run.
			c := resolveClient(gatewayURL)
			if c.Email() == "" {
				// GATEWAY_API_KEY bearer session — no local CLI session to revoke.
				fmt.Fprintln(os.Stderr, "Authenticated via GATEWAY_API_KEY — there is no local CLI session to log out of.")
				os.Exit(1)
			}

			if Offline() {
				if err := keychain.DeleteToken(c.Email()); err != nil {
					return fmt.Errorf("delete keychain entry: %w", err)
				}
				fmt.Printf("Offline — skipped server-side session revoke. Removed local session for %s\n", c.Email())
				return nil
			}

			resolvedURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}
			c.GatewayURL = strings.TrimRight(resolvedURL, "/")

			if err := auth.Logout(c); err != nil {
				fmt.Fprintln(os.Stderr, "Logout failed:", err)
				os.Exit(1)
			}

			fmt.Printf("Logged out %s\n", c.Email())
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL")
	return cmd
}
