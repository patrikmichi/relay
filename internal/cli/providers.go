package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// ProvidersCmd returns the `relay providers` cobra command — lists the 4
// built-in agent-skill providers, whether each is detected as installed on
// this machine, and the user/project skill directories relay searches for
// each.
func ProvidersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "List supported agent-skill providers and detection status",
		Long: `List the built-in skill-manager providers (claude, codex, opencode, cursor):
whether each is detected as installed on this machine (any of its user skill
directories exist), and the user/project directories relay searches for
skills of that provider.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, a := range agentport.AllAdapters() {
				status := "not detected"
				if a.Detect() {
					status = "detected"
				}
				fmt.Printf("%-10s %s\n", a.ID(), status)
				fmt.Println("  user dirs:")
				for _, d := range a.UserDirs() {
					fmt.Printf("    %s\n", d)
				}
				fmt.Println("  project dirs:")
				for _, d := range a.ProjectDirs() {
					fmt.Printf("    %s\n", d)
				}
				fmt.Println()
			}
			return nil
		},
	}
	return cmd
}
