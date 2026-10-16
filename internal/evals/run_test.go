package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedNow() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) }

// stubHarness answers each case by a function of its name.
type stubHarness struct {
	subject string
	fn      func(Case, Cell) Output
}

func (s stubHarness) Subject() string { return s.subject }
func (s stubHarness) Digest() string  { return "digest" }
func (s stubHarness) Run(_ context.Context, c Case, cell Cell) Output {
	return s.fn(c, cell)
}

func taskCases(n int) []Case {
	var cs []Case
	for i := 0; i < n; i++ {
		cs = append(cs, Case{Kind: KindTask, Key: fmt.Sprintf("c%02d", i), Name: fmt.Sprintf("case %d", i), Input: Input{Text: "q"},
			Expected: &Expected{Assertions: []Assertion{{Type: "contains", Value: "ok"}}}})
	}
	AssignSplit(cs, 50, 20, 1)
	return cs
}

func cellsOf(models ...string) []Cell {
	var out []Cell
	for _, m := range models {
		ref, _ := ParseModel(m)
		out = append(out, Cell{Model: ref, Provider: &fake{}})
	}
	return out
}

func execute(t *testing.T, cases []Case, h Harness, cells []Cell, repeats int, judge *JudgeRunner) *Run {
	t.Helper()
	suite := &Suite{ID: "s", Target: Target{Kind: "prompt"}}
	run, err := Execute(context.Background(), Options{Suite: suite, Config: &Config{Hash: "h", Prices: map[string]Price{"m1": {Input: 1000, Output: 2000}}}, Cases: cases, Harness: h, Cells: cells, Judge: judge, Repeats: repeats, Seed: 1, Now: fixedNow, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// passRate makes the harness pass a share of the cases for a model.
func passing(rates map[string]int) stubHarness {
	return stubHarness{fn: func(c Case, cell Cell) Output {
		var idx int
		_, _ = fmt.Sscanf(c.Key, "c%d", &idx)
		text := "no"
		if idx < rates[cell.Model.Model] {
			text = "ok"
		}
		return Output{Text: text, Usage: Usage{InputTokens: 1000, OutputTokens: 500}, LatencyMs: 10}
	}}
}

func TestExecuteGradesAndSummarizes(t *testing.T) {
	run := execute(t, taskCases(20), passing(map[string]int{"m1": 15, "m2": 20}), cellsOf("anthropic/m1", "openai/m2"), 2, nil)
	if len(run.Results) != 80 || len(run.Cells) != 2 || run.Offline != true || run.HarnessVersion != HarnessVersion || run.Format != ResultsFormat {
		t.Fatalf("run = %d results, %+v", len(run.Results), run.Cells)
	}
	for i := 1; i < len(run.Results); i++ {
		a, b := run.Results[i-1], run.Results[i]
		if a.Cell > b.Cell || (a.Cell == b.Cell && a.CaseKey > b.CaseKey) {
			t.Fatal("results are not in a stable order")
		}
	}
	byCell := map[string]CellSummary{}
	for _, s := range Summarize(run) {
		if s.Split == SplitAll {
			byCell[s.Cell] = s
		}
	}
	m1, m2 := byCell["anthropic/m1"], byCell["openai/m2"]
	near(t, "m1 pass rate", m1.PassRate, 0.75)
	near(t, "m2 pass rate", m2.PassRate, 1)
	if m1.N != 40 || m1.PassLow >= 0.75 || m1.PassHigh <= 0.75 || m1.PassAtK != 0.75 || m1.PassPowK != 0.75 || len(m1.Flaky) != 0 {
		t.Errorf("m1 = %+v", m1)
	}
	if m1.CostNano == 0 || m2.CostNano != 0 {
		t.Errorf("only the priced model has a cost: %d %d", m1.CostNano, m2.CostNano)
	}
	var splits int
	for _, s := range Summarize(run) {
		if s.Cell == "openai/m2" && s.Split != SplitAll {
			splits += s.N
		}
	}
	if splits != 40 {
		t.Errorf("splits cover %d results, want 40", splits)
	}
}

func TestErrorsAreNotHeldAgainstTheModel(t *testing.T) {
	h := stubHarness{fn: func(c Case, cell Cell) Output {
		switch c.Key {
		case "c00":
			return Output{Error: &CaseError{Kind: ErrRateLimited, Message: "429"}}
		case "c01":
			return Output{Error: &CaseError{Kind: ErrModel, Message: "loop"}}
		}
		return Output{Text: "ok"}
	}}
	run := execute(t, taskCases(4), h, cellsOf("anthropic/m1"), 1, nil)
	var s CellSummary
	for _, x := range Summarize(run) {
		if x.Split == SplitAll {
			s = x
		}
	}
	if s.Errors != 1 || s.N != 3 || s.PassRate != 2.0/3 {
		t.Errorf("summary = %+v", s)
	}
}

func TestFlakyCasesAreFlagged(t *testing.T) {
	n := 0
	h := stubHarness{fn: func(c Case, cell Cell) Output {
		if c.Key == "c00" {
			n++
			if n%2 == 0 {
				return Output{Text: "ok"}
			}
			return Output{Text: "no"}
		}
		return Output{Text: "ok"}
	}}
	run := execute(t, taskCases(3), h, cellsOf("anthropic/m1"), 4, nil)
	for _, s := range Summarize(run) {
		if s.Split == SplitAll && (len(s.Flaky) != 1 || s.Flaky[0] != "c00") {
			t.Errorf("flaky = %v", s.Flaky)
		}
	}
}

func TestTriggerStats(t *testing.T) {
	yes, no := true, false
	var cs []Case
	for i, sh := range []*bool{&yes, &yes, &yes, &no, &no} {
		cs = append(cs, Case{Kind: KindTrigger, Key: fmt.Sprintf("t%d", i), Name: "t", Input: Input{Text: "x"}, ShouldTrigger: sh})
	}
	chosen := map[string]string{"t0": "pdf", "t1": "pdf", "t2": "", "t3": "pdf", "t4": ""}
	h := stubHarness{subject: "pdf", fn: func(c Case, _ Cell) Output {
		o := Output{}
		if v := chosen[c.Key]; v != "" {
			o.Choice = &v
		}
		return o
	}}
	run := execute(t, cs, h, cellsOf("anthropic/m1"), 1, nil)
	for _, s := range Summarize(run) {
		if s.Split != SplitAll {
			continue
		}
		tr := s.Trigger
		if tr == nil || tr.TP != 2 || tr.FN != 1 || tr.FP != 1 || tr.TN != 1 {
			t.Fatalf("trigger = %+v", tr)
		}
		near(t, "precision", tr.Precision, 2.0/3)
		near(t, "recall", tr.Recall, 2.0/3)
		near(t, "f1", tr.F1, 2.0/3)
	}
}

func TestJudgeGradesRubrics(t *testing.T) {
	jf := &fake{fn: text(`{"score": 0.9, "reason": "meets it"}`)}
	judge := &JudgeRunner{Providers: []Cell{{Model: ModelRef{"openai", "judge"}, Provider: jf}}, Threshold: 0.6}
	cs := []Case{{Kind: KindTask, Key: "r", Name: "rubric", Input: Input{Text: "q"}, Expected: &Expected{Rubric: "be polite", Reference: "thanks"}}}
	h := stubHarness{fn: func(Case, Cell) Output { return Output{Text: "thank you"} }}
	run := execute(t, cs, h, cellsOf("anthropic/m1"), 1, judge)
	r := run.Results[0]
	if r.Outcome != OutcomePass || r.Graders[0].Name != "judge" || !strings.Contains(r.Graders[0].Reason, "meets it") {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(jf.reqs[0].Turns[0].Text, "Reference answer:\nthanks") || len(run.Warnings) != 0 {
		t.Errorf("judge prompt %q warnings %v", jf.reqs[0].Turns[0].Text, run.Warnings)
	}

	same := &JudgeRunner{Providers: []Cell{{Model: ModelRef{"anthropic", "m1"}, Provider: jf}}, Threshold: 0.6}
	if run := execute(t, cs, h, cellsOf("anthropic/m1"), 1, same); len(run.Warnings) != 1 {
		t.Errorf("a judge grading itself: %v", run.Warnings)
	}
	jf.fn = text("no idea")
	if run := execute(t, cs, h, cellsOf("anthropic/m1"), 1, judge); run.Results[0].Outcome != OutcomeFail || !strings.Contains(run.Results[0].Graders[0].Reason, "not a score") {
		t.Errorf("an unreadable judge: %+v", run.Results[0])
	}
	jf.fn = func(ChatRequest) (ChatResponse, error) { return ChatResponse{}, errors.New("down") }
	if run := execute(t, cs, h, cellsOf("anthropic/m1"), 1, judge); run.Results[0].Outcome != OutcomeFail {
		t.Errorf("a failing judge passed")
	}

	if _, err := Execute(context.Background(), Options{Suite: &Suite{}, Config: &Config{}, Cases: cs, Harness: h, Cells: cellsOf("anthropic/m1")}); err == nil || !strings.Contains(err.Error(), "judge") {
		t.Errorf("a rubric without a judge: %v", err)
	}
	if _, err := Execute(context.Background(), Options{Suite: &Suite{}, Config: &Config{}, Cases: cs, Harness: h}); err == nil {
		t.Error("no cells")
	}
}

func TestJudgeAggregation(t *testing.T) {
	scores := []string{`{"score":0.9}`, `{"score":0.2}`, `{"score":0.8}`}
	mk := func(agg string) GraderResult {
		i := 0
		f := &fake{fn: func(ChatRequest) (ChatResponse, error) { i++; return ChatResponse{Text: scores[i-1]}, nil }}
		j := &JudgeRunner{Aggregate: agg, Threshold: 0.6, Providers: []Cell{{Provider: f}, {Provider: f}, {Provider: f}}}
		return j.Grade(context.Background(), "q", "", "r", "a")
	}
	if g := mk("mean"); !g.Pass || g.Score < 0.63 || g.Score > 0.64 {
		t.Errorf("mean = %+v", g)
	}
	if g := mk("min"); g.Pass || g.Score != 0.2 {
		t.Errorf("min = %+v", g)
	}
	if g := mk("majority"); !g.Pass || g.Score < 0.849 || g.Score > 0.851 {
		t.Errorf("majority = %+v", g)
	}
}

func TestCompareAndGates(t *testing.T) {
	cases := taskCases(40)
	base := execute(t, cases, passing(map[string]int{"m1": 36}), cellsOf("anthropic/m1"), 2, nil)
	worse := execute(t, cases, passing(map[string]int{"m1": 24}), cellsOf("anthropic/m1"), 2, nil)
	same := execute(t, cases, passing(map[string]int{"m1": 36}), cellsOf("anthropic/m1"), 2, nil)

	cmps := CompareAll(base, worse, [][2]string{{"anthropic/m1", "anthropic/m1"}}, 1)
	var all Comparison
	for _, c := range cmps {
		if c.Split == SplitAll {
			all = c
		}
	}
	if !all.Regression || !all.Significant || all.Losses != 24 || all.Wins != 0 || all.OnlyA != 24 || all.McNemarP > 0.001 || all.MeanDelta > -0.29 {
		t.Errorf("worse = %+v", all)
	}
	if len(cmps) < 2 {
		t.Errorf("per-split comparisons missing: %d", len(cmps))
	}

	g := Gate(worse, Gates{NoRegressionVs: "main"}, base, 1)
	if g.Passed || g.ExitCode() != ExitRegression || g.Failures[0].Code != "regression" {
		t.Errorf("regression gate = %+v", g)
	}
	if g := Gate(same, Gates{NoRegressionVs: "main"}, base, 1); !g.Passed || g.ExitCode() != 0 {
		t.Errorf("an identical run failed: %+v", g)
	}
	min := 0.95
	if g := Gate(same, Gates{MinPassRate: &min}, nil, 1); g.Passed || g.ExitCode() != ExitThreshold {
		t.Errorf("threshold gate = %+v", g)
	}
	if g := Gate(worse, Gates{MinPassRate: &min, NoRegressionVs: "main"}, base, 1); g.ExitCode() != ExitRegression || len(g.Failures) != 2 {
		t.Errorf("a regression outranks a threshold: %+v", g)
	}

	none := execute(t, cases, stubHarness{fn: func(Case, Cell) Output {
		return Output{Error: &CaseError{Kind: ErrTarget, Message: "down"}}
	}}, cellsOf("anthropic/m1"), 1, nil)
	if g := Gate(none, Gates{}, nil, 1); g.ExitCode() != ExitRunFailed || !strings.Contains(g.Failures[0].Message, "target: down") {
		t.Errorf("a run with no completed case = %+v", g)
	}
	if (*GateResult)(nil).ExitCode() != 0 {
		t.Error("nil gate")
	}
}

func TestInconclusiveWhenTooFewCases(t *testing.T) {
	cases := taskCases(6)
	flip := 0
	noisy := stubHarness{fn: func(c Case, _ Cell) Output {
		flip++
		if flip%3 == 0 {
			return Output{Text: "no"}
		}
		return Output{Text: "ok"}
	}}
	a := execute(t, cases, noisy, cellsOf("anthropic/m1"), 1, nil)
	flip = 1
	b := execute(t, cases, noisy, cellsOf("anthropic/m1"), 1, nil)
	g := Gate(b, Gates{NoRegressionVs: "main", FailOnInconclusive: true}, a, 1)
	if g.Passed || g.ExitCode() != ExitInconclusive || !strings.Contains(g.Failures[0].Message, "needed") {
		t.Errorf("gate = %+v", g)
	}
	if g := Gate(b, Gates{NoRegressionVs: "main"}, a, 1); !g.Passed {
		t.Errorf("inconclusive without fail_on_inconclusive must pass: %+v", g)
	}
}

func TestBaselinesRoundTripAndRejectBadRefs(t *testing.T) {
	root := t.TempDir()
	run := execute(t, taskCases(3), passing(map[string]int{"m1": 3}), cellsOf("anthropic/m1"), 1, nil)
	raw, _ := json.Marshal(run)
	p, err := SaveBaseline(root, "main", raw)
	if err != nil || !strings.HasSuffix(p, ".zuano/evals/baselines/main.json") {
		t.Fatalf("SaveBaseline = %q, %v", p, err)
	}
	got, err := LoadBaseline(root, "main")
	if err != nil || len(got.Results) != 3 {
		t.Fatalf("LoadBaseline = %v, %v", got, err)
	}
	if _, err := LoadBaseline(root, "other"); err == nil || !strings.Contains(err.Error(), "eval baseline set other") {
		t.Errorf("missing baseline: %v", err)
	}
	for _, bad := range []string{"", "../x", "a/b", ".hidden", "a..b", strings.Repeat("a", 80)} {
		if _, err := BaselinePath(root, bad); err == nil {
			t.Errorf("ref %q accepted", bad)
		}
	}
	if _, err := ReadRun([]byte(`{"format":"other"}`)); err == nil {
		t.Error("a foreign document")
	}
	if _, err := ReadRun([]byte(`[`)); err == nil {
		t.Error("broken JSON")
	}
}

func TestReports(t *testing.T) {
	cases := taskCases(10)
	base := execute(t, cases, passing(map[string]int{"m1": 9}), cellsOf("anthropic/m1"), 2, nil)
	run := execute(t, cases, passing(map[string]int{"m1": 5}), cellsOf("anthropic/m1"), 2, nil)
	run.Results[0].Output = "<script>alert(1)</script>\x1b[2J"
	run.Results[0].Outcome = OutcomeFail
	run.Gate = Gate(run, Gates{NoRegressionVs: "main"}, base, 1)
	run.Warnings = []string{"careful | pipe"}
	in := ReportInput{Run: run, BaselineRef: "main", Comparisons: CompareAll(base, run, [][2]string{{"anthropic/m1", "anthropic/m1"}}, 1)}

	md := Markdown(in)
	for _, want := range []string{"### Zuano Evals: s", "**Failed**", "regression", "| anthropic/m1 | 20 |", "vs main", "Warning: careful \\| pipe", "Seed 1, 2 repeat(s)"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	page, err := HTML(in)
	if err != nil {
		t.Fatal(err)
	}
	h := string(page)
	if strings.Contains(h, "<script>") || strings.Contains(h, "\x1b") || !strings.Contains(h, "&lt;script&gt;") || !strings.Contains(h, "regression") || !strings.Contains(h, "Content-Security-Policy") {
		t.Errorf("HTML is unsafe or incomplete:\n%s", h)
	}
	if clean := Markdown(ReportInput{Run: base}); !strings.Contains(clean, "| anthropic/m1 |") || strings.Contains(clean, "vs ") {
		t.Errorf("plain markdown:\n%s", clean)
	}
	if clean, _ := HTML(ReportInput{Run: base}); !bytes.Contains(clean, []byte("Every case passed")) && !bytes.Contains(clean, []byte("Failures and errors")) {
		t.Error("plain HTML")
	}
}

func TestJudgePairControlsForPositionBias(t *testing.T) {
	answer := func(first, second string) func(ChatRequest) (ChatResponse, error) { return nil }
	_ = answer
	mk := func(w1, w2 string) PairwiseResult {
		n := 0
		f := &fake{fn: func(ChatRequest) (ChatResponse, error) {
			n++
			w := w1
			if n == 2 {
				w = w2
			}
			return ChatResponse{Text: fmt.Sprintf(`{"winner":%q,"reason":"r"}`, w)}, nil
		}}
		res, err := JudgePair(context.Background(), Cell{Model: ModelRef{"openai", "j"}, Provider: f}, "better", "q", "answer a", "answer b")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := mk("A", "B"); r.Final != "a" {
		t.Errorf("consistent A win = %+v", r)
	}
	if r := mk("B", "A"); r.Final != "b" {
		t.Errorf("consistent B win = %+v", r)
	}
	if r := mk("A", "A"); r.Final != "tie" {
		t.Errorf("position-biased judge = %+v", r)
	}
	if r := mk("tie", "tie"); r.Final != "tie" {
		t.Errorf("tie = %+v", r)
	}
	bad := &fake{fn: text("no winner")}
	if _, err := JudgePair(context.Background(), Cell{Provider: bad}, "r", "q", "a", "b"); err == nil {
		t.Error("an unreadable verdict")
	}
}

// goldenRun is a fixed run, so the reports it renders can be compared byte for byte.
func goldenRun() ReportInput {
	yes := true
	pdf := "pdf"
	run := &Run{
		Format: ResultsFormat, ID: "run-1", Suite: "pdf", Target: "skill", Subject: "pdf", Seed: 1, Repeats: 2, HarnessVersion: HarnessVersion,
		Started: fixedNow(), Finished: fixedNow(), Cells: []CellInfo{{Key: "anthropic/m", Provider: "anthropic", Model: "m"}},
	}
	for i, outcome := range []string{OutcomePass, OutcomePass, OutcomeFail, OutcomePass, OutcomeError, OutcomeError} {
		r := Result{Cell: "anthropic/m", CaseKey: fmt.Sprintf("c%d", i/2), CaseName: fmt.Sprintf("case %d", i/2), Kind: KindTrigger, Split: SplitValidation,
			Repeat: i%2 + 1, Outcome: outcome, ShouldTrigger: &yes, InputTokens: 100, OutputTokens: 20, CostNano: 1500000, LatencyMs: 40}
		if outcome != OutcomeError {
			r.Choice, r.Score = &pdf, 1
		}
		switch outcome {
		case OutcomeFail:
			r.Score, r.Output = 0, "I would use <b>docx</b> instead."
			r.Graders = []GraderResult{{Name: "trigger", Reason: `chose "docx"; should trigger: true`}}
		case OutcomeError:
			r.Error = &CaseError{Kind: ErrRateLimited, Message: "429"}
		}
		run.Results = append(run.Results, r)
	}
	run.Gate = &GateResult{Passed: false, Failures: []GateFailure{{Code: "threshold", Exit: ExitThreshold, Message: "anthropic/m passes 75.0% of cases, below the required 90.0%"}}}
	return ReportInput{Run: run}
}

func TestReportsMatchTheirGoldenFiles(t *testing.T) {
	in := goldenRun()
	page, err := HTML(in)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"report.md.golden": Markdown(in), "report.html.golden": string(page)} {
		path := filepath.Join("testdata", "golden", name)
		if os.Getenv("UPDATE_GOLDEN") != "" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(want) != got {
			t.Errorf("%s differs from the rendered report; rerun with UPDATE_GOLDEN=1 if the change is intended\n--- got ---\n%s", name, got)
		}
	}
}
