package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentport"
)

// SkillDiffCmd returns the `relay skill diff <name>` cobra command — a
// preview of `relay skill migrate` that writes nothing.
func SkillDiffCmd() *cobra.Command {
	var (
		fromFlag  string
		toFlag    string
		scopeFlag string
		jsonFlag  bool
	)

	cmd := &cobra.Command{
		Use:   "diff <name>",
		Short: "Preview a migration: field-level differences + fidelity-loss report, without writing",
		Long: `Load <name> from --from's directory (in the given --scope), project it
to --to, and print the field-level differences plus the fidelity-loss
report — without writing anything to --to's real skill directory or
recording a manifest entry. This is a preview of 'relay skill migrate'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillDiff(cmd, args[0], skillDiffOpts{from: fromFlag, to: toFlag, scope: scopeFlag, jsonOut: jsonFlag})
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Print one JSON document on stdout")

	cmd.Flags().StringVar(&fromFlag, "from", "", "Source provider (required)")
	cmd.Flags().StringVar(&toFlag, "to", "", "Target provider (required)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to search: user or project")

	return cmd
}

type skillDiffOpts struct {
	from  string
	to    string
	scope string

	jsonOut bool
}

func runSkillDiff(cmd *cobra.Command, name string, opts skillDiffOpts) error {
	doc, finish := beginAction(cmd, opts.jsonOut, "skill diff")
	return finish(doSkillDiff(cmd, name, opts, doc))
}

func doSkillDiff(cmd *cobra.Command, name string, opts skillDiffOpts, doc actionDoc) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}
	if opts.from == "" || opts.to == "" {
		return fmt.Errorf("both --from and --to are required")
	}

	fromAdapter, ok := agentport.AdapterByID(agentport.ProviderID(opts.from))
	if !ok {
		return fmt.Errorf("unknown --from provider %q", opts.from)
	}
	toAdapter, ok := agentport.AdapterByID(agentport.ProviderID(opts.to))
	if !ok {
		return fmt.Errorf("unknown --to provider %q", opts.to)
	}

	skillPath, err := agentport.ResolveSkillPath(fromAdapter, scope, name)
	if err != nil {
		return err
	}
	src, err := fromAdapter.Load(skillPath)
	if err != nil {
		return fmt.Errorf("load %s from %s: %w", name, opts.from, err)
	}

	plan, err := agentport.Migrate(src, toAdapter, scope)
	if err != nil {
		return fmt.Errorf("migrate to %s: %w", opts.to, err)
	}

	doc.put("from", string(fromAdapter.ID()))
	doc.put("to", string(toAdapter.ID()))
	doc.put("scope", string(scope))
	doc.put("wouldWrite", plan.TargetPaths)
	doc.put("fidelity", lossReports(plan.Loss))
	doc.put("fieldDiff", []string{})
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "== %s -> %s (%s scope, preview only) ==\n", fromAdapter.ID(), toAdapter.ID(), scope)
	fmt.Fprintf(out, "would write to: %s\n", plan.TargetPaths)
	printLossReport(out, plan.Loss)

	// Project into a scratch temp dir (never plan.TargetPaths) so we can
	// re-Load it and report field-level differences without writing
	// anything under the real target directory or touching the manifest.
	tmp, err := os.MkdirTemp("", "relay-skill-diff-*")
	if err != nil {
		return fmt.Errorf("create scratch dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	scratchDir := filepath.Join(tmp, src.Name)
	if err := writeScratchFiles(scratchDir, plan.Files); err != nil {
		return fmt.Errorf("write scratch files: %w", err)
	}

	projected, err := toAdapter.Load(scratchDir)
	if err != nil {
		return fmt.Errorf("re-load projected skill: %w", err)
	}

	diffs := agentport.FieldDiff(src, projected)
	if doc != nil && len(diffs) > 0 {
		lines := make([]string, len(diffs))
		for i, d := range diffs {
			lines[i] = fmt.Sprint(d)
		}
		doc.put("fieldDiff", lines)
	}
	if len(diffs) == 0 {
		fmt.Fprintln(out, "field diff: no differences")
		return nil
	}
	fmt.Fprintln(out, "field diff:")
	for _, d := range diffs {
		fmt.Fprintf(out, "  %s\n", d)
	}
	return nil
}

// writeScratchFiles materializes a Plan's Files map under dir — mirrors
// agentport.Write's file-writing half without the manifest-recording half,
// used only for skill diff's throwaway preview directory (which must never
// touch the real target dir or record a manifest entry).
func writeScratchFiles(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for rel, data := range files {
		dest := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
