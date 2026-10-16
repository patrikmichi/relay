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
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// automationPollDelay is how long to wait between checks of a run.
var automationPollDelay = 2 * time.Second

type runStepDoc struct {
	Seq           int    `json:"seq"`
	Path          string `json:"path"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	Error         string `json:"error"`
	ApprovalID    string `json:"approvalId"`
	ApprovalURL   string `json:"approvalUrl"`
	PreapprovalID string `json:"preapprovalId"`
}

type automationRunDoc struct {
	ID     string       `json:"id"`
	Status string       `json:"status"`
	Mode   string       `json:"mode"`
	Error  string       `json:"error"`
	Seq    int          `json:"seq"`
	Steps  []runStepDoc `json:"steps"`
}

func runTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "skipped":
		return true
	}
	return false
}

func fetchRun(ctx context.Context, c *client.Client, runID string, after int) (automationRunDoc, []byte, error) {
	var doc struct {
		Run automationRunDoc `json:"run"`
	}
	path := "/api/automations/runs/" + url.PathEscape(runID)
	if after > 0 {
		path += fmt.Sprintf("?after=%d", after)
	}
	raw, err := gatewayJSON(ctx, c, http.MethodGet, path, nil, &doc)
	return doc.Run, raw, err
}

func stepLine(s runStepDoc) string {
	line := fmt.Sprintf("  %-24s %s", textsafe.Clean(s.Path), textsafe.Clean(s.Status))
	if s.PreapprovalID != "" {
		line += " (pre-approved " + textsafe.Clean(s.PreapprovalID) + ")"
	}
	if s.Error != "" {
		line += ": " + textsafe.Clean(s.Error)
	}
	return line
}

func printStepAndLink(out io.Writer, c *client.Client, s runStepDoc) {
	fmt.Fprintln(out, stepLine(s))
	if s.Status == "held" && s.ApprovalURL != "" {
		if link, err := gatewayLink(c.GatewayURL, s.ApprovalURL); err == nil {
			fmt.Fprintf(out, "    waiting for approval: %s\n", textsafe.Clean(link))
		}
	}
}

// automationRunExit maps a finished run to the process exit status.
func automationRunExit(r automationRunDoc) error {
	switch r.Status {
	case "succeeded":
		return nil
	case "cancelled", "skipped":
		return &ExitError{Code: 1, Msg: fmt.Sprintf("run %s %s", textsafe.Clean(r.ID), r.Status)}
	}
	code := 20
	for _, s := range r.Steps {
		switch s.Status {
		case "denied":
			code = exitDenied
		case "expired":
			code = exitExpired
		}
	}
	msg := "run " + textsafe.Clean(r.ID) + " failed"
	if r.Error != "" {
		msg += ": " + textsafe.Clean(r.Error)
	}
	return &ExitError{Code: code, Msg: msg}
}

func anyHeld(r automationRunDoc) bool {
	for _, s := range r.Steps {
		if s.Status == "held" {
			return true
		}
	}
	return r.Status == "held"
}

// waitForRun follows a run until it finishes, waits for an approval or the
// time ends; the last two exit 12.
func waitForRun(ctx context.Context, c *client.Client, runID string, total time.Duration, out io.Writer, jsonOut bool) error {
	deadline := time.Now().Add(total)
	for {
		r, raw, err := fetchRun(ctx, c, runID, 0)
		if err != nil {
			return err
		}
		finished := runTerminal(r.Status)
		held := !finished && anyHeld(r)
		if finished || held || time.Now().After(deadline) {
			if jsonOut {
				printRaw(out, raw)
			} else {
				fmt.Fprintf(out, "Run %s: %s\n", textsafe.Clean(r.ID), textsafe.Clean(r.Status))
				for _, s := range r.Steps {
					printStepAndLink(out, c, s)
				}
			}
			if finished {
				return automationRunExit(r)
			}
			return &ExitError{Code: exitPending}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(automationPollDelay):
		}
	}
}

func automationRunCmd() *cobra.Command {
	var f approvalFlags
	var dryRun, test, yes bool
	var sample, event, inputFile, wait string
	cmd := &cobra.Command{
		Use:   "run <atm_id>",
		Short: "Run an automation now, or dry-run it without changing anything",
		Long: `--dry-run reads for real and shows what each step that would change something
would do, without doing it. --test runs it for real with a sample or a recent
event, under the normal approvals, and needs --yes. Without either, an enabled
automation runs live.

--wait follows the run (a duration such as 90s; plain --wait is 5m). It stops
when the run finishes, or when a step waits for an approval, which exits 12 and
prints the link where a person decides.`,
		Example: `  zuano automation run atm_k3j2h1 --dry-run --sample invoice-created
  zuano automation run atm_k3j2h1 --wait`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := automationID(args[0])
			if err != nil {
				return err
			}
			if dryRun && test {
				return errors.New("--dry-run and --test cannot be used together")
			}
			if n := countSet(sample != "", event != "", inputFile != ""); n > 1 {
				return errors.New("give at most one of --sample, --event and --input")
			}
			total, err := parseRunWait(wait)
			if err != nil {
				return err
			}
			mode := "live"
			switch {
			case dryRun:
				mode = "dry_run"
			case test:
				mode = "test"
				if err := confirmOrYes(cmd, yes, "Run it for real with this input, under the normal approvals?"); err != nil {
					return err
				}
			}
			body := map[string]any{"mode": mode}
			if sample != "" {
				body["sample"] = sample
			}
			if event != "" {
				body["eventId"] = event
			}
			if inputFile != "" {
				data, err := os.ReadFile(inputFile)
				if err != nil {
					return fmt.Errorf("read %s: %w", textsafe.Clean(inputFile), err)
				}
				var in any
				if err := json.Unmarshal(data, &in); err != nil {
					return fmt.Errorf("%s is not JSON: %w", textsafe.Clean(inputFile), err)
				}
				body["input"] = in
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var started struct {
				RunID string `json:"run_id"`
				JobID string `json:"job_id"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, automationPath(id, "run"), body, &started)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if started.RunID == "" {
				return errors.New("the gateway did not say which run started")
			}
			if total == 0 {
				if f.jsonOut {
					printRaw(out, raw)
					return nil
				}
				fmt.Fprintf(out, "Started %s. Follow it with `%s`.\n", textsafe.Clean(started.RunID), brand.Cmd("automation logs "+started.RunID+" --follow"))
				return nil
			}
			return waitForRun(cmd.Context(), c, started.RunID, total, out, f.jsonOut)
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Do not change anything; show what each changing step would do")
	cmd.Flags().BoolVar(&test, "test", false, "Run for real with a sample or event, under the normal approvals")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm --test without asking")
	cmd.Flags().StringVar(&sample, "sample", "", "Input: the template's sample with this name")
	cmd.Flags().StringVar(&event, "event", "", "Input: a recent event (wev_...) of the automation's endpoint")
	cmd.Flags().StringVar(&inputFile, "input", "", "Input: a JSON file (for a manual trigger)")
	cmd.Flags().StringVar(&wait, "wait", "", "Follow the run for up to this long (default 5m when given without a value)")
	cmd.Flags().Lookup("wait").NoOptDefVal = "5m"
	return cmd
}

func countSet(flags ...bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

func parseRunWait(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid --wait %q: use a duration such as 90s or 10m", textsafe.Clean(s))
	}
	return d, nil
}

func automationLogsCmd() *cobra.Command {
	var f approvalFlags
	var follow bool
	var step string
	cmd := &cobra.Command{
		Use:   "logs <atm_id|atr_id>",
		Short: "Show a run's steps; for an automation, its latest run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			runID := args[0]
			switch {
			case automationRunPattern.MatchString(runID):
			case automationIDPattern.MatchString(runID):
				var list struct {
					Runs []struct {
						ID string `json:"id"`
					} `json:"runs"`
				}
				if _, err := gatewayJSON(ctx, c, http.MethodGet, automationPath(runID, "runs"), nil, &list); err != nil {
					return err
				}
				if len(list.Runs) == 0 {
					fmt.Fprintln(out, "No runs yet.")
					return nil
				}
				runID = list.Runs[0].ID
			default:
				return fmt.Errorf("%q is neither an automation id (atm_...) nor a run id (atr_...)", textsafe.Clean(args[0]))
			}
			after, header := 0, false
			for {
				r, raw, err := fetchRun(ctx, c, runID, after)
				if err != nil {
					return err
				}
				if f.jsonOut {
					printRaw(out, raw)
				} else {
					if !header {
						fmt.Fprintf(out, "Run %s (%s): %s\n", textsafe.Clean(r.ID), textsafe.Clean(r.Mode), textsafe.Clean(r.Status))
						header = true
					}
					for _, s := range r.Steps {
						if step == "" || s.Path == step {
							printStepAndLink(out, c, s)
						}
					}
				}
				if runTerminal(r.Status) {
					if follow {
						return automationRunExit(r)
					}
					return nil
				}
				if !follow {
					return nil
				}
				after = r.Seq
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(automationPollDelay):
				}
			}
		},
	}
	f.bind(cmd, "Print each response body unchanged")
	cmd.Flags().BoolVar(&follow, "follow", false, "Keep polling and print step changes until the run finishes")
	cmd.Flags().StringVar(&step, "step", "", "Only this step path")
	return cmd
}
