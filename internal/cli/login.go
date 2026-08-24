// Package cli implements the cobra command definitions.
package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/auth"
)

// LoginCmd returns the `relay login` cobra command.
func LoginCmd() *cobra.Command {
	var gatewayURL string
	var deviceFlow bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with the gateway (opens browser)",
		Long: `Authenticate your CLI session with the gateway using Google OAuth.

By default, opens a browser window and starts a local callback server.
Use --device for headless environments (SSH, containers) where a browser
is not available.`,
		RunE: func(cmd *cobra.Command, _args []string) error {
			// login is a gateway command like any other catalog verb — it
			// cannot succeed offline (there is nothing to authenticate
			// against), so it fails closed the same way services/call/etc.
			// do rather than dialing an unresolved or --offline-forbidden
			// URL. See gateway.go's resolveGatewayURLOrFailClosed doc
			// comment.
			resolvedURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}

			ctx := context.Background()

			if deviceFlow {
				result, err := auth.DeviceLogin(ctx, resolvedURL)
				if err != nil {
					fmt.Fprintln(os.Stderr, "Login failed:", err)
					os.Exit(1)
				}
				fmt.Printf("Logged in as %s\n", result.Email)
				return nil
			}

			result, err := auth.Login(ctx, resolvedURL)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Login failed:", err)
				os.Exit(1)
			}

			fmt.Printf("Logged in as %s\n", result.Email)
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")
	cmd.Flags().BoolVar(&deviceFlow, "device", false, "Use device-code flow for headless environments")

	return cmd
}
