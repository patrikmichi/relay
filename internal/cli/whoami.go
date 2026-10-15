package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/auth"
)

// WhoamiCmd returns the `relay whoami` cobra command.
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

			fmt.Fprintf(cmd.OutOrStdout(), "Email:    %s\n", info.Email)
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
