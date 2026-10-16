package evals

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/patrikmichi/zuano/internal/brand"
)

// SplitAll is the summary over every split.
const SplitAll = "all"

// FlakyThreshold is the share of disagreeing repeats above which a case is flagged.
const FlakyThreshold = 0.3

// WinLossEpsilon is the score difference below which two results tie.
const WinLossEpsilon = 0.01

// TriggerStats are the precision, recall and F1 of trigger cases.
type TriggerStats struct {
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
	TP        int     `json:"tp"`
	FP        int     `json:"fp"`
	FN        int     `json:"fn"`
	TN        int     `json:"tn"`
}

// CellSummary condenses the results of one cell and split.
type CellSummary struct {
	Cell         string        `json:"cell"`
	Split        string        `json:"split"`
	N            int           `json:"n"`
	Errors       int           `json:"errors"`
	Mean         float64       `json:"mean"`
	StdDev       float64       `json:"stddev"`
	PassRate     float64       `json:"pass_rate"`
	PassLow      float64       `json:"pass_low"`
	PassHigh     float64       `json:"pass_high"`
	PassAtK      float64       `json:"pass_at_k"`
	PassPowK     float64       `json:"pass_pow_k"`
	Flaky        []string      `json:"flaky,omitempty"`
	Trigger      *TriggerStats `json:"trigger,omitempty"`
	InputTokens  int           `json:"input_tokens"`
	OutputTokens int           `json:"output_tokens"`
	CostNano     int64         `json:"cost_nano,omitempty"`
	MeanLatency  float64       `json:"mean_latency_ms"`
}

// Summarize summarizes every cell and split of a run; errors are counted but
// held neither for nor against the model.
func Summarize(r *Run) []CellSummary {
	type key struct{ cell, split string }
	group := map[key][]Result{}
	for _, res := range r.Results {
		group[key{res.Cell, SplitAll}] = append(group[key{res.Cell, SplitAll}], res)
		if res.Split != "" {
			group[key{res.Cell, res.Split}] = append(group[key{res.Cell, res.Split}], res)
		}
	}
	var out []CellSummary
	for k, rs := range group {
		out = append(out, summarize(k.cell, k.split, rs, r.Repeats, r.Subject))
	}
	order := map[string]int{SplitAll: 0, SplitTrain: 1, SplitValidation: 2, SplitTest: 3}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cell != out[j].Cell {
			return out[i].Cell < out[j].Cell
		}
		return order[out[i].Split] < order[out[j].Split]
	})
	return out
}

func summarize(cell, split string, rs []Result, repeats int, subject string) CellSummary {
	s := CellSummary{Cell: cell, Split: split}
	byCase := map[string][2]int{} // passes, runs
	var scores, lat []float64
	var weightSum, weightedSum float64
	var tr TriggerStats
	triggers := 0
	for _, r := range rs {
		s.InputTokens += r.InputTokens
		s.OutputTokens += r.OutputTokens
		s.CostNano += r.CostNano
		if r.Outcome == OutcomeError {
			s.Errors++
			continue
		}
		s.N++
		scores = append(scores, r.Score)
		w := r.Weight
		if w <= 0 {
			w = 1
		}
		weightSum += w
		weightedSum += w * r.Score
		lat = append(lat, float64(r.LatencyMs))
		c := byCase[r.CaseKey]
		c[1]++
		if r.Outcome == OutcomePass {
			c[0]++
		}
		byCase[r.CaseKey] = c
		if r.Kind == KindTrigger && r.ShouldTrigger != nil {
			triggers++
			chose := r.Choice != nil && *r.Choice == subject && subject != ""
			switch {
			case *r.ShouldTrigger && chose:
				tr.TP++
			case *r.ShouldTrigger:
				tr.FN++
			case chose:
				tr.FP++
			default:
				tr.TN++
			}
		}
	}
	s.StdDev, s.MeanLatency = StdDev(scores), Mean(lat)
	if weightSum > 0 {
		s.Mean = weightedSum / weightSum
	}
	passes := 0
	for key, c := range byCase {
		passes += c[0]
		if Flakiness(c[0], c[1]) > FlakyThreshold {
			s.Flaky = append(s.Flaky, key)
		}
		k := repeats
		if k > c[1] {
			k = c[1]
		}
		s.PassAtK += PassAtK(c[1], c[0], k)
		s.PassPowK += PassPowK(c[1], c[0], k)
	}
	sort.Strings(s.Flaky)
	if n := len(byCase); n > 0 {
		s.PassAtK /= float64(n)
		s.PassPowK /= float64(n)
	}
	if s.N > 0 {
		s.PassRate = float64(passes) / float64(s.N)
	}
	s.PassLow, s.PassHigh = Wilson(passes, s.N)
	if triggers > 0 {
		if tr.TP+tr.FP > 0 {
			tr.Precision = float64(tr.TP) / float64(tr.TP+tr.FP)
		}
		if tr.TP+tr.FN > 0 {
			tr.Recall = float64(tr.TP) / float64(tr.TP+tr.FN)
		}
		if tr.Precision+tr.Recall > 0 {
			tr.F1 = 2 * tr.Precision * tr.Recall / (tr.Precision + tr.Recall)
		}
		s.Trigger = &tr
	}
	return s
}

// Pair is a result of A and of B for the same case and repeat.
type Pair struct{ A, B Result }

// PairResults pairs the results of cell a in run ra with those of cell b in
// run rb by case and repeat, leaving out pairs where either side errored.
func PairResults(ra *Run, a string, rb *Run, b string) []Pair {
	type key struct {
		c string
		r int
	}
	idx := map[key]Result{}
	for _, r := range ra.Results {
		if r.Cell == a && r.Outcome != OutcomeError {
			idx[key{r.CaseKey, r.Repeat}] = r
		}
	}
	var out []Pair
	for _, r := range rb.Results {
		if r.Cell != b || r.Outcome == OutcomeError {
			continue
		}
		if x, ok := idx[key{r.CaseKey, r.Repeat}]; ok {
			out = append(out, Pair{x, r})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].A.CaseKey != out[j].A.CaseKey {
			return out[i].A.CaseKey < out[j].A.CaseKey
		}
		return out[i].A.Repeat < out[j].A.Repeat
	})
	return out
}

// Comparison is B measured against A, A being the baseline.
type Comparison struct {
	A         string  `json:"a"`
	B         string  `json:"b"`
	Split     string  `json:"split"`
	Pairs     int     `json:"pairs"`
	MeanDelta float64 `json:"mean_delta"`
	CILow     float64 `json:"ci_low"`
	CIHigh    float64 `json:"ci_high"`
	PValue    float64 `json:"p_value"`
	// Adjusted is PValue after the Holm correction over all compared pairs of cells.
	Adjusted     float64 `json:"adjusted_p"`
	Wins         int     `json:"wins"`
	Losses       int     `json:"losses"`
	Ties         int     `json:"ties"`
	McNemarP     float64 `json:"mcnemar_p"`
	OnlyA        int     `json:"only_a"`
	OnlyB        int     `json:"only_b"`
	Significant  bool    `json:"significant"`
	Regression   bool    `json:"regression"`
	Inconclusive bool    `json:"inconclusive"`
	// NeedPairs is how many paired cases would detect a 5-point change.
	NeedPairs int `json:"need_pairs"`
}

// Compare measures B against A on the pairs of one split.
func Compare(a, b, split string, pairs []Pair, seed int64) Comparison {
	c := Comparison{A: a, B: b, Split: split}
	var deltas []float64
	for _, p := range pairs {
		if split != SplitAll && p.A.Split != split {
			continue
		}
		d := p.B.Score - p.A.Score
		deltas = append(deltas, d)
		switch {
		case d > WinLossEpsilon:
			c.Wins++
		case d < -WinLossEpsilon:
			c.Losses++
		default:
			c.Ties++
		}
		switch ap, bp := p.A.Outcome == OutcomePass, p.B.Outcome == OutcomePass; {
		case ap && !bp:
			c.OnlyA++
		case bp && !ap:
			c.OnlyB++
		}
	}
	c.Pairs = len(deltas)
	bs := Bootstrap(deltas, 2000, uint32(seed))
	c.MeanDelta, c.CILow, c.CIHigh, c.PValue, c.Adjusted = bs.MeanDelta, bs.CILow, bs.CIHigh, bs.PValue, bs.PValue
	c.McNemarP = McNemarExact(c.OnlyA, c.OnlyB)
	c.NeedPairs = PowerHint(StdDev(deltas))
	return c
}

// CompareAll compares every (a, b) with the Holm correction over the family
// of whole-run comparisons, then classifies each one.
func CompareAll(ra *Run, rb *Run, cellPairs [][2]string, seed int64) []Comparison {
	var all []Comparison
	var ps []float64
	for _, cp := range cellPairs {
		pairs := PairResults(ra, cp[0], rb, cp[1])
		for _, split := range []string{SplitAll, SplitTrain, SplitValidation, SplitTest} {
			c := Compare(cp[0], cp[1], split, pairs, seed)
			if split != SplitAll && c.Pairs == 0 {
				continue
			}
			all = append(all, c)
			if split == SplitAll {
				ps = append(ps, c.PValue)
			}
		}
	}
	adj := Holm(ps)
	i := 0
	for k := range all {
		if all[k].Split != SplitAll {
			continue
		}
		all[k].Adjusted = adj[i]
		i++
		c := &all[k]
		c.Significant = c.Adjusted < 0.05
		c.Regression = c.Significant && c.MeanDelta < -WinLossEpsilon
		c.Inconclusive = !c.Significant && c.Pairs < c.NeedPairs
	}
	return all
}

// GateFailure is one reason a run did not pass.
type GateFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Exit    int    `json:"exit"`
}

// GateResult is the verdict on a run.
type GateResult struct {
	Passed   bool          `json:"passed"`
	Failures []GateFailure `json:"failures,omitempty"`
}

// Exit statuses of a gate, as in the exit-code registry.
const (
	ExitRunFailed    = 20
	ExitRegression   = 21
	ExitInconclusive = 23
	ExitThreshold    = 24
)

// ExitCode is the status a failing gate ends the process with: a failed run
// outranks a regression, which outranks a missed threshold, which outranks an
// inconclusive comparison.
func (g *GateResult) ExitCode() int {
	if g == nil || g.Passed {
		return 0
	}
	rank := map[int]int{ExitRunFailed: 4, ExitRegression: 3, ExitThreshold: 2, ExitInconclusive: 1}
	best := 0
	for _, f := range g.Failures {
		if rank[f.Exit] > rank[best] {
			best = f.Exit
		}
	}
	return best
}

// Gate evaluates a run against a suite's gates and, when it names one, a baseline.
func Gate(run *Run, gates Gates, baseline *Run, seed int64) *GateResult {
	g := &GateResult{}
	fail := func(code string, exit int, format string, a ...any) {
		g.Failures = append(g.Failures, GateFailure{Code: code, Message: fmt.Sprintf(format, a...), Exit: exit})
	}
	sums := map[string]CellSummary{}
	completed := 0
	for _, s := range Summarize(run) {
		if s.Split == SplitAll {
			sums[s.Cell] = s
			if s.N > 0 {
				completed++
			}
		}
	}
	if completed == 0 {
		why := ""
		for _, r := range run.Results {
			if r.Error != nil {
				why = ": " + r.Error.Error()
				break
			}
		}
		fail("run_failed", ExitRunFailed, "no model completed a case%s", why)
		return g
	}
	if gates.MinPassRate != nil {
		for _, c := range run.Cells {
			if s := sums[c.Key]; s.N > 0 && s.PassRate < *gates.MinPassRate {
				fail("threshold", ExitThreshold, "%s passes %.1f%% of cases, below the required %.1f%%", c.Key, s.PassRate*100, *gates.MinPassRate*100)
			}
		}
	}
	if baseline != nil {
		var cellPairs [][2]string
		for _, c := range run.Cells {
			if base := matchBaselineCell(baseline, c.Key); base != "" {
				cellPairs = append(cellPairs, [2]string{base, c.Key})
			}
		}
		if len(cellPairs) == 0 {
			fail("run_failed", ExitRunFailed, "the baseline has no cell to compare with")
		}
		for _, c := range CompareAll(baseline, run, cellPairs, seed) {
			if c.Split != SplitAll {
				continue
			}
			switch {
			case c.Regression:
				fail("regression", ExitRegression, "%s dropped by %.3f against the baseline (corrected p %.3f)", c.B, -c.MeanDelta, c.Adjusted)
			case c.Inconclusive && gates.FailOnInconclusive:
				fail("inconclusive", ExitInconclusive, "%s against the baseline is inconclusive: %d paired cases, about %d needed", c.B, c.Pairs, c.NeedPairs)
			}
		}
	}
	g.Passed = len(g.Failures) == 0
	return g
}

func matchBaselineCell(base *Run, key string) string {
	for _, c := range base.Cells {
		if c.Key == key {
			return key
		}
	}
	if len(base.Cells) == 1 {
		return base.Cells[0].Key
	}
	return ""
}

var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// BaselineDir is where local baselines live, under the directory the commands run from.
const BaselineDir = ".zuano/evals/baselines"

// BaselinePath is the file of a baseline ref.
func BaselinePath(root, ref string) (string, error) {
	if !refPattern.MatchString(ref) || strings.Contains(ref, "..") {
		return "", fmt.Errorf("%q is not a baseline name: use letters, digits, dot, dash and underscore", ref)
	}
	return filepath.Join(root, BaselineDir, ref+".json"), nil
}

// SaveBaseline records a run as the baseline for ref.
func SaveBaseline(root, ref string, raw []byte) (string, error) {
	p, err := BaselinePath(root, ref)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, raw, 0o644)
}

// LoadBaseline reads the baseline for ref.
func LoadBaseline(root, ref string) (*Run, error) {
	p, err := BaselinePath(root, ref)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no baseline %q: record one with `%s`", ref, brand.Cmd("eval baseline set "+ref))
		}
		return nil, err
	}
	return ReadRun(raw)
}

// Pct formats a share as a percentage.
func Pct(v float64) string { return fmt.Sprintf("%.1f%%", math.Round(v*1000)/10) }
