package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/patrikmichi/zuano/internal/brand"
)

// ResultsFormat is the version of a results document.
const ResultsFormat = brand.EvalsFormat + ".results/v1"

// Outcomes of one result.
const (
	OutcomePass  = "pass"
	OutcomeFail  = "fail"
	OutcomeError = "error"
)

// Result is one case, repeat and cell.
type Result struct {
	Cell          string         `json:"cell"`
	CaseKey       string         `json:"case_key"`
	CaseName      string         `json:"case_name"`
	Kind          string         `json:"kind"`
	Split         string         `json:"split"`
	Repeat        int            `json:"repeat"`
	Outcome       string         `json:"outcome"`
	Score         float64        `json:"score"`
	Weight        float64        `json:"weight,omitempty"`
	ShouldTrigger *bool          `json:"should_trigger,omitempty"`
	Graders       []GraderResult `json:"graders,omitempty"`
	Output        string         `json:"output,omitempty"`
	Choice        *string        `json:"choice,omitempty"`
	ToolCalls     []ToolCall     `json:"tool_calls,omitempty"`
	InputTokens   int            `json:"input_tokens"`
	OutputTokens  int            `json:"output_tokens"`
	CostNano      int64          `json:"cost_nano,omitempty"`
	LatencyMs     int64          `json:"latency_ms"`
	Error         *CaseError     `json:"error,omitempty"`
}

// CellInfo describes a model of a run.
type CellInfo struct {
	Key      string `json:"key"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Run is a results document.
type Run struct {
	Format         string      `json:"format"`
	ID             string      `json:"id"`
	Suite          string      `json:"suite"`
	Target         string      `json:"target"`
	TargetDigest   string      `json:"target_digest"`
	Subject        string      `json:"subject,omitempty"`
	Started        time.Time   `json:"started"`
	Finished       time.Time   `json:"finished"`
	Seed           int64       `json:"seed"`
	Repeats        int         `json:"repeats"`
	HarnessVersion string      `json:"harness_version"`
	ConfigHash     string      `json:"config_hash"`
	Offline        bool        `json:"offline"`
	Cells          []CellInfo  `json:"cells"`
	Results        []Result    `json:"results"`
	Warnings       []string    `json:"warnings,omitempty"`
	Gate           *GateResult `json:"gate,omitempty"`
}

// ReadRun parses a results document.
func ReadRun(raw []byte) (*Run, error) {
	var r Run
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("not a results document: %w", err)
	}
	if r.Format != ResultsFormat {
		return nil, fmt.Errorf("format %q is not %s", r.Format, ResultsFormat)
	}
	return &r, nil
}

// Options configures a local run.
type Options struct {
	Suite       *Suite
	Config      *Config
	Cases       []Case
	Harness     Harness
	Cells       []Cell
	Judge       *JudgeRunner
	Repeats     int
	Seed        int64
	Concurrency int
	Timeout     time.Duration
	Now         func() time.Time
	// Progress is told after each unit completes.
	Progress func(done, total int)
}

// DefaultSeed is used when a suite sets none.
const DefaultSeed = 1

type unit struct {
	cell   Cell
	c      Case
	repeat int
}

// Execute runs every case of the suite on every cell, repeats times.
func Execute(ctx context.Context, o Options) (*Run, error) {
	if len(o.Cells) == 0 {
		return nil, errors.New("no model to run: list models in the suite or pass --models")
	}
	if o.Repeats < 1 {
		o.Repeats = 1
	}
	if o.Concurrency < 1 {
		o.Concurrency = 4
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Timeout == 0 {
		o.Timeout = 60 * time.Second
	}
	run := &Run{
		Format: ResultsFormat, ID: fmt.Sprintf("run-%s", o.Now().UTC().Format("20060102T150405Z")),
		Suite: o.Suite.ID, Target: o.Suite.Target.Kind, TargetDigest: o.Harness.Digest(), Subject: o.Harness.Subject(),
		Started: o.Now().UTC(), Seed: o.Seed, Repeats: o.Repeats, HarnessVersion: HarnessVersion,
		ConfigHash: o.Config.Hash, Offline: true,
	}
	needsJudge := false
	for _, c := range o.Cases {
		if c.Expected != nil && c.Expected.Rubric != "" {
			needsJudge = true
		}
	}
	if o.Suite.Graders.Judge != nil && o.Suite.Graders.Judge.Rubric != "" {
		needsJudge = true
	}
	if needsJudge && o.Judge == nil {
		return nil, errors.New("cases use a rubric, but no judge model is set: add graders.judge.model to the suite")
	}
	for _, cell := range o.Cells {
		run.Cells = append(run.Cells, CellInfo{Key: cell.Key(), Provider: cell.Model.Provider, Model: cell.Model.Model})
		if o.Judge != nil && o.Judge.Uses(cell.Model) {
			run.Warnings = append(run.Warnings, "the judge and the subject are both "+cell.Model.String()+": a model grades its own answers more kindly")
		}
	}

	var units []unit
	for _, cell := range o.Cells {
		for _, c := range o.Cases {
			for r := 1; r <= o.Repeats; r++ {
				units = append(units, unit{cell, c, r})
			}
		}
	}
	results := make([]Result, len(units))
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	sem := make(chan struct{}, o.Concurrency)
	for i := range units {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = runUnit(ctx, o, units[i])
			if o.Progress != nil {
				mu.Lock()
				done++
				o.Progress(done, len(units))
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.Cell != b.Cell {
			return a.Cell < b.Cell
		}
		if a.CaseKey != b.CaseKey {
			return a.CaseKey < b.CaseKey
		}
		return a.Repeat < b.Repeat
	})
	run.Results = results
	run.Finished = o.Now().UTC()
	return run, nil
}

func runUnit(ctx context.Context, o Options, u unit) Result {
	cctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	out := o.Harness.Run(cctx, u.c, u.cell)
	if out.Error == nil && cctx.Err() != nil {
		out.Error = &CaseError{Kind: ErrTimeout, Message: "the case timed out"}
	}
	r := Result{
		Cell: u.cell.Key(), CaseKey: u.c.ID(), CaseName: u.c.Name, Kind: u.c.Kind, Split: u.c.Split, Repeat: u.repeat,
		Weight: u.c.Weight, ShouldTrigger: u.c.ShouldTrigger, Output: out.Text, Choice: out.Choice, ToolCalls: out.ToolCalls,
		InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens, LatencyMs: out.LatencyMs,
		Error: out.Error,
	}
	if p, ok := o.Config.Prices[u.cell.Model.Model]; ok {
		out.CostNano = int64((float64(out.Usage.InputTokens)*p.Input + float64(out.Usage.OutputTokens)*p.Output) * 1000)
	}
	r.CostNano = out.CostNano
	if out.Error != nil && out.Error.Kind != ErrModel {
		r.Outcome = OutcomeError
		return r
	}
	if out.Error != nil {
		r.Outcome, r.Graders = OutcomeFail, []GraderResult{{Name: "run", Reason: out.Error.Message}}
		return r
	}
	r.Graders = gradeCase(cctx, o, u.c, out)
	var sum float64
	pass := true
	for _, g := range r.Graders {
		sum += g.Score
		pass = pass && g.Pass
	}
	if len(r.Graders) > 0 {
		r.Score = sum / float64(len(r.Graders))
	}
	r.Outcome = OutcomeFail
	if pass {
		r.Outcome = OutcomePass
	}
	return r
}

func gradeCase(ctx context.Context, o Options, c Case, out Output) []GraderResult {
	var gs []GraderResult
	if c.IsTrigger() {
		return append(gs, CheckTrigger(c, o.Harness.Subject(), out))
	}
	var rubric string
	if c.Expected != nil {
		for _, a := range c.Expected.Assertions {
			gs = append(gs, a.Check(out))
		}
		rubric = c.Expected.Rubric
	}
	for _, a := range o.Suite.Graders.Assertions {
		gs = append(gs, a.Check(out))
	}
	if rubric == "" && o.Suite.Graders.Judge != nil {
		rubric = o.Suite.Graders.Judge.Rubric
	}
	if rubric != "" && o.Judge != nil {
		ref := ""
		if c.Expected != nil {
			ref = c.Expected.Reference
		}
		gs = append(gs, o.Judge.Grade(ctx, c.Input.Prompt(), ref, rubric, out.Text))
	}
	return gs
}

// JudgeRunner grades with one or more judge models.
type JudgeRunner struct {
	Providers []Cell
	Aggregate string
	Threshold float64
}

// Uses reports whether the model is one of the judges.
func (j *JudgeRunner) Uses(m ModelRef) bool {
	for _, p := range j.Providers {
		if p.Model == m {
			return true
		}
	}
	return false
}

const judgeSystem = `You grade an answer against a rubric. Be strict and literal. Reply with JSON only: {"score": <number from 0 to 1>, "reason": "<one sentence>"}. 1 means the rubric is fully met, 0 means not at all.`

// ParseJudge reads a judge's reply.
func ParseJudge(text string) (score float64, reason string, ok bool) {
	doc, found := ExtractJSON(text)
	m, isMap := doc.(map[string]any)
	if !found || !isMap {
		return 0, "", false
	}
	s, isNum := m["score"].(float64)
	if !isNum || s < 0 || s > 1 {
		return 0, "", false
	}
	reason, _ = m["reason"].(string)
	return s, truncate(reason, 300), true
}

// Grade asks every judge and combines their scores.
func (j *JudgeRunner) Grade(ctx context.Context, input, reference, rubric, answer string) GraderResult {
	var b strings.Builder
	fmt.Fprintf(&b, "Rubric:\n%s\n\nRequest:\n%s\n\n", rubric, input)
	if reference != "" {
		fmt.Fprintf(&b, "Reference answer:\n%s\n\n", reference)
	}
	fmt.Fprintf(&b, "Answer to grade:\n%s\n", answer)
	var scores []float64
	var reasons []string
	for _, p := range j.Providers {
		resp, err := p.Provider.Complete(ctx, ChatRequest{Model: p.Model.Model, System: judgeSystem, Turns: []Turn{{Role: "user", Text: b.String()}}, MaxTokens: 300})
		if err != nil {
			return GraderResult{Name: "judge", Reason: "the judge failed: " + asCaseError(err).Message}
		}
		s, why, ok := ParseJudge(resp.Text)
		if !ok {
			return GraderResult{Name: "judge", Reason: "the judge's reply was not a score"}
		}
		scores = append(scores, s)
		reasons = append(reasons, why)
	}
	score := 0.0
	switch j.Aggregate {
	case "min":
		score = scores[0]
		for _, s := range scores {
			if s < score {
				score = s
			}
		}
	case "majority":
		var passing, failing []float64
		for _, s := range scores {
			if s >= j.Threshold {
				passing = append(passing, s)
			} else {
				failing = append(failing, s)
			}
		}
		// The side with more judges decides; a tie goes against the answer.
		if len(passing) > len(failing) {
			score = Mean(passing)
		} else {
			score = Mean(failing)
		}
	default:
		score = Mean(scores)
	}
	return GraderResult{Name: "judge", Pass: score >= j.Threshold, Score: score, Reason: strings.Join(reasons, " | ")}
}
