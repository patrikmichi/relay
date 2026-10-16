package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/evals"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// evalResultsDir is where a local run writes its documents.
const evalResultsDir = ".zuano/evals/results"

// EvalCmd returns `zuano eval`, the evaluation commands. A run is local with
// --offline, using your own provider keys; otherwise it runs on the gateway.
func EvalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Evaluate skills, prompts, subagents, MCP servers and apps",
		Long: brand.EvalsProduct + ` runs a dataset of cases against a target on one or more models and grades
the answers. With --offline it runs on this machine with your own provider keys
(ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY, or an OpenAI-compatible
server at OPENAI_COMPATIBLE_BASE_URL); without it the run happens on the
gateway and the CLI waits for it.

Targets: skill, prompt, agent_config, mcp_server and app run both ways;
agent_endpoint, code graders and coding-agent harnesses run on the gateway only.

Exit status
  0   passed
  20  the run failed: no model completed a case
  21  a regression against the baseline, significant after correction
  23  inconclusive, when the suite sets fail_on_inconclusive
  24  a pass-rate threshold was not met
  13  rate limited
  30  a plan feature or limit
  31  an allowance or spend cap is used up
  17  --timeout ended before the run did`,
	}
	cmd.AddCommand(evalInitCmd(), evalRunCmd(), evalCompareCmd(), evalReportCmd(), evalBaselineCmd(), evalDatasetsCmd(), evalOptimizeCmd(), evalReviewCmd(), evalLeaderboardCmd())
	return cmd
}

func evalConfigFlag(cmd *cobra.Command, path *string) {
	cmd.Flags().StringVar(path, "config", brand.EvalsConfigFile, "Evaluation config file")
}

func loadEvalConfig(path string) (*evals.Config, error) { return evals.Load(path) }

// printCellTable prints one row per model for the whole dataset.
func printCellTable(w io.Writer, cells []evals.CellSummary) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tCASES\tPASS RATE (95% CI)\tMEAN\tERRORS\tCOST")
	for _, s := range cells {
		if s.Split != evals.SplitAll && s.Split != "" {
			continue
		}
		cost := "-"
		if s.CostNano > 0 {
			cost = fmt.Sprintf("$%.4f", float64(s.CostNano)/1e9)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s (%s to %s)\t%.3f\t%d\t%s\n", textsafe.Strict(s.Cell), s.N, evals.Pct(s.PassRate), evals.Pct(s.PassLow), evals.Pct(s.PassHigh), s.Mean, s.Errors, cost)
	}
	_ = tw.Flush()
}

// gateExit turns a gate verdict into the process status, printing why it failed.
func gateExit(w io.Writer, g *evals.GateResult, ci bool) error {
	if g == nil || g.Passed {
		return nil
	}
	for _, f := range g.Failures {
		fmt.Fprintf(w, "FAILED (%s): %s\n", f.Code, textsafe.Strict(f.Message))
		if ci && os.Getenv("GITHUB_ACTIONS") != "" {
			fmt.Fprintf(w, "::error title=%s::%s\n", brand.EvalsProduct, strings.ReplaceAll(textsafe.Strict(f.Message), "\n", " "))
		}
	}
	code := g.ExitCode()
	if code == 0 {
		code = exitRunFailed
	}
	return &ExitError{Code: code, Msg: "the gate failed: " + textsafe.Strict(g.Failures[0].Message)}
}

// gateCodeExit is the status for a failure the gateway reports without one.
var gateCodeExit = map[string]int{
	"run_failed":   exitRunFailed,
	"regression":   exitEvalRegression,
	"threshold":    exitEvalThreshold,
	"inconclusive": exitEvalInconclusive,
}

func normalizeGate(g *evals.GateResult) {
	if g == nil {
		return
	}
	for i := range g.Failures {
		if g.Failures[i].Exit == 0 {
			g.Failures[i].Exit = gateCodeExit[g.Failures[i].Code]
		}
	}
}

func writeJSONFile(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

func readRunFile(path string) (*evals.Run, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r, err := evals.ReadRun(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// sameCells pairs the cells two runs share; with one cell each they pair
// regardless of name.
func sameCells(a, b *evals.Run) ([][2]string, error) {
	if len(a.Cells) == 1 && len(b.Cells) == 1 {
		return [][2]string{{a.Cells[0].Key, b.Cells[0].Key}}, nil
	}
	var out [][2]string
	for _, ca := range a.Cells {
		for _, cb := range b.Cells {
			if ca.Key == cb.Key {
				out = append(out, [2]string{ca.Key, cb.Key})
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the two runs share no model to compare")
	}
	return out, nil
}
