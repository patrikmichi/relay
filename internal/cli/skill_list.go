package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// SkillListCmd returns the `relay skill list` cobra command — enumerates
// skills present across provider skill directories.
func SkillListCmd() *cobra.Command {
	var (
		providerFlag string
		scopeFlag    string
		provenance   bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed skills across agent-skill providers",
		Long: `Enumerate skills present in each provider's skill directories: name,
provider, and scope. With --provenance, joins the manifest ledger to show
where each skill was installed/migrated from (when relay recorded it).

Scope 'project' is resolved relative to the current working directory only
(no parent-directory search, unlike 'relay skill migrate --from').`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSkillList(cmd, skillListOpts{provider: providerFlag, scope: scopeFlag, provenance: provenance})
		},
	}

	cmd.Flags().StringVar(&providerFlag, "provider", "", "Restrict to one provider: claude, codex, opencode, or cursor (default: all)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to list: user or project")
	cmd.Flags().BoolVar(&provenance, "provenance", false, "Show where each skill was installed/migrated from")

	return cmd
}

type skillListOpts struct {
	provider   string
	scope      string
	provenance bool
}

func runSkillList(cmd *cobra.Command, opts skillListOpts) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}

	var adapters []agentport.Adapter
	if opts.provider != "" {
		a, ok := agentport.AdapterByID(agentport.ProviderID(opts.provider))
		if !ok {
			return fmt.Errorf("unknown --provider %q", opts.provider)
		}
		adapters = []agentport.Adapter{a}
	} else {
		adapters = agentport.AllAdapters()
	}

	var refs []agentport.SkillRef
	for _, a := range adapters {
		found, err := agentport.List(a, scope)
		if err != nil {
			return fmt.Errorf("list %s: %w", a.ID(), err)
		}
		refs = append(refs, found...)
	}

	out := cmd.OutOrStdout()
	if len(refs) == 0 {
		fmt.Fprintln(out, "no skills found")
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
		entry, ok := agentport.LastEntryFor(m, r.Name, r.Provider, r.Scope, agentport.KindSkill)
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
