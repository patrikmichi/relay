package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

var auditFormats = []string{"ndjson", "csv"}

// auditFilter is the set of filters the audit routes share.
type auditFilter struct {
	from, to                         string
	q, actor, service, tool, outcome string
}

func (f *auditFilter) bind(cmd *cobra.Command, withSearch bool) {
	cmd.Flags().StringVar(&f.from, "from", "", "First day, as YYYY-MM-DD")
	cmd.Flags().StringVar(&f.to, "to", "", "Last day, as YYYY-MM-DD")
	if withSearch {
		cmd.Flags().StringVar(&f.q, "q", "", "Search text")
		cmd.Flags().StringVar(&f.actor, "actor", "", "Only this actor")
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
	for k, v := range map[string]string{"q": f.q, "actor": f.actor, "service": f.service, "tool": f.tool, "outcome": f.outcome} {
		if v != "" {
			q.Set(k, v)
		}
	}
	for k, vs := range extra {
		q[k] = vs
	}
	return q, nil
}

// AuditCmd returns `relay audit`, the client for the gateway's tamper-evident
// audit log. Both commands need an organisation admin.
func AuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Verify and export the organisation's audit log (admins)",
		Long: `The gateway chains every audit event to the one before it and signs a daily
checkpoint. 'verify' asks the gateway to recompute the chain and check the
checkpoints; it exits 1 when the log does not verify, naming the first broken
event. 'export' downloads the events, filtered, as NDJSON or CSV.`,
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

func auditExportCmd() *cobra.Command {
	var f approvalFlags
	var filter auditFilter
	var format, output string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Download audit events as NDJSON or CSV",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !slices.Contains(auditFormats, format) {
				return fmt.Errorf("invalid --format %q: use one of %s", format, strings.Join(auditFormats, ", "))
			}
			q, err := filter.query(url.Values{"format": {format}})
			if err != nil {
				return err
			}
			dest := cmd.OutOrStdout()
			if output != "" {
				file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return fmt.Errorf("create %s (it must not exist yet): %w", output, err)
				}
				defer file.Close()
				dest = file
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			ctx, cancel, err := requestContext(cmd.Context(), transferRequest)
			if err != nil {
				return err
			}
			defer cancel()
			resp, err := c.GetContext(ctx, "/api/admin/audit/export?"+q.Encode())
			if err != nil {
				return timeoutCause(ctx, fmt.Errorf("GET /api/admin/audit/export: %w", err))
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				raw, _ := readLimitedResponse(resp.Body, maxApprovalBody)
				return gatewayStatusError(resp.StatusCode, raw)
			}
			n, err := io.Copy(dest, resp.Body)
			if err != nil {
				return timeoutCause(ctx, fmt.Errorf("download the export: %w", err))
			}
			if output != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Wrote %d bytes to %s\n", n, output)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&f.gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	filter.bind(cmd, true)
	cmd.Flags().StringVar(&format, "format", "ndjson", "Export format: "+strings.Join(auditFormats, " or "))
	cmd.Flags().StringVar(&output, "output", "", "Write to this new file (mode 0600) instead of stdout")
	return cmd
}
