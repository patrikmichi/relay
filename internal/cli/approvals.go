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

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/keychain"
)

// Exit statuses of `relay approvals wait` and of a call run with --wait.
// 0 means approved and executed; any other failure is 1.
const (
	exitDenied  = 10
	exitExpired = 11
	exitPending = 12

	defaultWaitSeconds = 300
	maxAwaitSeconds    = 30
	maxApprovalBody    = 1 << 20
	maxApprovalNote    = 500
	maxGrantMinutes    = 480
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

var errAgentCannotDecide = errors.New("the agent profile cannot approve or deny calls, and the gateway refuses it: an agent must not decide its own approvals. Unset RELAY_AGENT and run this with your own session (`relay login`)")

type approvalFlags struct {
	gatewayURL string
	jsonOut    bool
}

func (f *approvalFlags) bind(cmd *cobra.Command, jsonHelp string) {
	cmd.Flags().StringVar(&f.gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, then the config file)")
	cmd.Flags().BoolVar(&f.jsonOut, "json", false, jsonHelp)
}

// ApprovalsCmd returns `relay approvals`, the client for the gateway's
// approvals API.
func ApprovalsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "List, decide and wait for approvals of agent calls",
		Long: `An agent's writes wait for a human. These commands list the held calls,
decide them and collect the result of one that was approved.

Deciding (list, show, approve, deny) needs your own session: an org owner,
admin or listed approver, signed in with 'relay login' and without
RELAY_AGENT. The agent profile is refused. Collecting (wait) needs the
session that made the call, so run it with the same RELAY_AGENT setting.

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
	if method == http.MethodGet {
		resp, err = c.GetContext(ctx, path)
	} else {
		payload, merr := json.Marshal(body)
		if merr != nil {
			return 0, nil, fmt.Errorf("marshal request body: %w", merr)
		}
		resp, err = c.PostContext(ctx, path, "application/json", bytes.NewReader(payload))
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
		return errors.New("token expired or revoked — run `relay login` to re-authenticate")
	case http.StatusForbidden:
		return fmt.Errorf("%s (HTTP 403): approvals are for an org owner, admin or listed approver using their own session, not the agent profile", msg)
	}
	return fmt.Errorf("%s (HTTP %d)", msg, status)
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
		return string(raw)
	}
	return buf.String()
}

func approvalsDecideCmd(approve bool) *cobra.Command {
	var f approvalFlags
	var note string
	var grant int
	action, past := "deny", "Denied"
	short := "Deny a held call"
	if approve {
		action, past = "approve", "Approved"
		short = "Approve a held call; the agent collects it with await or by repeating the call"
	}
	cmd := &cobra.Command{
		Use:   action + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if client.ActiveProfile() == keychain.ProfileAgent {
				return errAgentCannotDecide
			}
			body := map[string]any{}
			if len(note) > maxApprovalNote {
				return fmt.Errorf("--note is at most %d characters", maxApprovalNote)
			}
			if note != "" {
				body["note"] = note
			}
			if cmd.Flags().Changed("grant") {
				if grant < 1 || grant > maxGrantMinutes {
					return fmt.Errorf("--grant is a number of minutes from 1 to %d", maxGrantMinutes)
				}
				body["grant_minutes"] = grant
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			code, raw, err := approvalsDo(cmd.Context(), c, controlRequest, http.MethodPost, "/api/approvals/"+url.PathEscape(args[0])+"/"+action, body)
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
				Approval approvalRecord `json:"approval"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("decode approval: %w", err)
			}
			a := doc.Approval
			fmt.Fprintf(out, "%s %s: %s %s (%s)\n", past, a.ApprovalID, a.Service, a.Tool, a.Risk)
			if a.GrantMinutes > 0 {
				fmt.Fprintf(out, "Writes to %s are allowed for this agent session for %d minutes\n", a.Service, a.GrantMinutes)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&note, "note", "", fmt.Sprintf("Note recorded with the decision (at most %d characters)", maxApprovalNote))
	if approve {
		cmd.Flags().IntVar(&grant, "grant", 0, fmt.Sprintf("For a write: also let this agent session write to the service for MINUTES (1 to %d)", maxGrantMinutes))
	}
	return cmd
}

func approvalsWaitCmd() *cobra.Command {
	var f approvalFlags
	var timeout string
	cmd := &cobra.Command{
		Use:   "wait <id>",
		Short: "Wait for a decision, then print the result of the approved call",
		Long: `Waits until the approval is decided or the timeout ends, runs the approved call
once and prints its result. Only the session that made the call can collect
it, so run this with the same RELAY_AGENT setting as the call.

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
	cmd.Flags().StringVar(&timeout, "timeout", strconv.Itoa(defaultWaitSeconds), "How long to wait, in seconds or as a duration such as 90s or 5m; 0 asks once")
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
		fmt.Fprintf(out, "Approved, but the call has not run yet: %s\nRun `relay approvals wait %s` again.\n", o.Message, id)
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
		fmt.Fprintf(out, "Still pending (expires %s). Run `relay approvals wait %s` to keep waiting.\n", o.ExpiresAt, id)
	default:
		return &ExitError{Code: 1, Msg: fmt.Sprintf("approval %s not found, or it was made by another session; collect it with the RELAY_AGENT setting of the session that made the call", id)}
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
