package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// ReviewsCmd returns `zuano reviews`, the client for access review campaigns.
func ReviewsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reviews",
		Short: "See access review campaigns and export their evidence",
		Long: `An access review asks owners to confirm that each agent, key or client still
needs the access it has. These commands list campaigns and export the
evidence. Deciding an item is a person's call with a passkey: 'decide' opens
the review page and sends nothing itself.`,
	}
	cmd.AddCommand(reviewsListCmd(), reviewsShowCmd(), reviewsExportCmd(), reviewsDecideCmd())
	return cmd
}

type reviewRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	DueAt    string `json:"dueAt"`
	Progress struct {
		Decided int `json:"decided"`
		Total   int `json:"total"`
	} `json:"progress"`
}

func reviewsListCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List campaigns",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Reviews []reviewRow `json:"reviews"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/access-reviews", nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tDUE\tDECIDED")
			for _, r := range doc.Reviews {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d of %d\n", textsafe.Clean(r.ID), textsafe.Clean(r.Name), textsafe.Clean(r.Status), orDash(textsafe.Clean(r.DueAt)), r.Progress.Decided, r.Progress.Total)
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func reviewsShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a campaign and its items",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Review reviewRow `json:"review"`
				Items  []struct {
					IdentityID string `json:"identityId"`
					Name       string `json:"name"`
					Reviewer   string `json:"reviewer"`
					Decision   string `json:"decision"`
				} `json:"items"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/access-reviews/"+url.PathEscape(args[0]), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			r := doc.Review
			fmt.Fprintf(out, "%s %s (%s), %d of %d decided, due %s\n", textsafe.Clean(r.ID), textsafe.Clean(r.Name), textsafe.Clean(r.Status), r.Progress.Decided, r.Progress.Total, orDash(textsafe.Clean(r.DueAt)))
			for _, it := range doc.Items {
				fmt.Fprintf(out, "  %-12s %-24s reviewer %-24s %s\n", textsafe.Clean(it.IdentityID), textsafe.Clean(it.Name), orDash(textsafe.Clean(it.Reviewer)), orDash(textsafe.Clean(it.Decision)))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func reviewsExportCmd() *cobra.Command {
	var f approvalFlags
	var format, outFile string
	cmd := &cobra.Command{
		Use:   "export <id> [--format csv|json] [--out file]",
		Short: "Export a campaign's evidence",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "csv" && format != "json" {
				return fmt.Errorf("invalid --format %q: use csv or json", textsafe.Clean(format))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			r, err := gatewayExchange(cmd.Context(), c, http.MethodGet, "/api/access-reviews/"+url.PathEscape(args[0])+"/export?format="+format, nil, nil)
			if err != nil {
				return err
			}
			if !r.ok() {
				return r.failure()
			}
			if outFile != "" {
				if err := os.WriteFile(outFile, r.Body, 0o600); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s.\n", textsafe.Clean(outFile))
				return nil
			}
			_, err = cmd.OutOrStdout().Write(r.Body)
			return err
		},
	}
	f.bind(cmd, "Unused: the export is printed as it is")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().StringVar(&format, "format", "json", "csv or json")
	cmd.Flags().StringVar(&outFile, "out", "", "Write to this file instead of standard output")
	return cmd
}

func reviewsDecideCmd() *cobra.Command {
	var f approvalFlags
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "decide <id>",
		Short: "Open the page where you confirm or reduce access, with your passkey",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAutomationInAgent(brand.Cmd("reviews decide")); err != nil {
				return err
			}
			return openAutomationPage(cmd, f.gatewayURL, "/reviews?campaign="+url.QueryEscape(args[0]), noBrowser, "Decide there")
		},
	}
	f.bind(cmd, "Unused: the command only opens a page")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the link instead of opening a browser")
	return cmd
}
