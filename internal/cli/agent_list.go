package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// AgentListCmd returns the `relay agent list` cobra command — the
// Agent-IR analogue of SkillListCmd. Enumerates agents present across
// provider agent directories.
func AgentListCmd() *cobra.Command {
	var (
		providerFlag string
		scopeFlag    string
		provenance   bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed agents across agent providers",
		Long: `Enumerate agents present in each provider's agent directory: name,
provider, and scope. With --provenance, joins the manifest ledger to show
where each agent was installed/migrated from (when relay recorded it).

Scope 'project' is resolved relative to the current working directory only
(no parent-directory search, unlike 'relay agent migrate --from').

Supported providers: claude, opencode.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentList(cmd, agentListOpts{provider: providerFlag, scope: scopeFlag, provenance: provenance})
		},
	}

	cmd.Flags().StringVar(&providerFlag, "provider", "", "Restrict to one provider: claude or opencode (default: all)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to list: user or project")
	cmd.Flags().BoolVar(&provenance, "provenance", false, "Show where each agent was installed/migrated from")

	return cmd
}

type agentListOpts struct {
	provider   string
	scope      string
	provenance bool
}

func runAgentList(cmd *cobra.Command, opts agentListOpts) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}

	var adapters []agentport.AgentAdapter
	if opts.provider != "" {
		a, ok := agentport.AgentAdapterByID(agentport.ProviderID(opts.provider))
		if !ok {
			return fmt.Errorf("unknown --provider %q", opts.provider)
		}
		adapters = []agentport.AgentAdapter{a}
	} else {
		adapters = agentport.AllAgentAdapters()
	}

	var refs []agentport.AgentRef
	for _, a := range adapters {
		found, err := agentport.AgentList(a, scope)
		if err != nil {
			return fmt.Errorf("list %s: %w", a.ID(), err)
		}
		refs = append(refs, found...)
	}

	out := cmd.OutOrStdout()
	if len(refs) == 0 {
		fmt.Fprintln(out, "no agents found")
		return nil
	}

	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Provider != refs[j].Provider {
			return refs[i].Provider < refs[j].Provider
		}
		return refs[i].Name < refs[j].Name
	})

	var m agentport.Manifest
	if opts.provenance {
		m, err = agentport.LoadManifest()
		if err != nil {
			return err
		}
	}

	for _, r := range refs {
		if !opts.provenance {
			fmt.Fprintf(out, "%-10s %-24s %-8s %s\n", r.Provider, r.Name, r.Scope, r.Path)
			continue
		}
		entry, ok := agentport.LastEntryFor(m, r.Name, r.Provider, r.Scope, agentport.KindAgent)
		if !ok {
			fmt.Fprintf(out, "%-10s %-24s %-8s %s (no manifest record)\n", r.Provider, r.Name, r.Scope, r.Path)
			continue
		}
		src := string(entry.SourceProvider)
		if src == "" {
			src = "local"
		}
		fmt.Fprintf(out, "%-10s %-24s %-8s %s (from %s: %s)\n", r.Provider, r.Name, r.Scope, r.Path, src, entry.Provenance.SourcePath)
	}
	return nil
}
