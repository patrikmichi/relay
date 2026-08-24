package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

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
		RunE: func(cmd *cobra.Command, _args []string) error {
			gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}

			c := resolveClient(gURL)

			resp, err := c.Get("/api/cli/whoami")
			if err != nil {
				return fmt.Errorf("GET /api/cli/whoami: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusUnauthorized {
				fmt.Fprintln(os.Stderr, "Token expired or revoked — run `relay login` to re-authenticate.")
				os.Exit(1)
			}

			if resp.StatusCode != http.StatusOK {
				var errBody map[string]string
				_ = json.NewDecoder(resp.Body).Decode(&errBody)
				return fmt.Errorf("tokens list failed (%d): %s", resp.StatusCode, errBody["error"])
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

			fmt.Printf("Email:     %s\n", info.Email)
			if len(info.Services) > 0 {
				fmt.Printf("Services:  %s\n", strings.Join(info.Services, ", "))
			} else {
				fmt.Println("Services:  all")
			}
			fmt.Printf("Issued at: %s\n", info.IssuedAt)
			if info.ExpiresAt != "" {
				fmt.Printf("Expires:   %s\n", info.ExpiresAt)
			}
			// Indicate token is stored in keychain (not printed for security)
			fmt.Printf("Token:     stored in system keychain (%d bytes)\n", len(c.AccessToken))
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")
	return cmd
}

// tokensRevokeCmd returns `relay tokens revoke`.
// Calls POST /api/cli/logout and removes the local keychain entry.
//
// Same two-halves shape as LogoutCmd (logout.go) — deleting the local OS
// keychain entry never needs the network and must work under --offline;
// only the server-side revoke (POST /api/cli/logout) is gateway-touching
// and subject to fail-closed resolution. See logout.go's doc comment for
// the full rationale; kept identical here since `tokens revoke` and
// `logout` perform the exact same two operations through two different
// command surfaces.
func tokensRevokeCmd() *cobra.Command {
	var gatewayURL string

	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke the current session token",
		RunE: func(cmd *cobra.Command, _args []string) error {
			// Building the client never dials the network — it only reads
			// GATEWAY_API_KEY / the OS keychain.
			c := resolveClient(gatewayURL)
			if c.Email() == "" {
				fmt.Fprintln(os.Stderr, "Authenticated via GATEWAY_API_KEY — there is no local CLI session to revoke.")
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

			resp, err := c.Post("/api/cli/logout", "application/json", nil)
			if err != nil {
				// Server unavailable — still delete local keychain entry
				fmt.Fprintf(os.Stderr, "Warning: could not reach gateway to revoke session: %v\n", err)
			} else {
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					body, _ := io.ReadAll(resp.Body)
					fmt.Fprintf(os.Stderr, "Warning: server returned %d during revoke: %s\n", resp.StatusCode, body)
				}
			}

			// Always delete the keychain entry
			if err := keychain.DeleteToken(c.Email()); err != nil {
				return fmt.Errorf("delete keychain entry: %w", err)
			}

			fmt.Println("Session revoked")
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")
	return cmd
}
