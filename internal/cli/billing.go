package cli

import (
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

var (
	billingPeriods = []string{"current", "previous"}
	billingGroups  = []string{"member", "service", "tool"}
)

// billingStatus mirrors GET /api/billing/status.
type billingStatus struct {
	OrgID   string `json:"orgId"`
	OrgSlug string `json:"orgSlug"`
	OrgName string `json:"orgName"`
	View    string `json:"view"`
	Plan    struct {
		Key               string `json:"key"`
		DisplayName       string `json:"displayName"`
		Source            string `json:"source"`
		Status            string `json:"status"`
		TrialEndsAt       string `json:"trialEndsAt"`
		PeriodStart       string `json:"periodStart"`
		PeriodEnd         string `json:"periodEnd"`
		CancelAtPeriodEnd bool   `json:"cancelAtPeriodEnd"`
	} `json:"plan"`
	Verified       bool   `json:"verified"`
	Suspended      bool   `json:"suspended"`
	OverageEnabled bool   `json:"overageEnabled"`
	SpendCapMicros *int64 `json:"spendCapMicros"`
	Currency       string `json:"currency"`
	Seats          struct {
		Used      int  `json:"used"`
		Purchased int  `json:"purchased"`
		Max       *int `json:"max"`
	} `json:"seats"`
	BillingURL string `json:"billingUrl"`
}

// billingUsage mirrors GET /api/billing/usage.
type billingUsage struct {
	Period struct {
		Key   string `json:"key"`
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"period"`
	By       *string `json:"by"`
	Currency string  `json:"currency"`
	Meters   []struct {
		Meter               string   `json:"meter"`
		Unit                string   `json:"unit"`
		Used                int64    `json:"used"`
		Included            *int64   `json:"included"`
		Percent             *float64 `json:"percent"`
		Projected           int64    `json:"projected"`
		ProjectedCostMicros int64    `json:"projectedCostMicros"`
		Overage             string   `json:"overage"`
		Breakdown           []struct {
			Key  string `json:"key"`
			Used int64  `json:"used"`
		} `json:"breakdown"`
	} `json:"meters"`
}

// BillingCmd returns `relay billing`.
func BillingCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "billing",
		Short: "Show the organisation's plan and usage, and open its billing page",
		Long: `Read-only views of the organisation's plan and metered usage, and a shortcut to
the billing page in the browser. Changing the plan, paying and the customer
portal are done there: the CLI holds no billing credential.

A request the plan does not allow ends with an exit status of 30 to 33 and the
link to upgrade, on every relay command:
  30  a feature or limit the plan does not include
  31  a metered allowance or the spend cap is used up
  32  billing could not be read; try again
  33  the organisation or your seat is suspended`,
	}
	cmd.AddCommand(billingStatusCmd(), billingUsageCmd(), billingOpenCmd())
	return cmd
}

func billingStatusCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the plan, subscription state, seats and spend cap",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var s billingStatus
			raw, err := gatewayJSON(cmd.Context(), c, "GET", "/api/billing/status", nil, &s)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			printBillingStatus(cmd.OutOrStdout(), s)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

var planSources = map[string]string{
	"stripe":    "subscription",
	"default":   "default plan",
	"admin":     "assigned by an administrator",
	"simulated": "simulated",
}

func printBillingStatus(out io.Writer, s billingStatus) {
	clean := textsafe.Clean
	name := clean(s.OrgName)
	if s.OrgSlug != "" {
		name += " (" + clean(s.OrgSlug) + ")"
	}
	fmt.Fprintf(out, "Org:       %s\n", name)
	plan := clean(s.Plan.DisplayName)
	if src, ok := planSources[s.Plan.Source]; ok {
		plan += " — " + src
	}
	fmt.Fprintf(out, "Plan:      %s\n", plan)
	if s.Suspended {
		fmt.Fprintln(out, "Status:    suspended")
	} else if s.Plan.Status != "" && s.Plan.Status != "none" {
		status := clean(s.Plan.Status)
		if s.Plan.Status == "trialing" && s.Plan.TrialEndsAt != "" {
			status += ", trial ends " + clean(s.Plan.TrialEndsAt)
		}
		fmt.Fprintf(out, "Status:    %s\n", status)
	}
	if s.Plan.PeriodEnd != "" {
		fmt.Fprintf(out, "Period:    %s to %s\n", clean(s.Plan.PeriodStart), clean(s.Plan.PeriodEnd))
	}
	if s.Plan.CancelAtPeriodEnd {
		fmt.Fprintln(out, "Cancels:   at the end of the period")
	}
	seats := fmt.Sprintf("%d used, %d purchased", s.Seats.Used, s.Seats.Purchased)
	if s.Seats.Max != nil {
		seats += fmt.Sprintf(", %d at most", *s.Seats.Max)
	}
	fmt.Fprintf(out, "Seats:     %s\n", seats)
	fmt.Fprintf(out, "Verified:  %s\n", yesNo(s.Verified))
	overage := "off"
	if s.OverageEnabled {
		overage = "on"
		if s.SpendCapMicros != nil {
			overage += ", spend cap " + formatMicros(*s.SpendCapMicros, s.Currency)
		} else {
			overage += ", no spend cap"
		}
	}
	fmt.Fprintf(out, "Overage:   %s\n", overage)
	if s.View == "reduced" {
		fmt.Fprintln(out, "           (a member's view: ask an org owner or admin for billing details)")
	}
	if s.BillingURL != "" {
		fmt.Fprintf(out, "Billing:   %s\n", clean(s.BillingURL))
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// formatMicros renders millionths of a currency unit, such as 25000000 as
// "25.00 EUR". The currency defaults to EUR, the one the gateway prices in.
func formatMicros(micros int64, currency string) string {
	if currency == "" {
		currency = "EUR"
	}
	sign := ""
	if micros < 0 {
		sign, micros = "-", -micros
	}
	cents := (micros + 5000) / 10000
	return fmt.Sprintf("%s%d.%02d %s", sign, cents/100, cents%100, textsafe.Clean(currency))
}

func billingUsageCmd() *cobra.Command {
	var f approvalFlags
	var meter, period, by string
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Show metered usage against the plan's allowances",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !slices.Contains(billingPeriods, period) {
				return fmt.Errorf("invalid --period %q: use one of %s", period, strings.Join(billingPeriods, ", "))
			}
			if by != "" && !slices.Contains(billingGroups, by) {
				return fmt.Errorf("invalid --by %q: use one of %s", by, strings.Join(billingGroups, ", "))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := url.Values{"period": {period}}
			if meter != "" {
				q.Set("meter", meter)
			}
			if by != "" {
				q.Set("by", by)
			}
			var u billingUsage
			raw, err := gatewayJSON(cmd.Context(), c, "GET", "/api/billing/usage?"+q.Encode(), nil, &u)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			printBillingUsage(cmd.OutOrStdout(), u)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&meter, "meter", "", "Only this meter, such as tool_calls")
	cmd.Flags().StringVar(&period, "period", "current", "Billing period: "+strings.Join(billingPeriods, " or "))
	cmd.Flags().StringVar(&by, "by", "", "Break usage down by "+strings.Join(billingGroups, ", "))
	return cmd
}

func printBillingUsage(out io.Writer, u billingUsage) {
	fmt.Fprintf(out, "Period: %s to %s\n", textsafe.Clean(u.Period.Start), textsafe.Clean(u.Period.End))
	if len(u.Meters) == 0 {
		fmt.Fprintln(out, "No metered usage in this period")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "METER\tUSED\tINCLUDED\tPERCENT\tPROJECTED\tPROJECTED COST")
	for _, m := range u.Meters {
		included, percent := "unlimited", "-"
		if m.Included != nil {
			included = fmt.Sprint(*m.Included)
		}
		if m.Percent != nil {
			percent = fmt.Sprintf("%.0f%%", *m.Percent)
		}
		cost := "-"
		if m.Overage == "bill" {
			cost = formatMicros(m.ProjectedCostMicros, u.Currency)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%d\t%s\n", textsafe.Clean(m.Meter), m.Used, included, percent, m.Projected, cost)
		for _, b := range m.Breakdown {
			fmt.Fprintf(tw, "  %s\t%d\t\t\t\t\n", textsafe.Clean(b.Key), b.Used)
		}
	}
	_ = tw.Flush()
}

func billingOpenCmd() *cobra.Command {
	var gatewayURL string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "open",
		Short: "Open the billing page in the browser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}
			origin, err := config.NormalizeGatewayURL(gURL)
			if err != nil {
				return err
			}
			page := origin + "/settings/billing"
			out := cmd.OutOrStdout()
			if canOpenBrowser(cmd, noBrowser) {
				if err := openBrowserURL(page); err == nil {
					fmt.Fprintf(out, "Opened %s\n", page)
					return nil
				}
			}
			fmt.Fprintln(out, page)
			return nil
		},
	}
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the link instead of opening a browser")
	return cmd
}
