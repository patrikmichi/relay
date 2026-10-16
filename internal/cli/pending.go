package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/client"
)

const pendingApprovalStatus = "pending_approval"

// pendingApproval is the result of a call the gateway held for a human
// decision instead of running it.
type pendingApproval struct {
	Status     string `json:"status"`
	ApprovalID string `json:"approval_id"`
	Service    string `json:"service"`
	Tool       string `json:"tool"`
	Risk       string `json:"risk"`
	ArgsDigest string `json:"args_digest"`
	ExpiresAt  string `json:"expires_at"`
}

func parsePending(raw []byte) *pendingApproval {
	var p pendingApproval
	if json.Unmarshal(raw, &p) != nil || p.Status != pendingApprovalStatus || p.ApprovalID == "" {
		return nil
	}
	return &p
}

// pendingFromREST recognises the HTTP 202 a REST /call answers with.
func pendingFromREST(status int, body []byte) *pendingApproval {
	if status != http.StatusAccepted {
		return nil
	}
	return parsePending(body)
}

// pendingFromResult recognises a held call in an MCP tool result: the
// structured content, or the JSON text of the first block for a server that
// sends only content.
func pendingFromResult(r *mcpToolResult) *pendingApproval {
	if r.IsError {
		return nil
	}
	if len(r.StructuredContent) > 0 {
		return parsePending(r.StructuredContent)
	}
	if len(r.Content) > 0 {
		if text, ok := contentBlockText(r.Content[0]); ok {
			return parsePending([]byte(text))
		}
	}
	return nil
}

// callOpts holds what a tool call does about a held call.
type callOpts struct {
	wait    bool
	waitFor time.Duration
}

// addWaitFlag registers --wait. A bare --wait waits the default time; a
// limit is given as --wait=SECONDS.
func addWaitFlag(cmd *cobra.Command, v *string) {
	cmd.Flags().StringVar(v, "wait", "", "If the gateway holds the call for approval, wait for the decision and print the result; limit in seconds or a duration (--wait=90)")
	cmd.Flags().Lookup("wait").NoOptDefVal = strconv.Itoa(defaultWaitSeconds)
}

func callOptsFor(cmd *cobra.Command, wait string) (callOpts, error) {
	if !cmd.Flags().Changed("wait") {
		return callOpts{}, nil
	}
	d, err := parseWaitTimeout(wait)
	if err != nil {
		return callOpts{}, err
	}
	return callOpts{wait: true, waitFor: d}, nil
}

// handlePending reports a held call and exits with the code for "still
// pending", or with --wait collects the result like `relay approvals wait`.
func handlePending(parent context.Context, c *client.Client, p *pendingApproval, jsonOut bool, o callOpts, out io.Writer) error {
	if !o.wait {
		if !jsonOut {
			fmt.Fprintln(out, "Approval required: this call is held until a human decides it.")
			fmt.Fprintf(out, "  approval: %s\n  call:     %s %s\n  risk:     %s\n  expires:  %s\n", p.ApprovalID, p.Service, p.Tool, p.Risk, p.ExpiresAt)
			fmt.Fprintf(out, "Collect the result with `relay approvals wait %s`, or repeat the call with --wait.\n", p.ApprovalID)
		}
		return &ExitError{Code: exitPending}
	}
	if !jsonOut {
		fmt.Fprintf(os.Stderr, "Approval %s required (%s, expires %s). Waiting up to %s for a decision...\n", p.ApprovalID, p.Risk, p.ExpiresAt, o.waitFor)
	}
	return waitForApproval(parent, c, p.ApprovalID, o.waitFor, out, jsonOut)
}
