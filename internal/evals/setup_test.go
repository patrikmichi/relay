package evals

import (
	"context"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/brand"
)

func suiteDir(t *testing.T, suiteYAML, cases string) *Config {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "SKILL.md", skillFile)
	writeFile(t, dir, "cases.jsonl", cases)
	writeFile(t, dir, brand.EvalsConfigFile, suiteYAML)
	cfg, err := Load(dir + "/" + brand.EvalsConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const skillCases = `{"kind":"trigger","key":"t1","name":"merge","input":"Merge a.pdf and b.pdf","should_trigger":true}
{"kind":"trigger","key":"t2","name":"unrelated","input":"What's the weather?","should_trigger":false}
{"kind":"task","key":"k1","name":"says merged","input":"Merge them","expected":{"assertions":[{"type":"icontains","value":"merged"}]}}
`

func TestRunSuiteEndToEnd(t *testing.T) {
	cfg := suiteDir(t, `
version: 1
suites:
  - id: pdf
    target: { kind: skill, path: SKILL.md }
    dataset: { path: cases.jsonl }
    models: [anthropic/a, openai/b]
    repeats: 2
    seed: 9
    split: { train: 50, test: 20 }
    gates: { min_pass_rate: 0.5 }
`, skillCases)
	routed := func(r ChatRequest) (ChatResponse, error) {
		if strings.Contains(r.System, "You route requests") {
			if strings.Contains(r.Turns[0].Text, "pdf") {
				return ChatResponse{Text: `{"choice":"pdf"}`}, nil
			}
			return ChatResponse{Text: `{"choice":null}`}, nil
		}
		return ChatResponse{Text: "All merged."}, nil
	}
	fa, fb := &fake{fn: routed}, &fake{fn: routed}
	var progress int
	run, err := RunSuite(context.Background(), RunSpec{
		Config: cfg, Suite: &cfg.Suites[0], Now: fixedNow, Progress: func(done, total int) { progress = total },
		Providers: func(id string) (Provider, error) {
			return map[string]Provider{"anthropic": fa, "openai": fb}[id], nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Results) != 12 || run.Seed != 9 || run.Repeats != 2 || progress != 12 || run.Subject != "pdf" {
		t.Fatalf("run: %d results seed %d repeats %d progress %d", len(run.Results), run.Seed, run.Repeats, progress)
	}
	for _, r := range run.Results {
		if r.Outcome != OutcomePass || r.Split == "" {
			t.Errorf("result %+v", r)
		}
	}
	if g := Gate(run, cfg.Suites[0].Gates, nil, 9); !g.Passed {
		t.Errorf("gate = %+v", g)
	}

	only, err := RunSuite(context.Background(), RunSpec{Config: cfg, Suite: &cfg.Suites[0], Models: []string{"anthropic/c"}, Repeats: 1,
		Providers: func(string) (Provider, error) { return fa, nil }})
	if err != nil || len(only.Cells) != 1 || only.Repeats != 1 {
		t.Errorf("overrides: %+v %v", only, err)
	}
}

func TestRunSuiteReportsSetupProblems(t *testing.T) {
	base := `
version: 1
suites:
  - id: pdf
    target: { kind: skill, path: SKILL.md }
    dataset: { path: cases.jsonl }
%s
`
	prov := func(string) (Provider, error) { return &fake{fn: text("x")}, nil }
	for name, tc := range map[string]struct {
		extra string
		cases string
		want  string
	}{
		"no models":      {"", skillCases, "lists no models"},
		"missing key":    {"    models: [openai/b]", skillCases, "no API key"},
		"bad dataset":    {"    models: [openai/b]", "{", "dataset cases.jsonl"},
		"split over 100": {"    models: [openai/b]\n    split: { train: 80, test: 40 }", skillCases, "over 100"},
		"cloud dataset":  {"    models: [openai/b]", skillCases, "cloud"},
		"bad judge":      {"    models: [openai/b]\n    graders: { judge: { model: google/j } }", skillCases, "judge"},
	} {
		yaml := strings.Replace(base, "%s", tc.extra, 1)
		if name == "cloud dataset" {
			yaml = strings.Replace(yaml, "dataset: { path: cases.jsonl }", "dataset: { cloud: evd_1 }", 1)
		}
		cfg := suiteDir(t, yaml, tc.cases)
		p := ProviderFactory(prov)
		switch name {
		case "missing key":
			p = EnvProviders(env(nil))
		case "bad judge":
			p = func(id string) (Provider, error) {
				if id == "google" {
					return nil, ErrNoKey
				}
				return prov(id)
			}
		}
		_, err := RunSuite(context.Background(), RunSpec{Config: cfg, Suite: &cfg.Suites[0], Providers: p})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestRunSuiteBuildsJudges(t *testing.T) {
	cfg := suiteDir(t, `
version: 1
suites:
  - id: pdf
    target: { kind: prompt, system: "Be helpful" }
    dataset: { path: cases.jsonl }
    models: [anthropic/a]
    graders:
      judge: { model: openai/j1, models: [google/j2], aggregate: majority, threshold: 0.5, rubric: "Be kind" }
`, `{"kind":"task","key":"k","name":"n","input":"hi"}
`)
	// A case without expectations is invalid on its own, but the suite judge's rubric grades it.
	if _, err := RunSuite(context.Background(), RunSpec{Config: cfg, Suite: &cfg.Suites[0], Providers: func(string) (Provider, error) { return &fake{fn: text("x")}, nil }}); err == nil {
		t.Fatal("a case with nothing to grade it was accepted")
	}
	cfg = suiteDir(t, `
version: 1
suites:
  - id: pdf
    target: { kind: prompt, system: "Be helpful" }
    dataset: { path: cases.jsonl }
    models: [anthropic/a]
    graders:
      judge: { model: openai/j1, models: [google/j2], aggregate: majority, threshold: 0.5, rubric: "Be kind" }
`, `{"kind":"task","key":"k","name":"n","input":"hi","expected":{"rubric":"Greets the user"}}
`)
	judges := 0
	run, err := RunSuite(context.Background(), RunSpec{Config: cfg, Suite: &cfg.Suites[0], Providers: func(id string) (Provider, error) {
		if id == "anthropic" {
			return &fake{fn: text("hello")}, nil
		}
		return &fake{fn: func(ChatRequest) (ChatResponse, error) { judges++; return ChatResponse{Text: `{"score":0.9}`}, nil }}, nil
	}})
	if err != nil || judges != 2 || run.Results[0].Outcome != OutcomePass {
		t.Fatalf("run %+v judges %d err %v", run, judges, err)
	}
}
