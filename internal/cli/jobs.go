package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/textsafe"
)

const jobIDPrefix = "job_"

// jobPollDelay is how long to wait before the attempt-th status check of a
// started job: a second at first, then longer, never more than five.
var jobPollDelay = func(attempt int) time.Duration {
	return min(time.Second+time.Duration(attempt)*time.Second, 5*time.Second)
}

// jobDoc mirrors a job of the gateway's durable-jobs API.
type jobDoc struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Attempts   int             `json:"attempts"`
	Progress   json.RawMessage `json:"progress"`
	Error      *string         `json:"error"`
	CreatedAt  string          `json:"created_at"`
	StartedAt  *string         `json:"started_at"`
	FinishedAt *string         `json:"finished_at"`
}

func (j jobDoc) sanitized() jobDoc {
	j.ID, j.Kind, j.Status, j.CreatedAt = textsafe.Clean(j.ID), textsafe.Clean(j.Kind), textsafe.Clean(j.Status), textsafe.Clean(j.CreatedAt)
	if j.Error != nil {
		e := textsafe.Clean(*j.Error)
		j.Error = &e
	}
	return j
}

func (j jobDoc) errorText() string {
	if j.Error == nil {
		return ""
	}
	return *j.Error
}

// jobTerminal reports whether a job can no longer change. queued, running
// and retrying are still going; dead is a job that failed every retry.
func jobTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "dead":
		return true
	}
	return false
}

// startedJob is the answer to a call that started work on the gateway
// instead of finishing it: 202 {run_id, job_id, status}.
type startedJob struct {
	RunID  string `json:"run_id"`
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

func parseStartedJob(raw []byte) *startedJob {
	var j startedJob
	if json.Unmarshal(raw, &j) != nil || (j.JobID == "" && j.RunID == "") {
		return nil
	}
	switch j.Status {
	case "queued", "running", "retrying":
		return &j
	}
	return nil
}

// jobFromREST recognises the 202 a REST route answers with.
func jobFromREST(status int, body []byte) *startedJob {
	if status != http.StatusAccepted {
		return nil
	}
	return parseStartedJob(body)
}

// jobFromResult recognises started work in an MCP tool result.
func jobFromResult(r *mcpToolResult) *startedJob {
	if r.IsError || len(r.StructuredContent) == 0 {
		return nil
	}
	return parseStartedJob(r.StructuredContent)
}

func (j *startedJob) pollID() string {
	if j.JobID != "" {
		return j.JobID
	}
	return j.RunID
}

// handleStartedJob reports work the gateway started and, with --wait, follows
// it to the end like 'relay runs wait'.
func handleStartedJob(parent context.Context, c *client.Client, j *startedJob, jsonOut bool, o callOpts, out io.Writer) error {
	if !o.wait {
		if !jsonOut {
			fmt.Fprintf(out, "Started on the gateway: %s (%s).\n", textsafe.Clean(j.pollID()), textsafe.Clean(j.Status))
			fmt.Fprintf(out, "Follow it with `relay runs show %s`, or repeat the call with --wait.\n", textsafe.Clean(j.pollID()))
		}
		return nil
	}
	if !jsonOut {
		fmt.Fprintf(os.Stderr, "Started %s (%s). Waiting up to %s for it to finish...\n", textsafe.Clean(j.pollID()), textsafe.Clean(j.Status), o.waitFor)
	}
	return waitForJob(parent, c, j.pollID(), o.waitFor, out, jsonOut)
}

// jobPath is the route that reports a job or a run.
func jobPath(id string) string {
	if strings.HasPrefix(id, jobIDPrefix) {
		return "/api/jobs/" + url.PathEscape(id)
	}
	return "/api/runtime/runs/" + url.PathEscape(id)
}

// jobStatusOf reads the state of a job or run from either route: a job
// document ({"job": {...}}) or a run document with a job_status.
func jobStatusOf(raw []byte) (jobDoc, error) {
	var doc struct {
		Job       *jobDoc `json:"job"`
		Run       *jobDoc `json:"run"`
		JobStatus string  `json:"job_status"`
		Status    string  `json:"status"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return jobDoc{}, fmt.Errorf("decode job: %w", err)
	}
	switch {
	case doc.Job != nil:
		return *doc.Job, nil
	case doc.Run != nil:
		j := *doc.Run
		if j.Status == "" || doc.JobStatus != "" {
			j.Status = firstNonEmptyString(doc.JobStatus, j.Status)
		}
		return j, nil
	}
	return jobDoc{Status: firstNonEmptyString(doc.JobStatus, doc.Status)}, nil
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// waitForJob polls a job or run until it finishes or total passes, prints it
// and returns the matching exit status: 0 succeeded, 1 failed, dead or
// cancelled, 12 still going when the time ended.
func waitForJob(ctx context.Context, c *client.Client, id string, total time.Duration, out io.Writer, jsonOut bool) error {
	deadline := time.Now().Add(total)
	for attempt := 0; ; attempt++ {
		var doc jobDoc
		raw, err := gatewayJSON(ctx, c, http.MethodGet, jobPath(id), nil, nil)
		if err == nil {
			doc, err = jobStatusOf(raw)
		}
		if err != nil {
			return err
		}
		if jobTerminal(doc.Status) || time.Now().After(deadline) {
			return reportJob(out, jsonOut, id, raw, doc)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jobPollDelay(attempt)):
		}
	}
}

func reportJob(out io.Writer, jsonOut bool, id string, raw []byte, doc jobDoc) error {
	doc = doc.sanitized()
	if jsonOut {
		printRaw(out, raw)
	} else {
		printJob(out, id, doc)
	}
	switch doc.Status {
	case "succeeded":
		return nil
	case "failed", "dead", "cancelled":
		msg := fmt.Sprintf("%s %s", textsafe.Clean(id), doc.Status)
		if e := doc.errorText(); e != "" {
			msg += ": " + e
		}
		if jsonOut {
			return &ExitError{Code: 1}
		}
		return &ExitError{Code: 1, Msg: msg}
	}
	return &ExitError{Code: exitPending}
}

func printJob(out io.Writer, id string, j jobDoc) {
	if j.ID == "" {
		j.ID = textsafe.Clean(id)
	}
	fmt.Fprintf(out, "Job:      %s (%s)\n", j.ID, j.Status)
	if j.Kind != "" {
		fmt.Fprintf(out, "Kind:     %s\n", j.Kind)
	}
	if j.Attempts > 0 {
		fmt.Fprintf(out, "Attempts: %d\n", j.Attempts)
	}
	if len(j.Progress) > 0 && string(j.Progress) != "null" && string(j.Progress) != "{}" {
		fmt.Fprintf(out, "Progress: %s\n", textsafe.Clean(string(j.Progress)))
	}
	if j.CreatedAt != "" {
		fmt.Fprintf(out, "Created:  %s\n", j.CreatedAt)
	}
	if j.StartedAt != nil {
		fmt.Fprintf(out, "Started:  %s\n", textsafe.Clean(*j.StartedAt))
	}
	if j.FinishedAt != nil {
		fmt.Fprintf(out, "Finished: %s\n", textsafe.Clean(*j.FinishedAt))
	}
	if e := j.errorText(); e != "" {
		fmt.Fprintf(out, "Error:    %s\n", e)
	}
}

// RunsCmd returns `relay runs`, the client for the gateway's durable jobs.
func RunsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List, inspect, wait for, cancel and retry the gateway's background runs",
		Long: `Agent runs, evals, webhook deliveries and host builds run on the gateway as
jobs that survive restarts. A job is queued, running, retrying, or finished:
succeeded, failed, cancelled, or dead (it failed every retry).

An id that starts with job_ is a job; any other id is looked up as a run.

wait exit status
  0   succeeded
  1   failed, dead or cancelled
  12  still going when the timeout ended`,
	}
	cmd.AddCommand(runsListCmd(), runsShowCmd(), runsWaitCmd(), runsCancelCmd(), runsRetryCmd())
	return cmd
}

func runsListCmd() *cobra.Command {
	var f approvalFlags
	var kind, status, cursor string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List jobs, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := url.Values{}
			for k, v := range map[string]string{"kind": kind, "status": status, "cursor": cursor} {
				if v != "" {
					q.Set(k, v)
				}
			}
			path := "/api/jobs"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var list struct {
				Jobs       []jobDoc `json:"jobs"`
				NextCursor *string  `json:"nextCursor"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &list)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(list.Jobs) == 0 {
				fmt.Fprintln(out, "No jobs")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tSTATUS\tATTEMPTS\tCREATED")
			for _, j := range list.Jobs {
				j = j.sanitized()
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", j.ID, j.Kind, j.Status, j.Attempts, j.CreatedAt)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if list.NextCursor != nil && *list.NextCursor != "" {
				fmt.Fprintf(out, "More: relay runs list --cursor %s\n", textsafe.Clean(*list.NextCursor))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&kind, "kind", "", "Only this kind, such as agent_run or webhook_delivery")
	cmd.Flags().StringVar(&status, "status", "", "Only this status: queued, running, succeeded, failed, cancelled or dead")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continue after the cursor a previous list printed")
	return cmd
}

func runsShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one job or run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, jobPath(args[0]), nil, nil)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			doc, err := jobStatusOf(raw)
			if err != nil {
				return err
			}
			printJob(out, args[0], doc.sanitized())
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func runsWaitCmd() *cobra.Command {
	var f approvalFlags
	var timeout string
	cmd := &cobra.Command{
		Use:   "wait <id>",
		Short: "Wait for a job or run to finish",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			total, err := parseWaitTimeout(timeout)
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			return waitForJob(cmd.Context(), c, args[0], total, cmd.OutOrStdout(), f.jsonOut)
		},
	}
	f.bind(cmd, "Print the last response body unchanged")
	cmd.Flags().StringVar(&timeout, "wait-timeout", "300", "How long to wait, in seconds or as a duration such as 90s or 5m; 0 asks once")
	return cmd
}

func runsCancelCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Ask the gateway to stop a job or run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodPost, jobPath(args[0])+"/cancel", map[string]any{}, nil)
			if err != nil {
				return err
			}
			return printJobAction(cmd.OutOrStdout(), f.jsonOut, args[0], raw, "Cancel requested")
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func runsRetryCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "retry <job-id>",
		Short: "Start a failed or dead job again (organisation admins)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !strings.HasPrefix(args[0], jobIDPrefix) {
				return errors.New("retry takes a job id (job_...); find it with `relay runs list --status dead`")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodPost, jobPath(args[0])+"/retry", map[string]any{}, nil)
			if err != nil {
				return err
			}
			return printJobAction(cmd.OutOrStdout(), f.jsonOut, args[0], raw, "Retry started")
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func printJobAction(out io.Writer, jsonOut bool, id string, raw []byte, done string) error {
	if jsonOut {
		printRaw(out, raw)
		return nil
	}
	doc, err := jobStatusOf(raw)
	if err != nil {
		return err
	}
	doc = doc.sanitized()
	if doc.ID == "" {
		doc.ID = textsafe.Clean(id)
	}
	fmt.Fprintf(out, "%s: %s (%s)\n", done, doc.ID, doc.Status)
	return nil
}
