package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/auth"
	"github.com/patrikmichi/relay/internal/keychain"
)

// LogoutCmd returns the `relay logout` cobra command.
func LogoutCmd() *cobra.Command {
	var gatewayURL string

	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Revoke the current session and remove the stored token",
		RunE: func(cmd *cobra.Command, _args []string) (err error) {
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
				if err := auth.ForgetSession(c.Profile(), c.GatewayOrigin(), c.Email()); err != nil {
					return fmt.Errorf("delete keychain entry: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Offline — skipped server-side session revoke. Removed local session for %s (NOT revoked on gateway)\n", c.Email())
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
			if err := remoteRevokeFailure(cmd.OutOrStdout(), result); err != nil {
				return err
			}

			suffix := ""
			if c.Profile() == keychain.ProfileAgent {
				suffix = " (agent profile)"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged out %s%s\n", result.Email, suffix)
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL")
	return cmd
}

// remoteRevokeFailure turns a failed server-side revoke into an error. The
// local session is already gone, but the token it held still works on the
// gateway, which a script or a person must not mistake for a completed logout.
func remoteRevokeFailure(out io.Writer, r auth.LogoutResult) error {
	if r.RemoteRevoked {
		return nil
	}
	fmt.Fprintf(out, "Removed the local session for %s\n", r.Email)
	return &ExitError{Code: 1, Msg: fmt.Sprintf("session NOT revoked on gateway: %s. The token stays valid there until it expires; revoke it from the gateway's session list", r.RemoteWarning)}
}
