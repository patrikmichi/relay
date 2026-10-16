package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentport"
)

// SkillListCmd returns the `zuano skill list` cobra command — enumerates
// skills present across provider skill directories.
func SkillListCmd() *cobra.Command {
	var (
		providerFlag string
		scopeFlag    string
		provenance   bool
		jsonFlag     bool
		personal     bool
		gatewayURL   string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed skills across agent-skill providers",
		Long: `Enumerate skills present in each provider's skill directories: name,
provider, and scope. With --provenance, joins the manifest ledger to show
where each skill was installed/migrated from (when zuano recorded it).

--personal lists the skills only you can see on the gateway, with where each
stands in your organisation; 'zuano sync' installs them.

Scope 'project' is resolved relative to the current working directory only
(no parent-directory search, unlike 'zuano skill migrate --from').`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if personal {
				return listPersonalSkills(cmd, approvalFlags{gatewayURL: gatewayURL, jsonOut: jsonFlag})
			}
			return runSkillList(cmd, skillListOpts{provider: providerFlag, scope: scopeFlag, provenance: provenance, jsonOut: jsonFlag})
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Print one JSON document on stdout")

	cmd.Flags().StringVar(&providerFlag, "provider", "", fmt.Sprintf("Restrict to one provider: %s (default: all)", providerIDsOxford()))
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to list: user or project")
	cmd.Flags().BoolVar(&provenance, "provenance", false, "Show where each skill was installed/migrated from")
	cmd.Flags().BoolVar(&personal, "personal", false, "List your personal skills on the gateway instead of the installed ones")
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL for --personal (default: $ZUANO_URL, then the config file)")

	return cmd
}

type skillListOpts struct {
	provider   string
	scope      string
	provenance bool
	jsonOut    bool
}

func runSkillList(cmd *cobra.Command, opts skillListOpts) error {
	doc, finish := beginAction(cmd, opts.jsonOut, "skill list")
	return finish(doSkillList(cmd, opts, doc))
}

func doSkillList(cmd *cobra.Command, opts skillListOpts, doc actionDoc) error {
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
	type listed struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
		Scope    string `json:"scope"`
		Path     string `json:"path"`
		From     string `json:"from,omitempty"`
	}
	skills := []listed{}
	doc.put("skills", skills)
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

	defer func() { doc.put("skills", skills) }()
	for _, r := range refs {
		item := listed{Provider: string(r.Provider), Name: r.Name, Scope: string(r.Scope), Path: r.Path}
		if !opts.provenance {
			skills = append(skills, item)
			fmt.Fprintf(out, "%-10s %-24s %-8s %s\n", r.Provider, r.Name, r.Scope, r.Path)
			continue
		}
		entry, ok := agentport.LastEntryFor(m, r.Name, r.Provider, r.Scope, agentport.KindSkill)
		if !ok {
			skills = append(skills, item)
			fmt.Fprintf(out, "%-10s %-24s %-8s %s (no manifest record)\n", r.Provider, r.Name, r.Scope, r.Path)
			continue
		}
		src := string(entry.SourceProvider)
		if src == "" {
			src = "local"
		}
		item.From = src
		skills = append(skills, item)
		fmt.Fprintf(out, "%-10s %-24s %-8s %s (from %s: %s)\n", r.Provider, r.Name, r.Scope, r.Path, src, entry.Provenance.SourcePath)
	}
	return nil
}
