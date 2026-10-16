package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// Exit statuses of `zuano approvals wait` and of a call run with --wait.
// 0 means approved and executed; any other failure is 1.
const (
	exitDenied  = 10
	exitExpired = 11
	exitPending = 12

	defaultWaitSeconds = 300
	maxAwaitSeconds    = 30
	maxApprovalBody    = 1 << 20
)

var awaitRetryDelay = time.Second

var approvalStatuses = []string{"pending", "approved", "denied", "executed", "failed", "expired", "all"}

// approvalRecord mirrors the approval object of the gateway's approvals API.
type approvalRecord struct {
	ApprovalID   string `json:"approval_id"`
	Status       string `json:"status"`
	Service      string `json:"service"`
	Tool         string `json:"tool"`
	Risk         string `json:"risk"`
	ArgsDigest   string `json:"args_digest"`
	Requester    string `json:"requester"`
	Client       string `json:"client"`
	Reason       string `json:"reason"`
	RuleID       string `json:"rule_id"`
	CreatedAt    string `json:"created_at"`
	ExpiresAt    string `json:"expires_at"`
	DecidedAt    string `json:"decided_at"`
	Approver     string `json:"approver"`
	GrantMinutes int    `json:"grant_minutes"`
	Note         string `json:"note"`
	ExecutedAt   string `json:"executed_at"`
}

// sanitized returns the record with control characters removed from every
// text field, since the gateway's values are printed on a terminal.
func (a approvalRecord) sanitized() approvalRecord {
	for _, f := range []*string{&a.ApprovalID, &a.Status, &a.Service, &a.Tool, &a.Risk, &a.ArgsDigest, &a.Requester, &a.Client, &a.Reason, &a.RuleID, &a.CreatedAt, &a.ExpiresAt, &a.DecidedAt, &a.Approver, &a.Note, &a.ExecutedAt} {
		*f = textsafe.Clean(*f)
	}
	return a
}

// awaitOutcome mirrors the body of POST /api/approvals/{id}/await.
type awaitOutcome struct {
	Status     string          `json:"status"`
	ApprovalID string          `json:"approval_id"`
	Executed   bool            `json:"executed"`
	Result     json.RawMessage `json:"result"`
	Error      string          `json:"error"`
	Message    string          `json:"message"`
	Approver   string          `json:"approver"`
	Note       string          `json:"note"`
	ExpiresAt  string          `json:"expires_at"`
}

type approvalFlags struct {
	gatewayURL string
	jsonOut    bool
}

func (f *approvalFlags) bind(cmd *cobra.Command, jsonHelp string) {
	cmd.Flags().StringVar(&f.gatewayURL, "gateway-url", "", "Gateway URL (default: $ZUANO_URL, then the config file)")
	cmd.Flags().BoolVar(&f.jsonOut, "json", false, jsonHelp)
}

// ApprovalsCmd returns `zuano approvals`, the client for the gateway's
// approvals API.
func ApprovalsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "List held calls, open their approval page and wait for the result",
		Long: `Calls from an agent or a personal CLI session wait for a person. These
commands list the held calls, take you to the page where you decide one, and
collect the result of one that was approved.

A decision is made only in a browser, with your passkey: 'approve' and 'deny'
open the approval page (or print a link and a QR code for your phone) and send
no decision themselves. Collecting (wait) needs the session that made the
call, so run it with the same ZUANO_AGENT setting.

wait exit status
  0   approved and executed
  1   the call failed, or an HTTP or usage error
  10  denied
  11  expired
  12  still pending when the timeout ended`,
	}
	cmd.AddCommand(approvalsListCmd(), approvalsShowCmd(), approvalsDecideCmd(true), approvalsDecideCmd(false), approvalsWaitCmd())
	return cmd
}

func approvalsClient(gatewayURL string) (*client.Client, error) {
	gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
	if err != nil {
		return nil, err
	}
	return resolveClient(gURL)
}

// approvalsDo sends one request to the approvals API and returns the status
// and body, bounded by the limit for kind.
func approvalsDo(parent context.Context, c *client.Client, kind requestKind, method, path string, body any) (status int, raw []byte, err error) {
	ctx, cancel, err := requestContext(parent, kind)
	if err != nil {
		return 0, nil, err
	}
	defer cancel()
	defer func() { err = timeoutCause(ctx, err) }()

	var resp *http.Response
	switch method {
	case http.MethodGet:
		resp, err = c.GetContext(ctx, path)
	case http.MethodPost:
		payload, merr := json.Marshal(body)
		if merr != nil {
			return 0, nil, fmt.Errorf("marshal request body: %w", merr)
		}
		resp, err = c.PostContext(ctx, path, "application/json", bytes.NewReader(payload))
	default:
		var payload io.Reader
		if body != nil {
			raw, merr := json.Marshal(body)
			if merr != nil {
				return 0, nil, fmt.Errorf("marshal request body: %w", merr)
			}
			payload = bytes.NewReader(raw)
		}
		req, rerr := http.NewRequestWithContext(ctx, method, c.GatewayURL+path, payload)
		if rerr != nil {
			return 0, nil, fmt.Errorf("create request: %w", rerr)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err = c.Do(req)
	}
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err = readLimitedResponse(resp.Body, maxApprovalBody)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, raw, nil
}

func approvalError(status int, raw []byte) error {
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	msg := body.Error
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch status {
	case http.StatusUnauthorized:
		return errors.New("token expired or revoked — run `zuano login` to re-authenticate")
	case http.StatusForbidden:
		return fmt.Errorf("%s (HTTP 403): approvals are for an org owner, admin or listed approver using their own session, not the agent profile", msg)
	}
	return fmt.Errorf("%s (HTTP %d)%s", msg, status, client.RequestIDNote())
}

func printRaw(out io.Writer, raw []byte) {
	fmt.Fprintln(out, strings.TrimSpace(string(raw)))
}

func approvalsListCmd() *cobra.Command {
	var f approvalFlags
	var status string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List approvals, newest first (at most 200)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !slices.Contains(approvalStatuses, status) {
				return fmt.Errorf("invalid --status %q: use one of %s", status, strings.Join(approvalStatuses, ", "))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			code, raw, err := approvalsDo(cmd.Context(), c, controlRequest, http.MethodGet, "/api/approvals?status="+url.QueryEscape(status), nil)
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return approvalError(code, raw)
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			var list struct {
				Approvals []approvalRecord `json:"approvals"`
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("decode approvals: %w", err)
			}
			if len(list.Approvals) == 0 {
				fmt.Fprintf(out, "No %s approvals\n", status)
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATUS\tCALL\tRISK\tREQUESTER\tEXPIRES")
			for _, a := range list.Approvals {
				a = a.sanitized()
				fmt.Fprintf(tw, "%s\t%s\t%s.%s\t%s\t%s\t%s\n", a.ApprovalID, a.Status, a.Service, a.Tool, a.Risk, a.Requester, a.ExpiresAt)
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&status, "status", "pending", "Filter: "+strings.Join(approvalStatuses, ", "))
	return cmd
}

func approvalsShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one approval and a preview of the call's arguments",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			code, raw, err := approvalsDo(cmd.Context(), c, controlRequest, http.MethodGet, "/api/approvals/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return approvalError(code, raw)
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			var doc struct {
				Approval    approvalRecord  `json:"approval"`
				ArgsPreview json.RawMessage `json:"args_preview"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("decode approval: %w", err)
			}
			printApproval(out, doc.Approval)
			if len(doc.ArgsPreview) > 0 && string(doc.ArgsPreview) != "null" {
				fmt.Fprintf(out, "Arguments: %s\n", indentJSON(doc.ArgsPreview))
			} else {
				fmt.Fprintln(out, "Arguments: no longer available")
			}
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func printApproval(out io.Writer, a approvalRecord) {
	a = a.sanitized()
	fmt.Fprintf(out, "Approval:  %s (%s)\n", a.ApprovalID, a.Status)
	fmt.Fprintf(out, "Call:      %s %s\n", a.Service, a.Tool)
	reason := a.Reason
	if a.RuleID != "" {
		reason += " " + a.RuleID
	}
	fmt.Fprintf(out, "Risk:      %s (%s)\n", a.Risk, reason)
	requester := a.Requester
	if a.Client != "" {
		requester += " via " + a.Client
	}
	fmt.Fprintf(out, "Requester: %s\n", requester)
	fmt.Fprintf(out, "Created:   %s\n", a.CreatedAt)
	fmt.Fprintf(out, "Expires:   %s\n", a.ExpiresAt)
	if a.Approver != "" {
		fmt.Fprintf(out, "Decided:   %s by %s\n", a.DecidedAt, a.Approver)
	}
	if a.GrantMinutes > 0 {
		fmt.Fprintf(out, "Grant:     writes to %s for %d minutes\n", a.Service, a.GrantMinutes)
	}
	if a.Note != "" {
		fmt.Fprintf(out, "Note:      %s\n", a.Note)
	}
	if a.ExecutedAt != "" {
		fmt.Fprintf(out, "Executed:  %s\n", a.ExecutedAt)
	}
}

func indentJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return textsafe.Block(string(raw))
	}
	return textsafe.Block(buf.String())
}

var approvalPollInterval = 2 * time.Second

// approvalPageURL is the page where a person decides one approval, on the
// gateway's own origin.
func approvalPageURL(gatewayOrigin, id, decision string) string {
	return strings.TrimRight(gatewayOrigin, "/") + "/approvals/" + url.PathEscape(id) + "?decision=" + decision
}

type decideFlags struct {
	approvalFlags
	noBrowser   bool
	wait        bool
	waitTimeout string
}

// approvalsDecideCmd returns `zuano approvals approve` or `deny`. Neither
// decides anything: a decision is a passkey confirmation in a browser, on this
// computer or a phone, so the command only takes the person to that page.
func approvalsDecideCmd(approve bool) *cobra.Command {
	var f decideFlags
	decision := "deny"
	short := "Open the page where a held call is denied"
	if approve {
		decision = "approve"
		short = "Open the page where a held call is approved"
	}
	cmd := &cobra.Command{
		Use:   decision + " <id>",
		Short: short,
		Long: `Opens the gateway's approval page for the held call in your browser. You review
the whole call there and confirm with your passkey; zuano sends no decision
itself, so nothing running as your user can approve a call through the CLI.

With --no-browser, or in an agent session or a script, zuano prints the link
and a QR code instead: scan the code to decide with the passkey on your phone.

--wait then polls the approval until it is decided and exits like 'zuano
approvals wait': 0 approved, 10 denied, 11 expired, 12 still pending when the
timeout ended.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDecidePage(cmd, f, decision, args[0])
		},
	}
	f.bind(cmd, "Print the link and the outcome as one JSON document")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "Print the link and a QR code for a phone instead of opening a browser")
	cmd.Flags().BoolVar(&f.wait, "wait", false, "Wait until the approval is decided")
	cmd.Flags().StringVar(&f.waitTimeout, "wait-timeout", strconv.Itoa(defaultWaitSeconds), "How long --wait polls, in seconds or as a duration such as 90s or 5m")
	return cmd
}

func runDecidePage(cmd *cobra.Command, f decideFlags, decision, id string) error {
	total, err := parseWaitTimeout(f.waitTimeout)
	if err != nil {
		return err
	}
	gURL, err := resolveGatewayURLOrFailClosed(f.gatewayURL)
	if err != nil {
		return err
	}
	origin, err := config.NormalizeGatewayURL(gURL)
	if err != nil {
		return err
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	c, clientErr := resolveClient(gURL)
	if clientErr == nil {
		if rec, ok, err := lookupApproval(cmd.Context(), c, id); err != nil {
			return err
		} else if ok {
			if rec.Status != "pending" {
				return fmt.Errorf("approval %s is already %s; nothing is left to decide", textsafe.Clean(id), textsafe.Clean(rec.Status))
			}
			if !f.jsonOut {
				printApproval(out, rec)
			}
		}
	} else if f.wait {
		return clientErr
	}

	page := approvalPageURL(origin, id, decision)
	opened := false
	if f.jsonOut {
		opened = openIfInteractive(cmd, page, f.noBrowser)
	} else {
		opened = showLink(cmd, page, f.noBrowser, "Decide it with your passkey")
	}

	status := "pending"
	exit := 0
	if f.wait {
		if !f.jsonOut {
			fmt.Fprintf(errOut, "Waiting up to %s for the decision...\n", total)
		}
		status, exit, err = pollDecision(cmd.Context(), c, id, total, out, f.jsonOut)
		if err != nil {
			return err
		}
	}
	if f.jsonOut {
		doc, err := json.Marshal(map[string]any{"approval_id": id, "decision": decision, "url": page, "opened": opened, "status": status})
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(doc))
	}
	if exit != 0 {
		return &ExitError{Code: exit}
	}
	return nil
}

// lookupApproval fetches the approval so the person sees what the page will
// show and an already-decided one is not opened. A gateway that cannot answer
// does not block opening the page; an unknown id does.
func lookupApproval(ctx context.Context, c *client.Client, id string) (approvalRecord, bool, error) {
	code, raw, err := approvalsDo(ctx, c, controlRequest, http.MethodGet, "/api/approvals/"+url.PathEscape(id), nil)
	if err != nil {
		return approvalRecord{}, false, err
	}
	switch code {
	case http.StatusOK:
		var doc struct {
			Approval approvalRecord `json:"approval"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			return approvalRecord{}, false, nil
		}
		return doc.Approval, true, nil
	case http.StatusNotFound:
		return approvalRecord{}, false, approvalError(code, raw)
	}
	return approvalRecord{}, false, nil
}

// pollDecision polls an approval until it leaves pending or total passes and
// returns its status with the matching exit status.
func pollDecision(ctx context.Context, c *client.Client, id string, total time.Duration, out io.Writer, jsonOut bool) (string, int, error) {
	deadline := time.Now().Add(total)
	for {
		code, raw, err := approvalsDo(ctx, c, controlRequest, http.MethodGet, "/api/approvals/"+url.PathEscape(id), nil)
		if err != nil {
			return "", 0, err
		}
		if code != http.StatusOK {
			return "", 0, approvalError(code, raw)
		}
		var doc struct {
			Approval approvalRecord `json:"approval"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return "", 0, fmt.Errorf("decode approval: %w", err)
		}
		a := doc.Approval.sanitized()
		if a.Status != "pending" {
			if !jsonOut {
				fmt.Fprintf(out, "%s: %s\n", textsafe.Clean(id), decisionSummary(a))
			}
			return a.Status, decisionExit(a.Status), nil
		}
		if time.Now().After(deadline) {
			if !jsonOut {
				fmt.Fprintf(out, "Still pending (expires %s).\n", a.ExpiresAt)
			}
			return "pending", exitPending, nil
		}
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(approvalPollInterval):
		}
	}
}

func decisionSummary(a approvalRecord) string {
	who := "an approver"
	if a.Approver != "" {
		who = a.Approver
	}
	switch a.Status {
	case "approved", "executed":
		return a.Status + " by " + who
	case "denied":
		return "denied by " + who
	}
	return a.Status
}

// decisionExit maps a decided approval to the exit status documented for wait.
func decisionExit(status string) int {
	switch status {
	case "approved", "executed":
		return 0
	case "denied":
		return exitDenied
	case "expired":
		return exitExpired
	case "pending":
		return exitPending
	}
	return 1
}

func approvalsWaitCmd() *cobra.Command {
	var f approvalFlags
	var timeout string
	cmd := &cobra.Command{
		Use:   "wait <id>",
		Short: "Wait for a decision, then print the result of the approved call",
		Long: `Waits until the approval is decided or the timeout ends, runs the approved call
once and prints its result. Only the session that made the call can collect
it, so run this with the same ZUANO_AGENT setting as the call.

Exit status: 0 approved and executed, 1 the call failed or an error, 10
denied, 11 expired, 12 still pending when the timeout ended.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			total, err := parseWaitTimeout(timeout)
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			return waitForApproval(cmd.Context(), c, args[0], total, cmd.OutOrStdout(), f.jsonOut)
		},
	}
	f.bind(cmd, "Print the last response body unchanged")
	cmd.Flags().StringVar(&timeout, "wait-timeout", strconv.Itoa(defaultWaitSeconds), "How long to wait, in seconds or as a duration such as 90s or 5m; 0 asks once")
	return cmd
}

// parseWaitTimeout reads a wait limit given in seconds or as a duration.
func parseWaitTimeout(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	if n, err := strconv.Atoi(s); err == nil {
		d = time.Duration(n) * time.Second
	} else if d, err = time.ParseDuration(s); err != nil {
		return 0, fmt.Errorf("invalid wait timeout %q: expected seconds or a duration such as 90s or 5m", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("wait timeout must not be negative")
	}
	return d, nil
}

// settled reports whether another await could change the outcome.
func (o awaitOutcome) settled() bool {
	switch o.Status {
	case "pending":
		return false
	case "approved":
		return o.Executed
	}
	return true
}

// exitCode maps an outcome to the exit status documented for wait.
func (o awaitOutcome) exitCode() int {
	switch o.Status {
	case "approved":
		if o.Executed && o.Error == "" {
			return 0
		}
		if o.Executed {
			return 1
		}
		return exitPending
	case "denied":
		return exitDenied
	case "expired":
		return exitExpired
	case "pending":
		return exitPending
	}
	return 1
}

// waitForApproval awaits id until it is decided or total passes, printing the
// final outcome and returning the matching exit status as an ExitError.
func waitForApproval(parent context.Context, c *client.Client, id string, total time.Duration, out io.Writer, jsonOut bool) error {
	deadline := time.Now().Add(total)
	path := "/api/approvals/" + url.PathEscape(id) + "/await"
	for {
		secs := min(max(int(time.Until(deadline)/time.Second), 0), maxAwaitSeconds)
		asked := time.Now()
		code, raw, err := approvalsDo(parent, c, toolCallRequest, http.MethodPost, path, map[string]int{"timeout_s": secs})
		if err != nil {
			return err
		}
		if code != http.StatusOK && code != http.StatusNotFound {
			return approvalError(code, raw)
		}
		var o awaitOutcome
		if err := json.Unmarshal(raw, &o); err != nil {
			return fmt.Errorf("decode await response: %w", err)
		}
		if code == http.StatusNotFound {
			o.Status = "not_found"
		}
		if o.settled() || time.Until(deadline) < time.Second {
			return reportOutcome(out, jsonOut, id, raw, o)
		}
		// The gateway holds an await open until the timeout; one that answers
		// at once (the call is running in another request) must not be asked
		// again in a tight loop.
		if time.Since(asked) < awaitRetryDelay {
			select {
			case <-parent.Done():
				return parent.Err()
			case <-time.After(awaitRetryDelay):
			}
		}
	}
}

func reportOutcome(out io.Writer, jsonOut bool, id string, raw []byte, o awaitOutcome) error {
	code := o.exitCode()
	o.Error, o.Message, o.Approver, o.Note, o.ExpiresAt = textsafe.Clean(o.Error), textsafe.Clean(o.Message), textsafe.Clean(o.Approver), textsafe.Clean(o.Note), textsafe.Clean(o.ExpiresAt)
	if jsonOut {
		printRaw(out, raw)
		if code == 0 {
			return nil
		}
		return &ExitError{Code: code}
	}
	switch o.Status {
	case "approved":
		switch {
		case o.Executed && o.Error == "":
			printToolOutput(out, o.Result)
			return nil
		case o.Executed:
			return &ExitError{Code: 1, Msg: fmt.Sprintf("approval %s was approved and the call failed: %s", id, o.Error)}
		}
		fmt.Fprintf(out, "Approved, but the call has not run yet: %s\nRun `zuano approvals wait %s` again.\n", o.Message, id)
	case "denied":
		who := "an approver"
		if o.Approver != "" {
			who = o.Approver
		}
		fmt.Fprintf(out, "Denied by %s\n", who)
		if o.Note != "" {
			fmt.Fprintf(out, "Note: %s\n", o.Note)
		}
	case "expired":
		fmt.Fprintln(out, "Expired: nobody decided within 15 minutes. Run the call again to ask anew.")
	case "pending":
		fmt.Fprintf(out, "Still pending (expires %s). Run `zuano approvals wait %s` to keep waiting.\n", o.ExpiresAt, id)
	default:
		return &ExitError{Code: 1, Msg: fmt.Sprintf("approval %s not found, or it was made by another session; collect it with the ZUANO_AGENT setting of the session that made the call", id)}
	}
	return &ExitError{Code: code}
}

// printToolOutput prints the output of a tool the gateway ran for an approval.
func printToolOutput(out io.Writer, result json.RawMessage) {
	var text string
	if json.Unmarshal(result, &text) == nil {
		fmt.Fprintln(out, text)
		return
	}
	if len(result) == 0 {
		fmt.Fprintln(out, "{}")
		return
	}
	fmt.Fprintln(out, indentJSON(result))
}
