package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// SkillRollbackCmd returns the `relay skill rollback` cobra command —
// reverses a manifest-recorded install/migrate.
func SkillRollbackCmd() *cobra.Command {
	var (
		lastFlag bool
		force    bool
	)
	cmd := &cobra.Command{
		Use:   "rollback [manifest-entry-id]",
		Short: "Reverse a manifest-recorded skill install/migrate",
		Long: `Reverse a manifest-recorded install or migrate: delete the files it
wrote (verified against the recorded sha256 hashes — refuses to delete a
file that has changed since, unless --force) and drop the manifest entry.

Use --last to roll back the most recently recorded entry, or pass a
manifest entry id (see 'relay skill list --provenance' or inspect
~/.config/relay/agentport-manifest.json) to roll back a specific one.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var id string
			if len(args) > 0 {
				id = args[0]
			}
			return runSkillRollback(cmd, skillRollbackOpts{id: id, last: lastFlag, force: force})
		},
	}
	cmd.Flags().BoolVar(&lastFlag, "last", false, "Roll back the most recently recorded manifest entry")
	cmd.Flags().BoolVar(&force, "force", false, "Delete files even if their content has changed since they were written")
	return cmd
}

type skillRollbackOpts struct {
	id    string
	last  bool
	force bool
}

func runSkillRollback(cmd *cobra.Command, opts skillRollbackOpts) error {
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
		// Kind-scoped, mirroring AgentRollbackCmd's runAgentRollback: the
		// manifest ledger is shared between skill and agent entries, so a
		// kind-agnostic "most recent entry" would silently roll back an
		// AGENT entry (e.g. after `relay agent migrate` ran more recently
		// than any skill operation) when the caller explicitly asked for
		// `relay skill rollback --last`.
		entry, ok = agentport.LastEntryOfKind(m, agentport.KindSkill)
		if !ok {
			return fmt.Errorf("no skill entries in the manifest — nothing to roll back")
		}
	} else {
		entry, ok = agentport.FindEntry(m, opts.id)
		if !ok {
			return fmt.Errorf("no manifest entry with id %q", opts.id)
		}
		if entry.Kind != agentport.KindSkill {
			return fmt.Errorf("manifest entry %q is a %s entry, not a skill — use 'relay agent rollback' instead", opts.id, entry.Kind)
		}
	}

	if err := agentport.Rollback(entry, opts.force); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "rolled back: %s -> %s (%s scope, id %s)\n", entry.SourceProvider, entry.Provider, entry.Scope, entry.ID)
	return nil
}
