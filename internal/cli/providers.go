package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// providerIDsCSV returns every loaded skill-provider id, comma-separated,
// in agentport.AllAdapters()'s stable order (e.g. "claude, codex,
// opencode, cursor, cline, gemini-cli, windsurf"). Used everywhere a
// cobra Long/flag-usage string used to hardcode the provider list, so the
// text always matches the live adapter registry.
func providerIDsCSV() string {
	adapters := agentport.AllAdapters()
	ids := make([]string, len(adapters))
	for i, a := range adapters {
		ids[i] = string(a.ID())
	}
	return strings.Join(ids, ", ")
}

// agentProviderIDsCSV is providerIDsCSV's Agent-IR analogue: every loaded
// agent-provider id, comma-separated, in agentport.AllAgentAdapters()'s
// stable order (e.g. "claude, opencode").
func agentProviderIDsCSV() string {
	adapters := agentport.AllAgentAdapters()
	ids := make([]string, len(adapters))
	for i, a := range adapters {
		ids[i] = string(a.ID())
	}
	return strings.Join(ids, ", ")
}

// providerIDsOxfordOr joins ids as "a, b, or c" (Oxford comma before the
// final "or"), matching the CLI's established flag-usage/error-message
// phrasing for a multi-provider list. Falls back gracefully for 0/1/2-id
// lists.
func providerIDsOxfordOr(ids []string) string {
	switch len(ids) {
	case 0:
		return ""
	case 1:
		return ids[0]
	case 2:
		return ids[0] + " or " + ids[1]
	default:
		return strings.Join(ids[:len(ids)-1], ", ") + ", or " + ids[len(ids)-1]
	}
}

// providerIDsOxford / agentProviderIDsOxford return the live skill/agent
// provider id lists in the Oxford-or flag-usage/error-message phrasing used
// across skill_*.go / agent_*.go, derived from the same live registries as
// providerIDsCSV/agentProviderIDsCSV instead of a literal.
func providerIDsOxford() string {
	adapters := agentport.AllAdapters()
	ids := make([]string, len(adapters))
	for i, a := range adapters {
		ids[i] = string(a.ID())
	}
	return providerIDsOxfordOr(ids)
}

func agentProviderIDsOxford() string {
	adapters := agentport.AllAgentAdapters()
	ids := make([]string, len(adapters))
	for i, a := range adapters {
		ids[i] = string(a.ID())
	}
	return providerIDsOxfordOr(ids)
}

// ProvidersCmd returns the `relay providers` cobra command — lists every
// built-in agent-skill provider, whether each is detected as installed on
// this machine, the user/project skill directories relay searches for
// each, and whether the same provider id also has an agent-provider
// adapter loaded (skills: yes always; agents: yes/no).
func ProvidersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "List supported agent-skill/agent providers and detection status",
		Long: fmt.Sprintf(`List the built-in skill-manager providers (%s):
whether each is detected as installed on this machine (any of its user skill
directories exist), the user/project directories relay searches for skills
of that provider, and whether relay also supports agent management for
that provider id (agents: %s).`, providerIDsCSV(), agentProviderIDsCSV()),
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			agentIDs := make(map[string]bool)
			for _, a := range agentport.AllAgentAdapters() {
				agentIDs[string(a.ID())] = true
			}
			for _, a := range agentport.AllAdapters() {
				status := "not detected"
				if a.Detect() {
					status = "detected"
				}
				agents := "no"
				if agentIDs[string(a.ID())] {
					agents = "yes"
				}
				fmt.Fprintf(out, "%-10s %s  skills: yes  agents: %s\n", a.ID(), status, agents)
				fmt.Fprintln(out, "  user dirs:")
				for _, d := range a.UserDirs() {
					fmt.Fprintf(out, "    %s\n", d)
				}
				fmt.Fprintln(out, "  project dirs:")
				for _, d := range a.ProjectDirs() {
					fmt.Fprintf(out, "    %s\n", d)
				}
				fmt.Fprintln(out)
			}
			return nil
		},
	}
	return cmd
}
