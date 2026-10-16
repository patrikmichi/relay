package evals

import (
	"context"
	"fmt"
	"time"
)

// ProviderFactory returns the provider with an id; the default reads the
// environment.
type ProviderFactory func(id string) (Provider, error)

// EnvProviders builds providers from the environment, once per id.
func EnvProviders(getenv func(string) string) ProviderFactory {
	cache := map[string]Provider{}
	return func(id string) (Provider, error) {
		if p, ok := cache[id]; ok {
			return p, nil
		}
		p, err := NewProvider(id, getenv)
		if err != nil {
			return nil, err
		}
		cache[id] = p
		return p, nil
	}
}

// RunSpec is a request to run one suite.
type RunSpec struct {
	Config *Config
	Suite  *Suite
	// Models override the suite's models when set.
	Models  []string
	Repeats int
	Getenv  func(string) string
	// Providers overrides the environment-based factory.
	Providers ProviderFactory
	Progress  func(done, total int)
	Now       func() time.Time
}

// RunSuite loads the dataset and target of a suite and runs it locally.
func RunSuite(ctx context.Context, sp RunSpec) (*Run, error) {
	s := sp.Suite
	if s.Dataset.Path == "" {
		return nil, fmt.Errorf("suite %s uses a dataset in the cloud; run it without --offline, or pull it with `eval datasets pull`", s.ID)
	}
	if sp.Getenv == nil {
		sp.Getenv = OSEnv
	}
	if sp.Providers == nil {
		sp.Providers = EnvProviders(sp.Getenv)
	}
	cases, err := LoadDataset(sp.Config.Resolve(s.Dataset.Path), s.Dataset.Columns)
	if err != nil {
		return nil, fmt.Errorf("dataset %s: %w", s.Dataset.Path, err)
	}
	seed := int64(DefaultSeed)
	if s.Seed != nil {
		seed = *s.Seed
	}
	train, test := 70, 0
	if s.Split != nil {
		if s.Split.Train != nil {
			train = *s.Split.Train
		}
		if s.Split.Test != nil {
			test = *s.Split.Test
		}
	}
	if train+test > 100 {
		return nil, fmt.Errorf("suite %s: split train %d + test %d is over 100", s.ID, train, test)
	}
	AssignSplit(cases, train, test, seed)
	h, err := NewHarness(sp.Config, s, sp.Getenv)
	if err != nil {
		return nil, err
	}
	models := sp.Models
	if len(models) == 0 {
		models = s.Models
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("suite %s lists no models: add models: to the suite or pass --models", s.ID)
	}
	var cells []Cell
	for _, m := range models {
		ref, err := ParseModel(m)
		if err != nil {
			return nil, err
		}
		p, err := sp.Providers(ref.Provider)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m, err)
		}
		cells = append(cells, Cell{Model: ref, Provider: p, Temperature: s.Temperature, MaxTokens: s.MaxTokens})
	}
	judge, err := buildJudge(s.Graders.Judge, sp.Providers)
	if err != nil {
		return nil, err
	}
	repeats := sp.Repeats
	if repeats == 0 {
		repeats = s.Repeats
	}
	timeout := time.Duration(s.TimeoutSecs) * time.Second
	return Execute(ctx, Options{
		Suite: s, Config: sp.Config, Cases: cases, Harness: h, Cells: cells, Judge: judge, Repeats: repeats,
		Seed: seed, Concurrency: s.Concurrency, Timeout: timeout, Now: sp.Now, Progress: sp.Progress,
	})
}

func buildJudge(j *Judge, providers ProviderFactory) (*JudgeRunner, error) {
	if j == nil {
		return nil, nil
	}
	models := j.Models
	if j.Model != "" {
		models = append([]string{j.Model}, models...)
	}
	if len(models) == 0 {
		return nil, nil
	}
	jr := &JudgeRunner{Aggregate: j.Aggregate, Threshold: 0.6}
	if j.Threshold != nil {
		jr.Threshold = *j.Threshold
	}
	for _, m := range models {
		ref, err := ParseModel(m)
		if err != nil {
			return nil, err
		}
		p, err := providers(ref.Provider)
		if err != nil {
			return nil, fmt.Errorf("judge %s: %w", m, err)
		}
		jr.Providers = append(jr.Providers, Cell{Model: ref, Provider: p})
	}
	return jr, nil
}

// PairwiseResult is one judged pair of outputs.
type PairwiseResult struct {
	CaseKey string `json:"case_key"`
	Repeat  int    `json:"repeat"`
	Verdict string `json:"verdict"`
	Swapped string `json:"verdict_swapped"`
	Final   string `json:"final"`
	Reason  string `json:"reason,omitempty"`
}

const pairSystem = `You compare two answers to the same request against a rubric. Reply with JSON only: {"winner": "A" | "B" | "tie", "reason": "<one sentence>"}.`

// JudgePair asks which of two outputs is better, in both orders to control for
// position bias; the pair counts as a win only when both orders agree.
func JudgePair(ctx context.Context, judge Cell, rubric, input, a, b string) (PairwiseResult, error) {
	ask := func(first, second string) (string, string, error) {
		prompt := fmt.Sprintf("Rubric:\n%s\n\nRequest:\n%s\n\nAnswer A:\n%s\n\nAnswer B:\n%s\n", rubric, input, first, second)
		resp, err := judge.Provider.Complete(ctx, ChatRequest{Model: judge.Model.Model, System: pairSystem, Turns: []Turn{{Role: "user", Text: prompt}}, MaxTokens: 300})
		if err != nil {
			return "", "", err
		}
		doc, _ := ExtractJSON(resp.Text)
		m, _ := doc.(map[string]any)
		w, _ := m["winner"].(string)
		reason, _ := m["reason"].(string)
		switch w {
		case "A", "B", "tie":
			return w, truncate(reason, 300), nil
		}
		return "", "", fmt.Errorf("the judge's reply named no winner")
	}
	v1, why, err := ask(a, b)
	if err != nil {
		return PairwiseResult{}, err
	}
	v2, _, err := ask(b, a)
	if err != nil {
		return PairwiseResult{}, err
	}
	// The second order swaps the labels back.
	back := map[string]string{"A": "B", "B": "A", "tie": "tie"}[v2]
	res := PairwiseResult{Verdict: v1, Swapped: v2, Final: "tie", Reason: why}
	if v1 == back {
		res.Final = map[string]string{"A": "a", "B": "b", "tie": "tie"}[v1]
	}
	return res, nil
}
