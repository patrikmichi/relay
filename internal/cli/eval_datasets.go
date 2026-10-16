package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/evals"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

func evalDatasetsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "datasets", Short: "Import, push and pull datasets of cases"}
	cmd.AddCommand(datasetsImportCmd(), datasetsPushCmd(), datasetsPullCmd())
	return cmd
}

func datasetsImportCmd() *cobra.Command {
	var from, out, criteria string
	var columns []string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "import <file|dir>",
		Short: "Convert a file from another tool into a dataset of cases",
		Long: `Converts a file to the case format, one case per line, and prints what could not
be converted. Nothing is converted into something weaker: an assertion with no
equivalent is listed and left out.

Sources (--from)
  jsonl          cases in the ` + evals.CaseFormat + ` format; checks and normalises them
  csv            one case per row; --column maps a field to a header, such as input=question
  promptfoo      the tests of a promptfoo config (inline tests; assertions mapped where equal)
  openai         OpenAI Evals items, one JSON object per line; --criteria is the
                 testing_criteria file (string_check graders are mapped)
  skill-creator  a skill-creator evals.json (expectations become a rubric)
  claude-plugin  the evals directory of a Claude Code plugin (give the directory)

Imports run offline and need no login.`,
		Example: `  zuano eval datasets import promptfooconfig.yaml --from promptfoo --out evals/cases.jsonl
  zuano eval datasets import data.csv --from csv --column input=question --column contains=answer`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw []byte
			var err error
			if from != "claude-plugin" {
				if raw, err = os.ReadFile(args[0]); err != nil {
					return err
				}
			}
			var cases []evals.Case
			var rep *evals.ImportReport
			switch from {
			case "claude-plugin":
				cases, rep, err = evals.ImportClaudePlugin(args[0])
			case "jsonl":
				cases, err = evals.ReadJSONL(bytes.NewReader(raw))
				rep = &evals.ImportReport{Source: "jsonl", Cases: len(cases)}
			case "csv":
				cols := map[string]string{}
				for _, c := range columns {
					k, v, ok := strings.Cut(c, "=")
					if !ok || k == "" || v == "" {
						return fmt.Errorf("--column %q must be field=header", c)
					}
					cols[k] = v
				}
				cases, err = evals.ReadCSV(bytes.NewReader(raw), cols)
				rep = &evals.ImportReport{Source: "csv", Cases: len(cases)}
			case "promptfoo":
				cases, rep, err = evals.ImportPromptfoo(raw)
			case "openai":
				var crit []byte
				if criteria != "" {
					if crit, err = os.ReadFile(criteria); err != nil {
						return err
					}
				}
				cases, rep, err = evals.ImportOpenAI(raw, crit)
			case "skill-creator":
				cases, rep, err = evals.ImportSkillCreator(raw)
			default:
				return fmt.Errorf("--from must be one of %s", strings.Join(evals.ImportSources, ", "))
			}
			if err != nil {
				printImportReport(cmd.ErrOrStderr(), rep)
				return err
			}
			if out == "" {
				out = strings.TrimSuffix(args[0], filepath.Ext(args[0])) + ".cases.jsonl"
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			fh, err := os.Create(out)
			if err != nil {
				return err
			}
			if err := evals.WriteJSONL(fh, cases); err != nil {
				_ = fh.Close()
				return err
			}
			if err := fh.Close(); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				printRaw(w, jsonBytes(map[string]any{"output": out, "report": rep}))
				return nil
			}
			fmt.Fprintf(w, "Wrote %d case(s) to %s\n", len(cases), out)
			printImportReport(w, rep)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "jsonl", "Source format: "+strings.Join(evals.ImportSources, ", "))
	cmd.Flags().StringVar(&out, "out", "", "Output file (default: next to the input)")
	cmd.Flags().StringVar(&criteria, "criteria", "", "OpenAI Evals testing criteria file")
	cmd.Flags().StringArrayVar(&columns, "column", nil, "CSV field=header mapping, repeatable")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the report as JSON")
	return cmd
}

func printImportReport(w io.Writer, rep *evals.ImportReport) {
	if rep == nil {
		return
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "note: %s\n", textsafe.Strict(n))
	}
	if len(rep.Unmapped) == 0 {
		return
	}
	fmt.Fprintf(w, "%d item(s) were not converted:\n", len(rep.Unmapped))
	for _, u := range rep.Unmapped {
		fmt.Fprintf(w, "  %s: %s: %s\n", textsafe.Strict(u.Case), textsafe.Strict(u.Type), textsafe.Strict(u.Reason))
	}
}

func datasetsPushCmd() *cobra.Command {
	var f approvalFlags
	var name, id, sensitivity string
	cmd := &cobra.Command{
		Use:   "push <cases.jsonl>",
		Short: "Upload a dataset as a new version",
		Long: `Creates a dataset (--name) or adds a version to one (--dataset). Editing a
dataset always creates a version; suites pin a version or follow the latest.
The gateway scans the cases for secrets and personal data on the way in, and
secrets are always redacted.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			cases, err := evals.ReadJSONL(bytes.NewReader(raw))
			if err != nil {
				return err
			}
			if (name == "") == (id == "") {
				return errors.New("give --name to create a dataset or --dataset to add a version to one")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if id == "" {
				var created struct {
					Dataset struct {
						ID string `json:"id"`
					} `json:"dataset"`
				}
				if _, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/evals/datasets", map[string]string{"name": name, "sensitivity": sensitivity}, &created); err != nil {
					return err
				}
				id = created.Dataset.ID
			}
			var version struct {
				Version struct {
					Seq       int `json:"seq"`
					CaseCount int `json:"case_count"`
				} `json:"version"`
			}
			rawOut, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/evals/datasets/"+url.PathEscape(id)+"/versions", map[string]any{"cases": cases}, &version)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), rawOut)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Dataset %s is at version %d with %d case(s)\n", id, version.Version.Seq, version.Version.CaseCount)
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().StringVar(&name, "name", "", "Create a dataset with this name")
	cmd.Flags().StringVar(&id, "dataset", "", "Add a version to this dataset id")
	cmd.Flags().StringVar(&sensitivity, "sensitivity", "internal", "Which providers may see it: public, internal, confidential or secret")
	return cmd
}

func datasetsPullCmd() *cobra.Command {
	var f approvalFlags
	var version int
	var out string
	cmd := &cobra.Command{
		Use:   "pull <dataset-id>",
		Short: "Download a dataset version as cases",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			id := url.PathEscape(args[0])
			if version == 0 {
				var vs struct {
					Versions []struct {
						Seq int `json:"seq"`
					} `json:"versions"`
				}
				if _, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/evals/datasets/"+id+"/versions", nil, &vs); err != nil {
					return err
				}
				for _, v := range vs.Versions {
					if v.Seq > version {
						version = v.Seq
					}
				}
				if version == 0 {
					return fmt.Errorf("dataset %s has no versions", args[0])
				}
			}
			var cases struct {
				Cases []evals.Case `json:"cases"`
			}
			if _, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/evals/datasets/"+id+"/versions/"+strconv.Itoa(version)+"/cases", nil, &cases); err != nil {
				return err
			}
			if len(cases.Cases) == 0 {
				return fmt.Errorf("version %d of %s has no cases", version, args[0])
			}
			var buf bytes.Buffer
			if err := evals.WriteJSONL(&buf, cases.Cases); err != nil {
				return err
			}
			if _, err := evals.ReadJSONL(bytes.NewReader(buf.Bytes())); err != nil {
				return fmt.Errorf("the gateway sent cases this CLI cannot read: %w", err)
			}
			if out == "" {
				_, err = cmd.OutOrStdout().Write(buf.Bytes())
				return err
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %d case(s) of version %d to %s\n", len(cases.Cases), version, out)
			return nil
		},
	}
	f.bind(cmd, "Unused: the cases are written as JSONL")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().IntVar(&version, "version", 0, "Version to download (default: the latest)")
	cmd.Flags().StringVar(&out, "out", "", "File to write (default: standard output)")
	return cmd
}

func evalOptimizeCmd() *cobra.Command {
	var f approvalFlags
	var suite, target, slot, models, sink string
	var candidates, repeats int
	var overfit float64
	var useTest, apply, noWait bool
	cmd := &cobra.Command{
		Use:   "optimize",
		Short: "Let the gateway propose a better description or prompt, checked against overfitting",
		Long: `Proposes candidates from the failures on the training split, scores them on
training and validation, discards a candidate whose training gain exceeds its
validation gain by more than --overfit, and discards candidates that copy case
text. --test-split scores the winner once on the holdout, which the proposer
never sees. Online only; needs a plan with optimization.

--apply sends the winner to its sink: a catalog draft for a skill, a patch
file otherwise.`,
		Example: `  zuano eval optimize --target skill:pr-triage --slot description --test-split
  zuano eval optimize --suite my-prompt --slot system --candidates 6 --apply`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if err := evalsAgentGate(ctx, c, brand.Cmd("eval optimize")); err != nil {
				return err
			}
			if slot == "" {
				return errors.New("--slot names what to rewrite, such as description or system")
			}
			if suite == "" {
				if target == "" {
					return errors.New("name the suite with --suite or --target skill:<slug>")
				}
				if suite, err = suiteByTarget(ctx, c, target); err != nil {
					return err
				}
			}
			body := map[string]any{"suiteId": suite, "slot": slot, "candidates": candidates, "repeats": repeats, "overfitThreshold": overfit, "useTestSplit": useTest}
			if m := commaList(models); len(m) > 0 {
				body["models"] = m
			}
			var started struct {
				OptimizeID string `json:"optimizeId"`
				Estimate   struct {
					Units int `json:"units"`
				} `json:"estimate"`
			}
			if _, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/evals/optimize", body, &started); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !f.jsonOut {
				fmt.Fprintf(out, "Started optimization %s (%d case runs reserved)\n", started.OptimizeID, started.Estimate.Units)
			}
			if noWait {
				return nil
			}
			job, raw, err := waitForOptimize(ctx, c, started.OptimizeID)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(out, raw)
			} else {
				renderOptimize(out, job)
			}
			if job.Status != "succeeded" {
				return &ExitError{Code: exitRunFailed, Msg: "optimization " + job.Status + ": " + textsafe.Strict(job.Error)}
			}
			if apply {
				if job.Result.Winner == "" {
					return errors.New("no candidate beat the base without overfitting, so there is nothing to apply")
				}
				if _, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/evals/optimize/"+url.PathEscape(started.OptimizeID)+"/apply", map[string]string{"candidate": job.Result.Winner, "sink": sink}, nil); err != nil {
					return err
				}
				fmt.Fprintf(out, "Applied candidate %s\n", job.Result.Winner)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the job as JSON")
	cmd.Flags().StringVar(&suite, "suite", "", "Suite id")
	cmd.Flags().StringVar(&target, "target", "", "Find the suite by target, such as skill:<slug>")
	cmd.Flags().StringVar(&slot, "slot", "", "What to rewrite: description, instructions, system, template, body or tool_description:<tool>")
	cmd.Flags().StringVar(&models, "models", "", "Comma-separated provider/model list to evaluate candidates on")
	cmd.Flags().IntVar(&candidates, "candidates", 4, "How many candidates to propose")
	cmd.Flags().IntVar(&repeats, "repeats", 3, "Repeats per case")
	cmd.Flags().Float64Var(&overfit, "overfit", 0.15, "Discard a candidate whose training gain exceeds its validation gain by more than this")
	cmd.Flags().BoolVar(&useTest, "test-split", false, "Score the winner once on the holdout split")
	cmd.Flags().BoolVar(&apply, "apply", false, "Send the winner to its sink")
	cmd.Flags().StringVar(&sink, "sink", "", "Sink for --apply: draft, patch or pr (default: the target's)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Start the job and return")
	return cmd
}

type optimizeJob struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error"`
	Result struct {
		Winner     string `json:"winner"`
		Base       score  `json:"base"`
		Candidates []struct {
			ID      string  `json:"id"`
			Train   float64 `json:"train"`
			Valid   float64 `json:"validation"`
			Test    float64 `json:"test"`
			HasTest bool    `json:"has_test"`
			Overfit bool    `json:"overfit"`
			Leakage bool    `json:"leakage"`
			Diff    string  `json:"diff"`
		} `json:"candidates"`
	} `json:"result"`
}

type score struct {
	Train float64 `json:"train"`
	Valid float64 `json:"validation"`
}

func waitForOptimize(ctx context.Context, c *client.Client, id string) (*optimizeJob, []byte, error) {
	for {
		var doc struct {
			Optimize optimizeJob `json:"optimize"`
		}
		raw, err := gatewayJSON(ctx, c, http.MethodGet, "/api/evals/optimize/"+url.PathEscape(id), nil, &doc)
		if err != nil {
			return nil, nil, err
		}
		if jobTerminal(doc.Optimize.Status) {
			return &doc.Optimize, raw, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(evalPollInterval):
		}
	}
}

func renderOptimize(w io.Writer, j *optimizeJob) {
	fmt.Fprintf(w, "Optimization %s %s. Base: train %.3f, validation %.3f\n", j.ID, j.Status, j.Result.Base.Train, j.Result.Base.Valid)
	for _, c := range j.Result.Candidates {
		flags := ""
		if c.Overfit {
			flags += " overfit"
		}
		if c.Leakage {
			flags += " leakage"
		}
		test := ""
		if c.HasTest {
			test = fmt.Sprintf(", test %.3f", c.Test)
		}
		mark := " "
		if c.ID == j.Result.Winner {
			mark = "*"
		}
		fmt.Fprintf(w, "%s %s  train %.3f, validation %.3f%s%s\n", mark, textsafe.Strict(c.ID), c.Train, c.Valid, test, flags)
	}
	if j.Result.Winner == "" {
		fmt.Fprintln(w, "No candidate beat the base without overfitting.")
	}
}

func evalReviewCmd() *cobra.Command {
	var f approvalFlags
	var suite string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Open the human review queue",
		Long: `Review is keyboard-driven and blind by default (model names are hidden), so it
happens in the browser. This lists the open queues and opens the first one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			path := "/api/evals/review-queues"
			if suite != "" {
				path += "?suite=" + url.QueryEscape(suite)
			}
			var doc struct {
				Queues []struct {
					ID     string `json:"id"`
					Name   string `json:"name"`
					Status string `json:"status"`
					Open   int    `json:"open_items"`
				} `json:"queues"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			var first string
			for _, q := range doc.Queues {
				if q.Status != "open" {
					continue
				}
				if first == "" {
					first = q.ID
				}
				fmt.Fprintf(out, "%s  %s  %d open item(s)\n", q.ID, textsafe.Strict(q.Name), q.Open)
			}
			if first == "" {
				fmt.Fprintln(out, "No review queue is open.")
				return nil
			}
			return openAutomationPage(cmd, f.gatewayURL, "/evals/review/"+url.PathEscape(first), noBrowser, "Review the items there")
		},
	}
	f.bind(cmd, "Print the queues as JSON")
	cmd.Flags().StringVar(&suite, "suite", "", "Only queues of this suite")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the link instead of opening a browser")
	return cmd
}

func evalLeaderboardCmd() *cobra.Command {
	var f approvalFlags
	var suite, target, split, tag string
	cmd := &cobra.Command{
		Use:   "leaderboard",
		Short: "Rank target versions and models on a suite",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if suite == "" {
				if target == "" {
					return errors.New("name the suite with --suite or --target skill:<slug>")
				}
				if suite, err = suiteByTarget(cmd.Context(), c, target); err != nil {
					return err
				}
			}
			q := url.Values{}
			if split != "" {
				q.Set("split", split)
			}
			if tag != "" {
				q.Set("tag", tag)
			}
			path := "/api/evals/leaderboards/" + url.PathEscape(suite)
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var doc struct {
				Entries []struct {
					Key         string   `json:"entry_key"`
					Metric      string   `json:"metric"`
					Value       float64  `json:"value"`
					Low         *float64 `json:"ci_low"`
					High        *float64 `json:"ci_high"`
					Rating      *float64 `json:"rating"`
					N           int      `json:"n"`
					CostPerPass *int64   `json:"cost_per_pass_nano"`
				} `json:"entries"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Entries) == 0 {
				fmt.Fprintln(out, "No entries yet: the board fills after a run.")
				return nil
			}
			for i, e := range doc.Entries {
				ci := ""
				if e.Low != nil && e.High != nil {
					ci = fmt.Sprintf(" (%.3f to %.3f)", *e.Low, *e.High)
				}
				extra := ""
				if e.Rating != nil {
					extra += fmt.Sprintf("  rating %.0f", *e.Rating)
				}
				if e.CostPerPass != nil {
					extra += fmt.Sprintf("  $%.4f per pass", float64(*e.CostPerPass)/1e9)
				}
				fmt.Fprintf(out, "%2d. %s  %s %.3f%s  n=%d%s\n", i+1, textsafe.Strict(e.Key), e.Metric, e.Value, ci, e.N, extra)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the board as JSON")
	cmd.Flags().StringVar(&suite, "suite", "", "Suite id")
	cmd.Flags().StringVar(&target, "target", "", "Find the suite by target, such as skill:<slug>")
	cmd.Flags().StringVar(&split, "split", "", "train, validation or test")
	cmd.Flags().StringVar(&tag, "tag", "", "Only cases with this tag")
	return cmd
}
