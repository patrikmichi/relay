package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport/txn"
)

// RecoverCmd returns `relay recover [id]` — inspects and completes or
// rolls back a pending transaction journal left behind by an interrupted
// skill/agent write. With no id, it recovers
// every pending journal found; with an id, only that one. Recovery always
// either finishes uneventfully (nothing was pending) or restores the
// artifact's prior state — it never attempts to "complete forward" a
// write whose source content might have changed since the crash.
func RecoverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recover [id]",
		Short: "Inspect and resolve an interrupted skill/agent write",
		Long: `An interrupted 'relay skill migrate/install' or 'relay agent migrate' can
leave a pending transaction journal describing exactly what it staged and
what was there before. This command finds and resolves it: a journal that
never started writing is simply marked restored; a journal that was
mid-write is compensated back to the artifact's state before the
interrupted operation began.

With no id, every pending journal is recovered. With an id, only that one.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			if len(args) == 1 {
				j, err := txn.Recover(ctx, args[0])
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "%s: %s (%s)\n", j.ID, j.State, j.Op)
				return nil
			}

			recovered, err := txn.RecoverAll(ctx)
			if err != nil {
				return err
			}
			if len(recovered) == 0 {
				fmt.Fprintln(out, "no pending transactions")
				return nil
			}
			for _, j := range recovered {
				fmt.Fprintf(out, "%s: %s (%s)\n", j.ID, j.State, j.Op)
			}
			return nil
		},
	}
	return cmd
}
