package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/auth"
	"github.com/patrikmichi/relay/internal/keychain"
)

// TokensCmd returns the `relay tokens` cobra command with list and revoke subcommands.
func TokensCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tokens",
		Short: "Manage gateway session tokens",
	}
	cmd.AddCommand(tokensListCmd(), tokensRevokeCmd())
	return cmd
}

// tokensListCmd returns `relay tokens list`.
// Calls GET /api/cli/whoami and prints active session info.
func tokensListCmd() *cobra.Command {
	var gatewayURL string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show active session info (email, services, issued at, expires at)",
		RunE: func(cmd *cobra.Command, _args []string) (err error) {
			gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}

			c, err := resolveClient(gURL)
			if err != nil {
				return err
			}

			ctx, cancel, err := requestContext(cmd.Context(), controlRequest)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { err = timeoutCause(ctx, err) }()
			resp, err := c.GetContext(ctx, "/api/cli/whoami")
			if err != nil {
				return fmt.Errorf("GET /api/cli/whoami: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusUnauthorized {
				return errors.New("token expired or revoked — run `relay login` to re-authenticate")
			}

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("tokens list failed (HTTP %d)", resp.StatusCode)
			}

			// whoamiResponse mirrors auth.WhoamiResponse but is local to avoid import cycle.
			var info struct {
				Email     string   `json:"email"`
				GoogleSub string   `json:"googleSub"`
				Services  []string `json:"services"`
				IssuedAt  string   `json:"issuedAt"`
				ExpiresAt string   `json:"expiresAt,omitempty"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Email:     %s\n", info.Email)
			if len(info.Services) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Services:  %s\n", strings.Join(info.Services, ", "))
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Services:  all")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Issued at: %s\n", info.IssuedAt)
			if info.ExpiresAt != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Expires:   %s\n", info.ExpiresAt)
			}
			// Indicate token is stored in keychain (not printed for security)
			fmt.Fprintf(cmd.OutOrStdout(), "Token:     stored in system keychain (%d bytes)\n", len(c.AccessToken))
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, then the config file)")
	return cmd
}

// tokensRevokeCmd returns `relay tokens revoke`.
func tokensRevokeCmd() *cobra.Command {
	var gatewayURL string
	var legacy bool

	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke the current session token",
		RunE: func(cmd *cobra.Command, _args []string) (err error) {
			if legacy {
				return logoutLegacySession()
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

			// Building the client never dials the network — it only reads
			// GATEWAY_API_KEY / the OS keychain.
			c, err := resolveClient(gURL)
			if err != nil {
				return err
			}
			if c.Email() == "" {
				return errors.New("authenticated via GATEWAY_API_KEY — there is no local CLI session to revoke")
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
				return fmt.Errorf("revoke failed: %w", err)
			}
			if !result.RemoteRevoked {
				fmt.Fprintf(os.Stderr, "Warning: %s\n", result.RemoteWarning)
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Session revoked")
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, then the config file)")
	cmd.Flags().BoolVar(&legacy, "legacy", false, "Remove a pre-gateway-identity session left over from an older relay version")
	return cmd
}
