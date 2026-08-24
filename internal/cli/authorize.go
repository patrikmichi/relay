package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/auth"
)

// AuthorizeCmd returns the `relay authorize <service>` cobra command.
// Used for per-service Google Workspace authorization (device-code fallback).
func AuthorizeCmd() *cobra.Command {
	var gatewayURL string

	cmd := &cobra.Command{
		Use:   "authorize <service>",
		Short: "Authorize a specific service (e.g. google-workspace)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}

			service := args[0]
			ctx := context.Background()

			// relay authorize uses the same device-code flow as relay login --device.
			// Per-service Workspace scope authorization is a follow-up task.
			fmt.Printf("Authorizing %s via device-code flow...\n", service)
			result, err := auth.DeviceLogin(ctx, resolvedURL)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Authorization failed:", err)
				os.Exit(1)
			}

			fmt.Printf("Authorized %s for %s\n", result.Email, service)
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL")
	return cmd
}
