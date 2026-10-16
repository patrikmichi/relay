package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type meanStat struct {
	Mean   float64 `json:"mean"`
	Stddev float64 `json:"stddev"`
	N      int     `json:"n"`
}

type triggerScore struct {
	Precision meanStat `json:"precision"`
	Recall    meanStat `json:"recall"`
	F1        meanStat `json:"f1"`
	Cases     int      `json:"cases"`
}

type taskScore struct {
	Score    meanStat `json:"score"`
	PassRate float64  `json:"passRate"`
	Cases    int      `json:"cases"`
}

type variantSummary struct {
	Label        string                  `json:"label"`
	VersionID    string                  `json:"versionId"`
	DraftID      string                  `json:"draftId"`
	Trigger      map[string]triggerScore `json:"trigger"`
	Task         map[string]taskScore    `json:"task"`
	InputTokens  int                     `json:"inputTokens"`
	OutputTokens int                     `json:"outputTokens"`
	CostUSD      float64                 `json:"costUsd"`
	LatencyMs    meanStat                `json:"latencyMs"`
	Errors       int                     `json:"errors"`
	Skipped      int                     `json:"skipped"`
}

type winLoss struct {
	Wins   int `json:"wins"`
	Losses int `json:"losses"`
	Ties   int `json:"ties"`
}

type significance struct {
	MeanDelta   float64 `json:"meanDelta"`
	CILow       float64 `json:"ciLow"`
	CIHigh      float64 `json:"ciHigh"`
	PValue      float64 `json:"pValue"`
	Significant bool    `json:"significant"`
}

type comparison struct {
	Baseline     string                  `json:"baseline"`
	Variant      string                  `json:"variant"`
	WinLoss      map[string]winLoss      `json:"winLoss"`
	Significance map[string]significance `json:"significance"`
}

type regressionAlert struct {
	VersionID         string  `json:"versionId"`
	PreviousVersionID string  `json:"previousVersionId"`
	Kind              string  `json:"kind"`
	Score             float64 `json:"score"`
	Previous          float64 `json:"previous"`
}

type runSummary struct {
	Total       int               `json:"total"`
	Completed   int               `json:"completed"`
	Variants    []variantSummary  `json:"variants"`
	Comparisons []comparison      `json:"comparisons"`
	Regressions []regressionAlert `json:"regressions"`
}

type evalRun struct {
	ID           string `json:"id"`
	SetID        string `json:"setId"`
	Kind         string `json:"kind"`
	Status       string `json:"status"`
	Repeats      int    `json:"repeats"`
	Model        string `json:"model"`
	JudgeModel   string `json:"judgeModel"`
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
	// The gateway sends null here until the run ends.
	Error   *string     `json:"error"`
	Summary *runSummary `json:"summary"`
}

type runDoc struct {
	Run      evalRun `json:"run"`
	Progress struct {
		Done  int `json:"done"`
		Total int `json:"total"`
	} `json:"progress"`
}

func (r evalRun) errorText() string {
	if r.Error != nil {
		return *r.Error
	}
	return ""
}

func terminalStatus(s string) bool { return s == "succeeded" || s == "failed" || s == "cancelled" }

// waitFor calls check every evalPollInterval until it reports done, the
// timeout passes or ctx ends. status is shown on stderr unless quiet.
func waitFor(ctx context.Context, timeout time.Duration, progress io.Writer, quiet bool, resume string, check func() (done bool, status string, err error)) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		done, status, err := check()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if !quiet && status != last {
			fmt.Fprintln(progress, status)
			last = status
		}
		if timeout > 0 && time.Now().After(deadline) {
			return fmt.Errorf("still not finished after %s (%s); it keeps running on the gateway, resume with: %s: %w", timeout, status, resume, context.DeadlineExceeded)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped waiting (%s); it keeps running on the gateway, resume with: %s: %w", status, resume, ctx.Err())
		case <-time.After(evalPollInterval):
		}
	}
}

type runOptions struct {
	set       string
	kind      string
	versions  []string
	drafts    []string
	repeats   int
	splits    []string
	model     string
	judge     string
	maxTokens int

	wait             bool
	waitTimeout      time.Duration
	failOnRegression bool
	runID            string
	cancel           bool
}

func (o *runOptions) addFlags(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&o.set, "set", "", "Eval set id or name (default: the skill's only set)")
	fl.StringVar(&o.kind, "kind", "", "trigger or task (default: the kind the set holds)")
	fl.IntVar(&o.repeats, "repeats", 0, "Repeats per case, 1 to 10 (default 3)")
	fl.StringSliceVar(&o.splits, "split", nil, "Score only train or validation (default: both)")
	fl.StringVar(&o.model, "model", "", "Model to run (default: the gateway's runtime model)")
	fl.StringVar(&o.judge, "judge-model", "", "Model that judges rubric cases")
	fl.IntVar(&o.maxTokens, "max-tokens", 0, "Lower the token budget of the run")
	fl.BoolVar(&o.wait, "wait", false, "Wait for the run to finish and print its scores")
	fl.DurationVar(&o.waitTimeout, "wait-timeout", 20*time.Minute, "Longest time to wait with --wait")
	fl.BoolVar(&o.failOnRegression, "fail-on-regression", false, fmt.Sprintf("Wait, and exit %d when a score dropped (implies --wait)", exitEvalRegression))
	fl.StringVar(&o.runID, "run", "", "Show (and with --wait, wait for) an existing run instead of starting one")
	fl.BoolVar(&o.cancel, "cancel", false, "With --run: cancel the run")
}

// variants builds the run's variant list.
func (o *runOptions) variants(ctx context.Context, a *evalAPI) ([]map[string]any, error) {
	var out []map[string]any
	for _, ref := range o.versions {
		v, err := a.resolveVersion(ctx, ref)
		if err != nil {
			return nil, err
		}
		label := v.Semver
		if label == "" {
			label = v.ID
		}
		out = append(out, map[string]any{"versionId": v.ID, "label": label})
	}
	for _, id := range o.drafts {
		out = append(out, map[string]any{"draftId": id, "label": "draft " + id})
	}
	return out, nil
}

// pickKind returns the case kind to run: the flag, else the only kind the set has.
func pickKind(flag string, set evalSet) (string, error) {
	switch {
	case flag == "trigger" || flag == "task":
		return flag, nil
	case flag != "":
		return "", fmt.Errorf("--kind must be trigger or task")
	case set.Counts.Task == 0 && set.Counts.Trigger > 0:
		return "trigger", nil
	case set.Counts.Trigger == 0 && set.Counts.Task > 0:
		return "task", nil
	case set.Counts.Trigger == 0:
		return "", fmt.Errorf("set %s has no cases; add some with `relay skill evals case add`", set.Name)
	}
	return "", fmt.Errorf("set %s has trigger and task cases; choose with --kind", set.Name)
}

// SkillEvalCmd returns `relay skill eval`.
func SkillEvalCmd() *cobra.Command {
	var f evalFlags
	var o runOptions
	var version, draft string
	cmd := &cobra.Command{
		Use:   "eval <skill>",
		Short: "Score a skill version or draft against an eval set",
		Long: fmt.Sprintf(`Start an eval run: every case of a set is sent to the model with the skill, repeated
--repeats times, and scored. A trigger run scores whether the model loads the
skill; a task run grades the answers. Without --wait the run starts and its id
is printed; with --wait relay polls and prints the scores.

Exit status: 0 on success, %d when the run failed or was cancelled, %d with
--fail-on-regression when a validation score dropped against the previous
version or a variant got significantly worse than the baseline, 4 when
--wait-timeout passed. Use --json for machine-readable output.

Examples:
  relay skill eval weekly-timesheet --wait
  relay skill eval weekly-timesheet --set smoke --version 1.2.0 --repeats 5 --wait --json
  relay skill eval weekly-timesheet --draft sdr_123 --wait
  relay skill eval weekly-timesheet --run evr_123 --wait
  relay skill eval weekly-timesheet --fail-on-regression    # in CI`, exitEvalFailed, exitEvalRegression),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if version != "" && draft != "" {
				return fmt.Errorf("--version and --draft cannot be used together")
			}
			if version != "" {
				o.versions = []string{version}
			}
			if draft != "" {
				o.drafts = []string{draft}
			}
			return runEval(cmd, f, args[0], o, false)
		},
	}
	f.add(cmd)
	o.addFlags(cmd)
	cmd.Flags().StringVar(&version, "version", "", "Version to score: semver or rev_ id (default: the current published version)")
	cmd.Flags().StringVar(&draft, "draft", "", "Score an optimization draft (sdr_ id) instead of a version")
	return cmd
}

// SkillCompareCmd returns `relay skill compare`.
func SkillCompareCmd() *cobra.Command {
	var f evalFlags
	var o runOptions
	var versions []string
	cmd := &cobra.Command{
		Use:   "compare <skill> --versions a,b",
		Short: "Score two or more versions or drafts side by side",
		Long: fmt.Sprintf(`Run one eval set against several versions (semver or rev_ ids) or drafts
(sdr_ ids) in a single run. The first is the baseline; each other variant is
compared with it per case, with wins, losses, ties and a paired bootstrap test.

With --fail-on-regression the command exits %d when a variant is significantly
worse than the baseline on validation.

Example:
  relay skill compare weekly-timesheet --versions 1.1.0,1.2.0 --wait`, exitEvalRegression),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, v := range versions {
				if strings.HasPrefix(v, "sdr_") {
					o.drafts = append(o.drafts, v)
				} else {
					o.versions = append(o.versions, v)
				}
			}
			if len(versions) < 2 && o.runID == "" {
				return fmt.Errorf("--versions needs at least two versions or drafts, for example --versions 1.1.0,1.2.0")
			}
			return runEval(cmd, f, args[0], o, true)
		},
	}
	f.add(cmd)
	o.addFlags(cmd)
	cmd.Flags().StringSliceVar(&versions, "versions", nil, "Comma-separated versions (semver or rev_ ids) or drafts (sdr_ ids); the first is the baseline")
	return cmd
}

func runEval(cmd *cobra.Command, f evalFlags, skillRef string, o runOptions, compare bool) error {
	a, err := f.open(cmd, skillRef)
	if err != nil {
		return err
	}
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	waiting := o.wait || o.failOnRegression

	runID := o.runID
	if runID != "" && o.cancel {
		raw, err := a.do(ctx, http.MethodDelete, "/evals/runs/"+url.PathEscape(runID), nil)
		if err != nil {
			return err
		}
		if f.jsonOut {
			return printIndented(out, raw)
		}
		fmt.Fprintf(out, "Cancelled run %s\n", runID)
		return nil
	}

	if runID == "" {
		set, err := a.resolveSet(ctx, o.set)
		if err != nil {
			return err
		}
		kind, err := pickKind(o.kind, set)
		if err != nil {
			return err
		}
		if len(o.versions)+len(o.drafts) == 0 {
			o.versions = []string{""}
		}
		variants, err := o.variants(ctx, a)
		if err != nil {
			return err
		}
		if compare && len(variants) < 2 {
			return fmt.Errorf("compare needs at least two variants")
		}
		body := map[string]any{"setId": set.ID, "kind": kind, "variants": variants}
		if o.repeats != 0 {
			body["repeats"] = o.repeats
		}
		if len(o.splits) > 0 {
			body["splits"] = o.splits
		}
		if o.model != "" {
			body["model"] = o.model
		}
		if o.judge != "" {
			body["judgeModel"] = o.judge
		}
		if o.maxTokens != 0 {
			body["maxTokens"] = o.maxTokens
		}
		var started runDoc
		raw, err := a.sendJSON(ctx, http.MethodPost, "/evals/runs", body, &started)
		if err != nil {
			return err
		}
		runID = started.Run.ID
		if !waiting {
			if f.jsonOut {
				return printIndented(out, raw)
			}
			fmt.Fprintf(out, "Started %s run %s on set %s.\nFollow it with: relay skill eval %s --run %s --wait\n", kind, runID, set.Name, a.label(), runID)
			return nil
		}
	}

	var doc runDoc
	var raw []byte
	fetch := func() error {
		var err error
		raw, err = a.getJSON(ctx, "/evals/runs/"+url.PathEscape(runID), &doc)
		return err
	}
	if waiting {
		resume := fmt.Sprintf("relay skill eval %s --run %s --wait", a.label(), runID)
		err = waitFor(ctx, o.waitTimeout, cmd.ErrOrStderr(), f.jsonOut, resume, func() (bool, string, error) {
			if err := fetch(); err != nil {
				return false, "", err
			}
			return terminalStatus(doc.Run.Status), fmt.Sprintf("run %s: %s %d/%d", runID, doc.Run.Status, doc.Progress.Done, doc.Progress.Total), nil
		})
		if err != nil {
			return err
		}
		if o.failOnRegression && doc.Run.Status == "succeeded" && len(findRegressions(doc.Run)) == 0 {
			// The gateway records regressions just after it marks the run succeeded.
			select {
			case <-ctx.Done():
			case <-time.After(evalSettleDelay):
			}
			if err := fetch(); err != nil {
				return err
			}
		}
	} else if err := fetch(); err != nil {
		return err
	}

	if f.jsonOut {
		if err := printIndented(out, raw); err != nil {
			return err
		}
	} else {
		renderRun(out, doc)
	}
	return runExit(doc.Run, o.failOnRegression)
}

// runExit maps a finished run to the process status.
func runExit(r evalRun, failOnRegression bool) error {
	switch r.Status {
	case "failed", "cancelled":
		msg := fmt.Sprintf("run %s %s", r.ID, r.Status)
		if e := r.errorText(); e != "" {
			msg += ": " + e
		}
		return &ExitError{Code: exitEvalFailed, Msg: msg}
	case "succeeded":
		if regs := findRegressions(r); failOnRegression && len(regs) > 0 {
			return &ExitError{Code: exitEvalRegression, Msg: "regression: " + strings.Join(regs, "; ")}
		}
	}
	return nil
}

// findRegressions lists what got worse: a version scoring below its
// predecessor on validation, or a variant significantly below the baseline.
func findRegressions(r evalRun) []string {
	if r.Summary == nil {
		return nil
	}
	var out []string
	for _, g := range r.Summary.Regressions {
		out = append(out, fmt.Sprintf("%s validation score fell from %.0f%% to %.0f%% (%s vs %s)", g.Kind, g.Previous*100, g.Score*100, g.VersionID, g.PreviousVersionID))
	}
	for _, c := range r.Summary.Comparisons {
		split := "validation"
		sig, ok := c.Significance[split]
		if !ok {
			split = "all"
			sig, ok = c.Significance[split]
		}
		if ok && sig.Significant && sig.MeanDelta < 0 {
			out = append(out, fmt.Sprintf("%s is significantly worse than %s on %s (mean delta %+.2f, p=%.3f)", c.Variant, c.Baseline, split, sig.MeanDelta, sig.PValue))
		}
	}
	return out
}

var splitOrder = []string{"train", "validation", "all"}

func (m meanStat) pm() string {
	if m.N < 2 {
		return fmt.Sprintf("%.2f", m.Mean)
	}
	return fmt.Sprintf("%.2f ±%.2f", m.Mean, m.Stddev)
}

func renderRun(out io.Writer, d runDoc) {
	r := d.Run
	fmt.Fprintf(out, "Run %s  %s  %s eval  set %s\n", r.ID, r.Status, r.Kind, r.SetID)
	if r.Model != "" {
		fmt.Fprintf(out, "Model %s, %d repeats, %d tokens in / %d out\n", r.Model, r.Repeats, r.InputTokens, r.OutputTokens)
	}
	if e := r.errorText(); e != "" {
		fmt.Fprintf(out, "Error: %s\n", e)
	}
	if r.Summary == nil {
		fmt.Fprintf(out, "Progress %d/%d\n", d.Progress.Done, d.Progress.Total)
		return
	}
	s := r.Summary
	fmt.Fprintf(out, "Scored %d of %d case runs\n", s.Completed, s.Total)
	for _, v := range s.Variants {
		fmt.Fprintf(out, "\n%s\n", v.Label)
		for _, split := range splitOrder {
			if t, ok := v.Trigger[split]; ok {
				fmt.Fprintf(out, "  %-11s F1 %s  precision %.2f  recall %.2f  (%d cases)\n", split, t.F1.pm(), t.Precision.Mean, t.Recall.Mean, t.Cases)
			}
			if t, ok := v.Task[split]; ok {
				fmt.Fprintf(out, "  %-11s score %s  pass %.0f%%  (%d cases)\n", split, t.Score.pm(), t.PassRate*100, t.Cases)
			}
		}
		fmt.Fprintf(out, "  %d tokens in / %d out, $%.4f, latency %.0f ms, %d errors, %d skipped\n", v.InputTokens, v.OutputTokens, v.CostUSD, v.LatencyMs.Mean, v.Errors, v.Skipped)
	}
	for _, c := range s.Comparisons {
		fmt.Fprintf(out, "\n%s vs %s\n", c.Variant, c.Baseline)
		for _, split := range splitOrder {
			wl, hasWL := c.WinLoss[split]
			sig, hasSig := c.Significance[split]
			if !hasWL && !hasSig {
				continue
			}
			verdict := "not significant"
			if sig.Significant {
				verdict = "significant"
			}
			fmt.Fprintf(out, "  %-11s %d wins, %d losses, %d ties; mean delta %+.2f (CI %.2f to %.2f, p=%.3f, %s)\n", split, wl.Wins, wl.Losses, wl.Ties, sig.MeanDelta, sig.CILow, sig.CIHigh, sig.PValue, verdict)
		}
	}
	if regs := findRegressions(r); len(regs) > 0 {
		fmt.Fprintln(out, "\nRegressions:")
		for _, g := range regs {
			fmt.Fprintf(out, "  - %s\n", g)
		}
	}
}
