package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/auth"
	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

// LogoutCmd returns the `relay logout` cobra command.
func LogoutCmd() *cobra.Command {
	var gatewayURL string
	var legacy bool

	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Revoke the current session and remove the stored token",
		RunE: func(cmd *cobra.Command, _args []string) (err error) {
			if legacy {
				return logoutLegacySession(cmd.OutOrStdout())
			}

			// The keychain lookup below is scoped to (gateway, email), so
			// the gateway must be identified before building a client —
			// resolved locally (no network) so this still works offline.
			gURL, err := resolveGatewayIdentityLocal(gatewayURL)
			if err != nil {
				return err
			}
			if gURL == "" {
				if Offline() {
					return errors.New("no gateway configured — cannot determine which session to remove; pass --gateway-url")
				}
				return errors.New(offlineGuidance)
			}

			c, err := resolveClient(gURL)
			if err != nil {
				return err
			}
			if c.Email() == "" {
				return errors.New("authenticated via GATEWAY_API_KEY — there is no local CLI session to log out of")
			}

			if Offline() {
				if err := keychain.DeleteToken(c.GatewayOrigin(), c.Email()); err != nil {
					return fmt.Errorf("delete keychain entry: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Offline — skipped server-side session revoke. Removed local session for %s\n", c.Email())
				return nil
			}

			ctx, cancel, err := requestContext(cmd.Context(), controlRequest)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { err = timeoutCause(ctx, err) }()
			result, err := auth.Logout(ctx, c)
			if err != nil {
				return fmt.Errorf("logout failed: %w", err)
			}
			if !result.RemoteRevoked {
				fmt.Fprintf(os.Stderr, "Warning: %s\n", result.RemoteWarning)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Logged out %s\n", result.Email)
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL")
	cmd.Flags().BoolVar(&legacy, "legacy", false, "Remove a pre-gateway-identity session left over from an older relay version")
	return cmd
}

// logoutLegacySession removes a pre-gateway-identity ("legacy") keychain
// entry for the current identity, without needing a resolved gateway. It
// never prints the stored token value.
func logoutLegacySession(writers ...io.Writer) error {
	out := outputWriter(writers)
	email, err := config.ResolveEmail()
	if err != nil {
		return err
	}
	if !keychain.HasLegacyToken(email) {
		fmt.Fprintf(out, "No legacy session found for %s\n", email)
		return nil
	}
	if err := keychain.DeleteLegacyToken(email); err != nil {
		return fmt.Errorf("delete legacy keychain entry: %w", err)
	}
	fmt.Fprintf(out, "Removed legacy session for %s\n", email)
	return nil
}
