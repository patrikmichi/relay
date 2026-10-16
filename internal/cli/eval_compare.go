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
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/evals"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// evalRoot is the directory baselines live under: the one holding the config
// file, or the current one when there is none.
func evalRoot(configPath string) string {
	if abs, err := filepath.Abs(configPath); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return filepath.Dir(abs)
		}
	}
	return "."
}

func defaultResultsPath() string { return filepath.Join(evalResultsDir, "results.json") }

func printComparisons(w io.Writer, cs []evals.Comparison, label string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "MODEL\tSPLIT\tPAIRS\tCHANGE VS %s (95%% CI)\tW/L/T\tCORRECTED P\tVERDICT\n", strings.ToUpper(label))
	for _, c := range cs {
		verdict := "no difference shown"
		switch {
		case c.Regression:
			verdict = "regression"
		case c.Significant && c.MeanDelta > 0:
			verdict = "improvement"
		case c.Significant:
			verdict = "changed"
		case c.Inconclusive:
			verdict = fmt.Sprintf("inconclusive: about %d cases needed", c.NeedPairs)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%+.3f (%+.3f to %+.3f)\t%d/%d/%d\t%.3f\t%s\n", textsafe.Strict(c.B), c.Split, c.Pairs, c.MeanDelta, c.CILow, c.CIHigh, c.Wins, c.Losses, c.Ties, c.Adjusted, verdict)
	}
	_ = tw.Flush()
}

func evalCompareCmd() *cobra.Command {
	var f approvalFlags
	var against, cfgPath, judgeModel, rubric string
	var failOnRegression, pairwise bool
	var maxPairs int
	cmd := &cobra.Command{
		Use:   "compare <run-a> [<run-b>]",
		Short: "Compare two runs, or a run against a baseline",
		Long: `Compares run B against run A (the baseline), paired by case and repeat: the
mean change with a bootstrap interval, wins, losses and ties, McNemar's exact
test on pass and fail, and p-values corrected across all compared models
(Holm-Bonferroni). A comparison that is not significant and has too few cases
says how many would be needed.

Runs are results files from a local run, or run ids (evr_...) on the gateway.

  zuano eval compare before.json after.json
  zuano eval compare after.json --against baseline:main
  zuano eval compare evr_123 evr_456`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if parseRunID(args[len(args)-1]) {
				return compareCloud(cmd, f, args, against, failOnRegression)
			}
			return compareLocal(cmd, f, args, against, cfgPath, failOnRegression, pairwise, judgeModel, rubric, maxPairs)
		},
	}
	f.bind(cmd, "Print the comparison as JSON")
	evalConfigFlag(cmd, &cfgPath)
	cmd.Flags().StringVar(&against, "against", "", "Compare with a baseline: baseline:<ref>")
	cmd.Flags().BoolVar(&failOnRegression, "fail-on-regression", false, "Exit 21 on a regression")
	cmd.Flags().BoolVar(&pairwise, "pairwise", false, "Also ask a judge which of two stored outputs is better, in both orders")
	cmd.Flags().StringVar(&judgeModel, "judge", "", "Judge model for --pairwise, as provider/model")
	cmd.Flags().StringVar(&rubric, "rubric", "Which answer is more correct, complete and clear?", "What the pairwise judge decides on")
	cmd.Flags().IntVar(&maxPairs, "max-pairs", 50, "Most pairs to judge with --pairwise")
	return cmd
}

func baselineRef(against string) (string, error) {
	ref, ok := strings.CutPrefix(against, "baseline:")
	if !ok || ref == "" {
		return "", fmt.Errorf("--against must be baseline:<ref>, such as baseline:main")
	}
	return ref, nil
}

func compareLocal(cmd *cobra.Command, f approvalFlags, args []string, against, cfgPath string, fail, pairwise bool, judgeModel, rubric string, maxPairs int) error {
	var a, b *evals.Run
	var err error
	label := "run a"
	switch {
	case against != "":
		if len(args) != 1 {
			return errors.New("give one run with --against")
		}
		ref, err := baselineRef(against)
		if err != nil {
			return err
		}
		if a, err = evals.LoadBaseline(evalRoot(cfgPath), ref); err != nil {
			return err
		}
		label = ref
		if b, err = readRunFile(args[0]); err != nil {
			return err
		}
	case len(args) == 2:
		if a, err = readRunFile(args[0]); err != nil {
			return err
		}
		if b, err = readRunFile(args[1]); err != nil {
			return err
		}
	default:
		return errors.New("give two runs, or one run with --against baseline:<ref>")
	}
	pairs, err := sameCells(a, b)
	if err != nil {
		return err
	}
	cs := evals.CompareAll(a, b, pairs, b.Seed)
	out := cmd.OutOrStdout()
	var pw []evals.PairwiseResult
	if pairwise {
		if pw, err = pairwiseJudge(cmd.Context(), a, b, pairs, judgeModel, rubric, maxPairs); err != nil {
			return err
		}
	}
	if f.jsonOut {
		raw, _ := json.MarshalIndent(map[string]any{"comparison": cs, "pairwise": pw}, "", "  ")
		fmt.Fprintln(out, string(raw))
	} else {
		printComparisons(out, cs, label)
		for _, c := range cs {
			if c.Split == evals.SplitAll {
				fmt.Fprintf(out, "%s: McNemar p %.3f (only A passed %d, only B passed %d); %d paired cases\n", textsafe.Strict(c.B), c.McNemarP, c.OnlyA, c.OnlyB, c.Pairs)
			}
		}
		if len(pw) > 0 {
			printPairwise(out, pw)
		}
	}
	if fail {
		for _, c := range cs {
			if c.Regression {
				return &ExitError{Code: exitEvalRegression, Msg: fmt.Sprintf("%s regressed against %s", c.B, label)}
			}
		}
	}
	return nil
}

func pairwiseJudge(ctx context.Context, a, b *evals.Run, cells [][2]string, judgeModel, rubric string, maxPairs int) ([]evals.PairwiseResult, error) {
	if judgeModel == "" {
		return nil, errors.New("--pairwise needs --judge provider/model")
	}
	ref, err := evals.ParseModel(judgeModel)
	if err != nil {
		return nil, err
	}
	p, err := evals.EnvProviders(os.Getenv)(ref.Provider)
	if err != nil {
		return nil, err
	}
	judge := evals.Cell{Model: ref, Provider: p}
	var out []evals.PairwiseResult
	for _, cp := range cells {
		for _, pair := range evals.PairResults(a, cp[0], b, cp[1]) {
			if len(out) >= maxPairs {
				return out, nil
			}
			if pair.A.Output == "" || pair.B.Output == "" {
				continue
			}
			r, err := evals.JudgePair(ctx, judge, rubric, pair.A.CaseName, pair.A.Output, pair.B.Output)
			if err != nil {
				return nil, err
			}
			r.CaseKey, r.Repeat = pair.A.CaseKey, pair.A.Repeat
			out = append(out, r)
		}
	}
	return out, nil
}

func printPairwise(w io.Writer, pw []evals.PairwiseResult) {
	var a, b, tie int
	for _, r := range pw {
		switch r.Final {
		case "a":
			a++
		case "b":
			b++
		default:
			tie++
		}
	}
	fmt.Fprintf(w, "\nPairwise (%d pairs, both orders): A wins %d, B wins %d, ties %d\n", len(pw), a, b, tie)
}

func compareCloud(cmd *cobra.Command, f approvalFlags, args []string, against string, fail bool) error {
	c, err := approvalsClient(f.gatewayURL)
	if err != nil {
		return err
	}
	run := args[len(args)-1]
	baseline := ""
	switch {
	case against != "":
		if baseline, err = baselineRef(against); err != nil {
			return err
		}
	case len(args) == 2:
		baseline = args[0]
	default:
		return errors.New("give two run ids, or one with --against baseline:<ref>")
	}
	var doc struct {
		Comparison []evals.Comparison `json:"comparison"`
	}
	raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/evals/runs/"+url.PathEscape(run)+"/compare?baseline="+url.QueryEscape(baseline), nil, &doc)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printRaw(cmd.OutOrStdout(), raw)
	} else {
		printComparisons(cmd.OutOrStdout(), doc.Comparison, baseline)
	}
	if fail {
		for _, c := range doc.Comparison {
			if c.Regression {
				return &ExitError{Code: exitEvalRegression, Msg: fmt.Sprintf("%s regressed against %s", c.B, baseline)}
			}
		}
	}
	return nil
}

func evalReportCmd() *cobra.Command {
	var f approvalFlags
	var format, baseline, outFile, cfgPath string
	cmd := &cobra.Command{
		Use:   "report [results.json | run-id]",
		Short: "Render results as Markdown, HTML or JSON",
		Long: `Renders a results file (default ` + defaultResultsPath() + `) or a gateway run.
The Markdown is the table posted on a pull request; the HTML is one standalone
file with the failing cases. --baseline adds the comparison with a baseline.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := defaultResultsPath()
			if len(args) == 1 {
				src = args[0]
			}
			if parseRunID(src) {
				return reportCloud(cmd, f, src)
			}
			run, err := readRunFile(src)
			if err != nil {
				return err
			}
			in := evals.ReportInput{Run: run}
			if baseline != "" {
				base, err := evals.LoadBaseline(evalRoot(cfgPath), baseline)
				if err != nil {
					return err
				}
				pairs, err := sameCells(base, run)
				if err != nil {
					return err
				}
				in.BaselineRef, in.Comparisons = baseline, evals.CompareAll(base, run, pairs, run.Seed)
			}
			var body []byte
			switch format {
			case "md":
				body = []byte(evals.Markdown(in))
			case "html":
				if body, err = evals.HTML(in); err != nil {
					return err
				}
			case "json":
				body, _ = json.MarshalIndent(run, "", "  ")
				body = append(body, '\n')
			default:
				return fmt.Errorf("--format must be md, html or json")
			}
			if outFile != "" {
				return os.WriteFile(outFile, body, 0o644)
			}
			_, err = cmd.OutOrStdout().Write(body)
			return err
		},
	}
	f.bind(cmd, "Unused: --format json prints the results")
	_ = cmd.Flags().MarkHidden("json")
	evalConfigFlag(cmd, &cfgPath)
	cmd.Flags().StringVar(&format, "format", "md", "md, html or json")
	cmd.Flags().StringVar(&baseline, "baseline", "", "Add the comparison with this baseline ref")
	cmd.Flags().StringVar(&outFile, "out", "", "Write to a file instead of the terminal")
	return cmd
}

func reportCloud(cmd *cobra.Command, f approvalFlags, id string) error {
	c, err := approvalsClient(f.gatewayURL)
	if err != nil {
		return err
	}
	run, raw, err := waitForEvalRun(cmd.Context(), c, id, 0)
	if err != nil {
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != exitWaitTimeout {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), ee.Msg)
		return nil
	}
	return finishCloudRun(cmd, run, raw, f.jsonOut, false)
}

func evalBaselineCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "baseline", Short: "Record and show the results later runs are compared with"}
	cmd.AddCommand(baselineSetCmd(), baselineShowCmd())
	return cmd
}

func baselineSetCmd() *cobra.Command {
	var f approvalFlags
	var runFile, cfgPath, suite, runID string
	var cloud bool
	cmd := &cobra.Command{
		Use:   "set <ref>",
		Short: "Make a run the baseline for a ref such as main",
		Long: `Locally this copies a results file to .zuano/evals/baselines/<ref>.json, which is
meant to be committed. With --cloud it records a gateway run as the suite's
baseline for the ref; that needs an administrator on protected refs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cloud {
				if suite == "" || runID == "" {
					return errors.New("--cloud needs --suite and --run-id")
				}
				c, err := approvalsClient(f.gatewayURL)
				if err != nil {
					return err
				}
				if _, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPut, "/api/evals/baselines/"+url.PathEscape(suite)+"/"+url.PathEscape(args[0]), map[string]string{"runId": runID}, nil); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Baseline %s of %s is now run %s\n", args[0], suite, runID)
				return nil
			}
			if runFile == "" {
				runFile = defaultResultsPath()
			}
			raw, err := os.ReadFile(runFile)
			if err != nil {
				return err
			}
			if _, err := evals.ReadRun(raw); err != nil {
				return fmt.Errorf("%s: %w", runFile, err)
			}
			p, err := evals.SaveBaseline(evalRoot(cfgPath), args[0], raw)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Baseline %s recorded in %s\n", args[0], p)
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	evalConfigFlag(cmd, &cfgPath)
	cmd.Flags().StringVar(&runFile, "run", "", "Results file (default "+defaultResultsPath()+")")
	cmd.Flags().BoolVar(&cloud, "cloud", false, "Record a gateway run instead of a file")
	cmd.Flags().StringVar(&suite, "suite", "", "Suite id, with --cloud")
	cmd.Flags().StringVar(&runID, "run-id", "", "Gateway run id, with --cloud")
	return cmd
}

func baselineShowCmd() *cobra.Command {
	var f approvalFlags
	var cfgPath, suite string
	var cloud bool
	cmd := &cobra.Command{
		Use:   "show <ref>",
		Short: "Show a baseline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if cloud {
				if suite == "" {
					return errors.New("--cloud needs --suite")
				}
				c, err := approvalsClient(f.gatewayURL)
				if err != nil {
					return err
				}
				var doc struct {
					Baseline struct {
						Ref   string `json:"ref"`
						RunID string `json:"run_id"`
						SetBy string `json:"set_by"`
						SetAt string `json:"set_at"`
					} `json:"baseline"`
				}
				raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/evals/baselines/"+url.PathEscape(suite)+"/"+url.PathEscape(args[0]), nil, &doc)
				if err != nil {
					return err
				}
				if f.jsonOut {
					printRaw(out, raw)
					return nil
				}
				b := doc.Baseline
				fmt.Fprintf(out, "%s of %s: run %s, set by %s at %s\n", textsafe.Strict(b.Ref), suite, textsafe.Strict(b.RunID), textsafe.Strict(b.SetBy), textsafe.Strict(b.SetAt))
				return nil
			}
			run, err := evals.LoadBaseline(evalRoot(cfgPath), args[0])
			if err != nil {
				return err
			}
			if f.jsonOut {
				raw, _ := json.MarshalIndent(run, "", "  ")
				fmt.Fprintln(out, string(raw))
				return nil
			}
			fmt.Fprintf(out, "Baseline %s: suite %s, run %s at %s, seed %d\n", args[0], run.Suite, run.ID, run.Finished.Format("2006-01-02 15:04"), run.Seed)
			printCellTable(out, evals.Summarize(run))
			return nil
		},
	}
	f.bind(cmd, "Print the baseline as JSON")
	evalConfigFlag(cmd, &cfgPath)
	cmd.Flags().BoolVar(&cloud, "cloud", false, "Show the gateway's baseline")
	cmd.Flags().StringVar(&suite, "suite", "", "Suite id, with --cloud")
	return cmd
}
