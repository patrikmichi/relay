package cli

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

var usageDay = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// orgUsage mirrors GET /api/org/usage.
type orgUsage struct {
	Plan    string `json:"plan"`
	From    string `json:"from"`
	To      string `json:"to"`
	Metrics []struct {
		Metric   string  `json:"metric"`
		Window   string  `json:"window"`
		Used     int64   `json:"used"`
		Soft     int64   `json:"soft"`
		Hard     int64   `json:"hard"`
		ResetsAt *string `json:"resetsAt"`
	} `json:"metrics"`
}

// UsageCmd returns `zuano usage`.
func UsageCmd() *cobra.Command {
	var f approvalFlags
	var from, to string
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Show the organisation's quota usage against its limits",
		Long: `Prints each quota metric with what the organisation used in its window, the
soft limit that raises a warning and the hard limit that blocks. A metric at
its hard limit refuses further use with exit status 31; 'zuano billing usage' shows metered usage and cost.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			for _, day := range []struct{ name, value string }{{"from", from}, {"to", to}} {
				if day.value == "" {
					continue
				}
				if !usageDay.MatchString(day.value) {
					return fmt.Errorf("invalid --%s %q: use a date such as 2026-10-16", day.name, day.value)
				}
				q.Set(day.name, day.value)
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			path := "/api/org/usage"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var u orgUsage
			raw, err := gatewayJSON(cmd.Context(), c, "GET", path, nil, &u)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			printOrgUsage(cmd.OutOrStdout(), u)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&from, "from", "", "First day of the daily usage, as YYYY-MM-DD")
	cmd.Flags().StringVar(&to, "to", "", "Last day of the daily usage, as YYYY-MM-DD")
	return cmd
}

func printOrgUsage(out io.Writer, u orgUsage) {
	if u.Plan != "" {
		fmt.Fprintf(out, "Plan: %s\n", textsafe.Clean(u.Plan))
	}
	if len(u.Metrics) == 0 {
		fmt.Fprintln(out, "No quota metrics")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tWINDOW\tUSED\tSOFT\tHARD\tRESETS")
	for _, m := range u.Metrics {
		resets := "-"
		if m.ResetsAt != nil {
			resets = textsafe.Clean(*m.ResetsAt)
		}
		state := ""
		switch {
		case m.Hard > 0 && m.Used >= m.Hard:
			state = "  at the hard limit"
		case m.Soft > 0 && m.Used >= m.Soft:
			state = "  over the soft limit"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s%s\n", textsafe.Clean(m.Metric), textsafe.Clean(m.Window), m.Used, m.Soft, m.Hard, resets, state)
	}
	_ = tw.Flush()
}
