// Package cli implements the cobra command definitions.
package cli

import (
	"fmt"

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

			// cmd.Context() carries the root command's signal-aware
			// cancellation (see cmd/relay/main.go) — Login/DeviceLogin each
			// narrow it further with their own operation-specific deadline.
			ctx := cmd.Context()

			if deviceFlow {
				result, err := auth.DeviceLogin(ctx, resolvedURL, cmd.OutOrStdout())
				if err != nil {
					return fmt.Errorf("login failed: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Logged in as %s\n", result.Email)
				return nil
			}

			result, err := auth.Login(ctx, resolvedURL, cmd.ErrOrStderr())
			if err != nil {
				return fmt.Errorf("login failed: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Logged in as %s\n", result.Email)
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, then the config file)")
	cmd.Flags().BoolVar(&deviceFlow, "device", false, "Use device-code flow for headless environments")

	return cmd
}
