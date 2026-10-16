package cli

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

// auditFilter is the set of filters the audit routes share.
type auditFilter struct {
	from, to                     string
	user, service, tool, outcome string
}

func (f *auditFilter) bind(cmd *cobra.Command, withSearch bool) {
	cmd.Flags().StringVar(&f.from, "from", "", "First day, as YYYY-MM-DD")
	cmd.Flags().StringVar(&f.to, "to", "", "Last day, as YYYY-MM-DD")
	if withSearch {
		cmd.Flags().StringVar(&f.user, "user", "", "Only this user (email)")
		cmd.Flags().StringVar(&f.service, "service", "", "Only this service")
		cmd.Flags().StringVar(&f.tool, "tool", "", "Only this tool")
		cmd.Flags().StringVar(&f.outcome, "outcome", "", "Only this outcome")
	}
}

func (f auditFilter) query(extra url.Values) (url.Values, error) {
	q := url.Values{}
	for _, day := range []struct{ name, value string }{{"from", f.from}, {"to", f.to}} {
		if day.value == "" {
			continue
		}
		if !usageDay.MatchString(day.value) {
			return nil, fmt.Errorf("invalid --%s %q: use a date such as 2026-10-16", day.name, day.value)
		}
		q.Set(day.name, day.value)
	}
	for k, v := range map[string]string{"user": f.user, "service": f.service, "tool": f.tool, "outcome": f.outcome} {
		if v != "" {
			q.Set(k, v)
		}
	}
	for k, vs := range extra {
		q[k] = vs
	}
	return q, nil
}

// AuditCmd returns `zuano audit`, the client for the gateway's tamper-evident
// audit log. Both commands need an organisation admin.
func AuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Verify and export the organisation's audit log (admins)",
		Long: `The gateway chains every audit event to the one before it and signs a daily
checkpoint. 'verify' asks the gateway to recompute the chain and check the
checkpoints; it exits 1 when the log does not verify, naming the first broken
event. 'export' downloads the events, filtered, as JSON lines or CSV, and an
export to a file that stopped part way continues with --resume.`,
	}
	cmd.AddCommand(auditVerifyCmd(), auditExportCmd())
	return cmd
}

// auditVerifyResult mirrors GET /api/admin/audit/verify.
type auditVerifyResult struct {
	OK          bool    `json:"ok"`
	From        string  `json:"from"`
	To          string  `json:"to"`
	Events      int64   `json:"events"`
	Checkpoints int64   `json:"checkpoints"`
	FirstBadSeq *int64  `json:"firstBadSeq"`
	Reason      *string `json:"reason"`
}

func auditVerifyCmd() *cobra.Command {
	var f approvalFlags
	var filter auditFilter
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check the audit log's hash chain and signed checkpoints",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q, err := filter.query(nil)
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			path := "/api/admin/audit/verify"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var res auditVerifyResult
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &res)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
			} else if res.OK {
				fmt.Fprintf(out, "Audit log verified: %d events, %d checkpoints, %s to %s\n", res.Events, res.Checkpoints, textsafe.Clean(res.From), textsafe.Clean(res.To))
			} else {
				fmt.Fprintf(out, "Audit log did NOT verify (%s to %s)\n", textsafe.Clean(res.From), textsafe.Clean(res.To))
			}
			if res.OK {
				return nil
			}
			msg := "the audit log does not verify"
			if res.FirstBadSeq != nil {
				msg += fmt.Sprintf(": first broken event is seq %d", *res.FirstBadSeq)
			}
			if res.Reason != nil && *res.Reason != "" {
				msg += " (" + textsafe.Clean(*res.Reason) + ")"
			}
			if f.jsonOut {
				return &ExitError{Code: 1}
			}
			return &ExitError{Code: 1, Msg: msg}
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	filter.bind(cmd, false)
	return cmd
}
