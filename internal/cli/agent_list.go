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
		jsonFlag     bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed agents across agent providers",
		Long: fmt.Sprintf(`Enumerate agents present in each provider's agent directory: name,
provider, and scope. With --provenance, joins the manifest ledger to show
where each agent was installed/migrated from (when relay recorded it).

Scope 'project' is resolved relative to the current working directory only
(no parent-directory search, unlike 'relay agent migrate --from').

Supported providers: %s.`, agentProviderIDsCSV()),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentList(cmd, agentListOpts{provider: providerFlag, scope: scopeFlag, provenance: provenance, jsonOut: jsonFlag})
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Print one JSON document on stdout")

	cmd.Flags().StringVar(&providerFlag, "provider", "", fmt.Sprintf("Restrict to one provider: %s (default: all)", agentProviderIDsOxford()))
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to list: user or project")
	cmd.Flags().BoolVar(&provenance, "provenance", false, "Show where each agent was installed/migrated from")

	return cmd
}

type agentListOpts struct {
	provider   string
	scope      string
	provenance bool
	jsonOut    bool
}

func runAgentList(cmd *cobra.Command, opts agentListOpts) error {
	doc, finish := beginAction(cmd, opts.jsonOut, "agent list")
	return finish(doAgentList(cmd, opts, doc))
}

func doAgentList(cmd *cobra.Command, opts agentListOpts, doc actionDoc) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}

	var adapters []agentport.AgentAdapter
	if opts.provider != "" {
		a, ok := agentport.AgentAdapterByID(agentport.ProviderID(opts.provider))
		if !ok {
			return unsupportedAgentProviderError(opts.provider)
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
	type listed struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
		Scope    string `json:"scope"`
		Path     string `json:"path"`
		From     string `json:"from,omitempty"`
	}
	agents := []listed{}
	doc.put("agents", agents)
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

	defer func() { doc.put("agents", agents) }()
	for _, r := range refs {
		item := listed{Provider: string(r.Provider), Name: r.Name, Scope: string(r.Scope), Path: r.Path}
		if !opts.provenance {
			agents = append(agents, item)
			fmt.Fprintf(out, "%-10s %-24s %-8s %s\n", r.Provider, r.Name, r.Scope, r.Path)
			continue
		}
		entry, ok := agentport.LastEntryFor(m, r.Name, r.Provider, r.Scope, agentport.KindAgent)
		if !ok {
			agents = append(agents, item)
			fmt.Fprintf(out, "%-10s %-24s %-8s %s (no manifest record)\n", r.Provider, r.Name, r.Scope, r.Path)
			continue
		}
		src := string(entry.SourceProvider)
		if src == "" {
			src = "local"
		}
		item.From = src
		agents = append(agents, item)
		fmt.Fprintf(out, "%-10s %-24s %-8s %s (from %s: %s)\n", r.Provider, r.Name, r.Scope, r.Path, src, entry.Provenance.SourcePath)
	}
	return nil
}
