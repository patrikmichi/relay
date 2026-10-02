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
		Long:  `List registered skill providers, detection status, and user/project directories.`,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, a := range agentport.AllAdapters() {
				status := "not detected"
				if a.Detect() {
					status = "detected"
				}
				_, supportsAgents := agentport.AgentAdapterByID(a.ID())
				fmt.Fprintf(cmd.OutOrStdout(), "%-10s %s; agents: %t\n", a.ID(), status, supportsAgents)
				fmt.Fprintln(cmd.OutOrStdout(), "  user dirs:")
				for _, d := range a.UserDirs() {
					fmt.Fprintf(cmd.OutOrStdout(), "    %s\n", d)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "  project dirs:")
				for _, d := range a.ProjectDirs() {
					fmt.Fprintf(cmd.OutOrStdout(), "    %s\n", d)
				}
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}
	return cmd
}
