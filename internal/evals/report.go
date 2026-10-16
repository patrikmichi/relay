package evals

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"strings"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

//go:embed report.html.tmpl
var reportTemplate string

// ReportInput is what a report shows.
type ReportInput struct {
	Run         *Run
	BaselineRef string
	Comparisons []Comparison
}

func cost(nano int64) string {
	if nano == 0 {
		return "-"
	}
	return fmt.Sprintf("$%.4f", float64(nano)/1e9)
}

func cleanCell(s string) string {
	s = strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ").Replace(textsafe.Strict(s))
	return s
}

// Markdown renders the run as the table posted on a pull request.
func Markdown(in ReportInput) string {
	var b strings.Builder
	r := in.Run
	fmt.Fprintf(&b, "### %s: %s\n\n", brand.EvalsProduct, cleanCell(r.Suite))
	if r.Gate != nil {
		if r.Gate.Passed {
			b.WriteString("**Passed**\n\n")
		} else {
			b.WriteString("**Failed**\n\n")
			for _, f := range r.Gate.Failures {
				fmt.Fprintf(&b, "- %s (exit %d)\n", cleanCell(f.Message), f.Exit)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("| Model | Cases | Pass rate (95% CI) | Mean score | Errors | Cost |")
	if len(in.Comparisons) > 0 {
		b.WriteString(" vs " + cleanCell(in.BaselineRef) + " | Significant |")
	}
	b.WriteString("\n|---|---|---|---|---|---|")
	if len(in.Comparisons) > 0 {
		b.WriteString("---|---|")
	}
	b.WriteString("\n")
	cmp := map[string]Comparison{}
	for _, c := range in.Comparisons {
		if c.Split == SplitAll {
			cmp[c.B] = c
		}
	}
	sums := Summarize(r)
	for _, s := range sums {
		if s.Split != SplitAll {
			continue
		}
		fmt.Fprintf(&b, "| %s | %d | %s (%s to %s) | %.3f | %d | %s |", cleanCell(s.Cell), s.N, Pct(s.PassRate), Pct(s.PassLow), Pct(s.PassHigh), s.Mean, s.Errors, cost(s.CostNano))
		if len(in.Comparisons) > 0 {
			if c, ok := cmp[s.Cell]; ok {
				verdict := "no"
				switch {
				case c.Regression:
					verdict = "regression"
				case c.Significant:
					verdict = "yes"
				case c.Inconclusive:
					verdict = fmt.Sprintf("inconclusive: about %d cases needed", c.NeedPairs)
				}
				fmt.Fprintf(&b, " %+.3f (%+.3f to %+.3f) | %s |", c.MeanDelta, c.CILow, c.CIHigh, verdict)
			} else {
				b.WriteString(" - | - |")
			}
		}
		b.WriteString("\n")
	}
	for _, s := range sums {
		if s.Trigger != nil && s.Split == SplitAll {
			fmt.Fprintf(&b, "\n%s trigger: precision %.2f, recall %.2f, F1 %.2f\n", cleanCell(s.Cell), s.Trigger.Precision, s.Trigger.Recall, s.Trigger.F1)
		}
		if len(s.Flaky) > 0 && s.Split == SplitAll {
			fmt.Fprintf(&b, "\n%s flaky cases (over %.0f%% of repeats disagree): %s\n", cleanCell(s.Cell), FlakyThreshold*100, cleanCell(strings.Join(s.Flaky, ", ")))
		}
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "\nWarning: %s\n", cleanCell(w))
	}
	fmt.Fprintf(&b, "\nSeed %d, %d repeat(s), harness %s, run locally.\n", r.Seed, r.Repeats, r.HarnessVersion)
	return b.String()
}

type htmlResult struct {
	Result
	Output string
	Reason string
}

// HTML renders a standalone report. Model output is untrusted text and is
// escaped.
func HTML(in ReportInput) ([]byte, error) {
	tmpl, err := template.New("report").Funcs(template.FuncMap{"pct": Pct, "cost": cost}).Parse(reportTemplate)
	if err != nil {
		return nil, err
	}
	var failing []htmlResult
	for _, res := range in.Run.Results {
		if res.Outcome == OutcomePass {
			continue
		}
		var reasons []string
		for _, g := range res.Graders {
			if !g.Pass && g.Reason != "" {
				reasons = append(reasons, g.Name+": "+g.Reason)
			}
		}
		if res.Error != nil {
			reasons = append(reasons, res.Error.Error())
		}
		failing = append(failing, htmlResult{Result: res, Output: truncate(textsafe.Strict(res.Output), 600), Reason: textsafe.Strict(strings.Join(reasons, "; "))})
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, map[string]any{
		"Product": brand.EvalsProduct, "Run": in.Run, "Summaries": Summarize(in.Run), "Failing": failing,
		"Comparisons": in.Comparisons, "Baseline": in.BaselineRef,
	})
	return buf.Bytes(), err
}
