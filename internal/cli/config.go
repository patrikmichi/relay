// Package cli implements the cobra command definitions.
package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/config"
)

// ConfigCmd returns the `relay config` cobra command group.
// Sub-commands: set-gateway, get-gateway, show.
func ConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage CLI configuration",
		Long:  "Read and write CLI configuration stored at ~/.config/relay/config.json.",
	}
	cmd.AddCommand(
		configSetGatewayCmd(),
		configGetGatewayCmd(),
		configShowCmd(),
	)
	return cmd
}

// configSetGatewayCmd returns `relay config set-gateway <url>`.
func configSetGatewayCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-gateway <url>",
		Short: "Set the gateway URL",
		Long: `Save the gateway URL to ~/.config/relay/config.json.

Example:
  relay config set-gateway https://gateway.example.com`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := args[0]
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			cfg.GatewayURL = url
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Printf("Gateway URL set to: %s\n", url)
			return nil
		},
	}
}

// configGetGatewayCmd returns `relay config get-gateway`.
func configGetGatewayCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get-gateway",
		Short: "Print the configured gateway URL",
		RunE: func(cmd *cobra.Command, _args []string) error {
			url, err := config.GatewayURL()
			if err != nil {
				return fmt.Errorf("resolve gateway URL: %w", err)
			}
			fmt.Println(url)
			return nil
		},
	}
}

// configShowCmd returns `relay config show`.
func configShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print all config values as JSON",
		RunE: func(cmd *cobra.Command, _args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// Resolve the effective gateway URL for display (may come from env var).
			effectiveURL, _ := config.GatewayURL()
			// Resolve the effective identity for display (RELAY_EMAIL, when
			// set, overrides the persisted login email — see
			// config.ResolveEmail). Never errors on "not logged in" here —
			// `relay config show` should always succeed; EffectiveEmail is
			// just empty in that case.
			effectiveEmail, _ := config.ResolveEmail()

			type showOutput struct {
				ConfiguredGatewayURL string `json:"configuredGatewayUrl,omitempty"`
				EffectiveGatewayURL  string `json:"effectiveGatewayUrl"`
				LoggedInEmail        string `json:"loggedInEmail,omitempty"`
				EffectiveEmail       string `json:"effectiveEmail,omitempty"`
			}

			out := showOutput{
				ConfiguredGatewayURL: cfg.GatewayURL,
				EffectiveGatewayURL:  effectiveURL,
				LoggedInEmail:        cfg.Email,
				EffectiveEmail:       effectiveEmail,
			}

			raw, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal output: %w", err)
			}
			fmt.Println(string(raw))
			return nil
		},
	}
}
