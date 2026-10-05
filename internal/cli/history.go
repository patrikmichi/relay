package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport/txn"
)

// HistoryCmd returns `relay history` — lists every recorded write
// transaction (committed, restored, or still pending recovery) with its
// journal id, so an operator has a stable, usable id to pass to `relay
// recover <id>` or `relay history prune <id>` (these ids are the same
// journal ids txn.Begin generates and persists, not re-derived or
// recomputed at display time).
func HistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history",
		Short: "List recorded skill/agent write transactions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHistoryList(cmd)
		},
	}
	cmd.AddCommand(historyPruneCmd())
	return cmd
}

func runHistoryList(cmd *cobra.Command) error {
	journals, err := txn.List()
	if err != nil {
		return err
	}
	if len(journals) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no recorded transactions")
		return nil
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tOP\tCREATED")
	for _, j := range journals {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", j.ID, j.State, j.Op, j.CreatedAt.Format("2006-01-02T15:04:05Z"))
	}
	return tw.Flush()
}

// historyPruneCmd returns `relay history prune <id>` — permanently
// deletes a COMMITTED or RESTORED journal's backup/staged bytes. It
// refuses a still-pending (prepared/applying/recovery) journal outright;
// see txn.Prune.
func historyPruneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prune <id>",
		Short: "Delete a finished transaction's backup data",
		Long: `Permanently deletes the backup and staged bytes for one COMMITTED or
RESTORED transaction. A transaction still pending recovery is never
prunable — run 'relay recover <id>' first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := txn.Prune(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pruned %s\n", args[0])
			return nil
		},
	}
}
