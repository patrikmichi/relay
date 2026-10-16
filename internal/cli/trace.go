package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

// currentOrg names the organisation the credential belongs to in the
// organisation routes.
const currentOrg = "current"

var traceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

type traceSpan struct {
	SpanID       string  `json:"span_id"`
	ParentSpanID *string `json:"parent_span_id"`
	Kind         string  `json:"kind"`
	Name         string  `json:"name"`
	StartTS      string  `json:"start_ts"`
	DurationMS   int64   `json:"duration_ms"`
	Status       string  `json:"status"`
	Service      string  `json:"service"`
	Tool         string  `json:"tool"`
	Outcome      string  `json:"outcome"`
}

type auditEvent struct {
	TS         string `json:"ts"`
	Event      string `json:"event"`
	UserEmail  string `json:"user_email"`
	Service    string `json:"service"`
	Tool       string `json:"tool"`
	Outcome    string `json:"outcome"`
	Risk       string `json:"risk"`
	DurationMS int64  `json:"duration_ms"`
	ApprovalID string `json:"approval_id"`
	RequestID  string `json:"request_id"`
	TraceID    string `json:"trace_id"`
}

type traceError struct {
	TS            string `json:"ts"`
	Level         string `json:"level"`
	Route         string `json:"route"`
	Message       string `json:"message"`
	ExceptionType string `json:"exception_type"`
	Fingerprint   string `json:"fingerprint"`
}

// traceLookup mirrors GET /api/org/{org}/observability/lookup.
type traceLookup struct {
	ID        string       `json:"id"`
	Matched   string       `json:"matched"`
	RequestID string       `json:"request_id"`
	TraceID   string       `json:"trace_id"`
	Spans     []traceSpan  `json:"spans"`
	Audit     []auditEvent `json:"audit"`
	Errors    []traceError `json:"errors"`
}

// TraceCmd returns `zuano trace`, which shows everything the gateway recorded
// for one request, trace or approval.
func TraceCmd() *cobra.Command {
	var f approvalFlags
	var org string
	cmd := &cobra.Command{
		Use:   "trace <request_id|trace_id|approval_id>",
		Short: "Show the audit entries, spans and errors recorded for a request (admins)",
		Long: `Look up a request id (req_..., shown in error messages and by --debug), a trace
id (32 hex characters) or an approval id and print the spans as a tree, the
audit entries and any errors the gateway linked to it. Needs an organisation
admin or owner.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !traceIDPattern.MatchString(id) {
				return fmt.Errorf("%q is not a request id, trace id or approval id", textsafe.Clean(id))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			path := "/api/org/" + url.PathEscape(org) + "/observability/lookup?id=" + url.QueryEscape(id)
			status, raw, err := approvalsDo(cmd.Context(), c, controlRequest, http.MethodGet, path, nil)
			if err != nil {
				return err
			}
			switch status {
			case http.StatusOK:
			case http.StatusForbidden:
				return fmt.Errorf("tracing needs an organisation admin or owner (HTTP 403); ask one to run this, or to share the request id with support")
			case http.StatusNotFound:
				return fmt.Errorf("nothing recorded for %s: the id is wrong, belongs to another organisation, or is older than your plan's retention", textsafe.Clean(id))
			default:
				return gatewayStatusError(status, raw)
			}
			var doc traceLookup
			if err := json.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("decode the lookup: %w", err)
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			printTrace(cmd.OutOrStdout(), doc)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&org, "org", currentOrg, "Organisation id (default: the one your credential belongs to)")
	return cmd
}

func printTrace(out io.Writer, d traceLookup) {
	fmt.Fprintf(out, "Request: %s\nTrace:   %s\n", orDash(d.RequestID), orDash(d.TraceID))
	fmt.Fprintf(out, "\nSpans (%d)\n", len(d.Spans))
	printSpanTree(out, d.Spans)
	fmt.Fprintf(out, "\nAudit (%d)\n", len(d.Audit))
	for _, e := range d.Audit {
		fmt.Fprintf(out, "  %s  %s  %s  outcome=%s", textsafe.Clean(e.TS), textsafe.Clean(e.Event), textsafe.Clean(joinTool(e.Service, e.Tool)), orDash(textsafe.Clean(e.Outcome)))
		if e.Risk != "" {
			fmt.Fprintf(out, "  risk=%s", textsafe.Clean(e.Risk))
		}
		if e.UserEmail != "" {
			fmt.Fprintf(out, "  user=%s", textsafe.Clean(e.UserEmail))
		}
		if e.ApprovalID != "" {
			fmt.Fprintf(out, "  approval=%s", textsafe.Clean(e.ApprovalID))
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "\nErrors (%d)\n", len(d.Errors))
	for _, e := range d.Errors {
		fmt.Fprintf(out, "  %s  %s  %s  %s\n", textsafe.Clean(e.TS), orDash(textsafe.Clean(e.Level)), orDash(textsafe.Clean(e.Route)), textsafe.Clean(e.Message))
	}
}

func printSpanTree(out io.Writer, spans []traceSpan) {
	byID := make(map[string]bool, len(spans))
	children := map[string][]traceSpan{}
	for _, s := range spans {
		byID[s.SpanID] = true
	}
	var roots []traceSpan
	for _, s := range spans {
		if s.ParentSpanID == nil || !byID[*s.ParentSpanID] || *s.ParentSpanID == s.SpanID {
			roots = append(roots, s)
			continue
		}
		children[*s.ParentSpanID] = append(children[*s.ParentSpanID], s)
	}
	byStart := func(list []traceSpan) {
		sort.SliceStable(list, func(i, j int) bool { return list[i].StartTS < list[j].StartTS })
	}
	byStart(roots)
	seen := map[string]bool{}
	var walk func(s traceSpan, depth int)
	walk = func(s traceSpan, depth int) {
		if seen[s.SpanID] || depth > 32 {
			return
		}
		seen[s.SpanID] = true
		name := s.Name
		if label := joinTool(s.Service, s.Tool); label != "" {
			name += " " + label
		}
		fmt.Fprintf(out, "  %s%s [%s]  %s  %dms\n", strings.Repeat("  ", depth), textsafe.Clean(name), textsafe.Clean(s.Kind), orDash(textsafe.Clean(firstNonEmptyString(s.Outcome, s.Status))), s.DurationMS)
		kids := children[s.SpanID]
		byStart(kids)
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
}

func joinTool(service, tool string) string {
	switch {
	case service != "" && tool != "":
		return service + "." + tool
	case service != "":
		return service
	}
	return tool
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
