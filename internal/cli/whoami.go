package cli

import (
	"fmt"
	"os"
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
		RunE: func(cmd *cobra.Command, _args []string) error {
			gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}

			c := resolveClient(gURL)

			info, err := auth.Whoami(c, full)
			if err != nil {
				fmt.Fprintln(os.Stderr, "whoami failed:", err)
				os.Exit(1)
			}

			fmt.Printf("Email:    %s\n", info.Email)
			if info.GoogleSub != "" {
				fmt.Printf("Sub:      %s\n", info.GoogleSub)
			}
			if len(info.Services) > 0 {
				fmt.Printf("Services: %s\n", strings.Join(info.Services, ", "))
			} else {
				fmt.Println("Services: all")
			}
			fmt.Printf("Issued:   %s\n", info.IssuedAt)
			if full && len(info.Groups) > 0 {
				fmt.Printf("Groups:   %s\n", strings.Join(info.Groups, ", "))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL")
	cmd.Flags().BoolVar(&full, "full", false, "Include resolved groups")
	return cmd
}
