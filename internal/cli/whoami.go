package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/auth"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/keychain"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// profileLabel says which session a command ran as, and why.
func profileLabel(c *client.Client) string {
	// A ZUANO_API_KEY is not a login session: zuano cannot say whether the
	// gateway treats it as a person's or an agent's, so it claims nothing.
	if c.Email() == "" {
		return "API key from ZUANO_API_KEY (zuano does not know whether the gateway holds its writes for approval)"
	}
	if c.Profile() == keychain.ProfileAgent {
		return "agent (ZUANO_AGENT=1; the gateway holds the writes of a session issued as an agent's for approval)"
	}
	return "human (your own session; set ZUANO_AGENT=1 for the agent profile)"
}

// WhoamiCmd returns the `zuano whoami` cobra command.
func WhoamiCmd() *cobra.Command {
	var gatewayURL string
	var full bool

	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show the current authenticated user",
		Long:  "Prints the email and services for the current session. Use --full to include resolved groups.",
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
			info, err := auth.Whoami(ctx, c, full)
			if err != nil {
				return fmt.Errorf("whoami failed: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Profile:  %s\n", profileLabel(c))
			fmt.Fprintf(cmd.OutOrStdout(), "Email:    %s\n", info.Email)
			if info.IdentityID != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Identity: %s (%s)\n", textsafe.Clean(info.IdentityID), orDash(textsafe.Clean(info.IdentityStatus)))
			}
			if info.GoogleSub != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Sub:      %s\n", info.GoogleSub)
			}
			if len(info.Services) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Services: %s\n", strings.Join(info.Services, ", "))
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Services: all")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Issued:   %s\n", info.IssuedAt)
			if full && len(info.Groups) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Groups:   %s\n", strings.Join(info.Groups, ", "))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL")
	cmd.Flags().BoolVar(&full, "full", false, "Include resolved groups")
	return cmd
}
