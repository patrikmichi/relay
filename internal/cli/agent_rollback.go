package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentport"
)

// AgentRollbackCmd returns the `relay agent rollback` cobra command —
// reverses a Kind: agent manifest-recorded install/migrate. The Agent-IR
// analogue of SkillRollbackCmd.
func AgentRollbackCmd() *cobra.Command {
	var (
		lastFlag bool
		force    bool
		jsonFlag bool
	)
	cmd := &cobra.Command{
		Use:   "rollback [manifest-entry-id]",
		Short: "Reverse a manifest-recorded agent install/migrate",
		Long: `Reverse a manifest-recorded agent install or migrate: delete the file it
wrote (verified against the recorded sha256 hash — refuses to delete a
file that has changed since, unless --force) and drop the manifest entry.

Use --last to roll back the most recently recorded AGENT entry (never a
skill entry, even if one sorts later in the shared manifest ledger), or
pass a manifest entry id (see 'relay agent list --provenance' or inspect
~/.config/relay/agentport-manifest.json) to roll back a specific one.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var id string
			if len(args) > 0 {
				id = args[0]
			}
			return runAgentRollback(cmd, agentRollbackOpts{id: id, last: lastFlag, force: force, jsonOut: jsonFlag})
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Print one JSON document on stdout")
	cmd.Flags().BoolVar(&lastFlag, "last", false, "Roll back the most recently recorded agent manifest entry")
	cmd.Flags().BoolVar(&force, "force", false, "Delete the file even if its content has changed since it was written")
	return cmd
}

type agentRollbackOpts struct {
	id    string
	last  bool
	force bool

	jsonOut bool
}

func runAgentRollback(cmd *cobra.Command, opts agentRollbackOpts) error {
	doc, finish := beginAction(cmd, opts.jsonOut, "agent rollback")
	return finish(doAgentRollback(cmd, opts, doc))
}

func doAgentRollback(cmd *cobra.Command, opts agentRollbackOpts, doc actionDoc) error {
	if opts.last == (opts.id != "") {
		return fmt.Errorf("specify exactly one of --last or <manifest-entry-id>")
	}

	m, err := agentport.LoadManifest()
	if err != nil {
		return err
	}

	var entry agentport.ManifestEntry
	var ok bool
	if opts.last {
		entry, ok = agentport.LastEntryOfKind(m, agentport.KindAgent)
		if !ok {
			return fmt.Errorf("no agent entries in the manifest — nothing to roll back")
		}
	} else {
		entry, ok = agentport.FindEntry(m, opts.id)
		if !ok {
			return fmt.Errorf("no manifest entry with id %q", opts.id)
		}
		if entry.Kind != agentport.KindAgent {
			return fmt.Errorf("manifest entry %q is a %s entry, not an agent — use 'relay skill rollback' instead", opts.id, entry.Kind)
		}
	}

	if err := agentport.Rollback(entry, opts.force); err != nil {
		return err
	}

	doc.put("rolledBack", map[string]string{"id": entry.ID, "from": string(entry.SourceProvider), "to": string(entry.Provider), "scope": string(entry.Scope)})
	fmt.Fprintf(cmd.OutOrStdout(), "rolled back:%s -> %s (%s scope, id %s)\n", entry.SourceProvider, entry.Provider, entry.Scope, entry.ID)
	return nil
}
