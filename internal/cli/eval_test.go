package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/evals"
)

// compatServer is an OpenAI-compatible model API on localhost; answer gets the
// system prompt and the last user message and returns the model's text.
func compatServer(t *testing.T, answer func(system, user string) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var system, user string
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				system = m.Content
			case "user":
				user = m.Content
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": answer(system, user)}}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 4},
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", srv.URL)
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "")
	return srv
}

const evalSkill = "---\nname: pdf\ndescription: Merge and split PDF files\n---\nUse the merge tool.\n"

// evalProject writes a config, a skill and a dataset into a temp dir.
func evalProject(t *testing.T, gates string) string {
	t.Helper()
	dir := t.TempDir()
	var cases strings.Builder
	cases.WriteString(`{"kind":"trigger","key":"t1","name":"merge","input":"Merge a.pdf and b.pdf","should_trigger":true}` + "\n")
	cases.WriteString(`{"kind":"trigger","key":"t2","name":"weather","input":"Weather in Lisbon?","should_trigger":false}` + "\n")
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&cases, `{"kind":"task","key":"k%02d","name":"task %d","input":"Merge file %d","expected":{"assertions":[{"type":"icontains","value":"merged"}]}}`+"\n", i, i, i)
	}
	files := map[string]string{
		"SKILL.md":            evalSkill,
		"cases.jsonl":         cases.String(),
		brand.EvalsConfigFile: "version: 1\nsuites:\n  - id: pdf\n    target: { kind: skill, path: SKILL.md }\n    dataset: { path: cases.jsonl }\n    models: [openai_compatible/test]\n    repeats: 2\n" + gates,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func goodModel(system, user string) string {
	if strings.Contains(system, "You route requests") {
		if strings.Contains(user, "PDF") || strings.Contains(user, "pdf") {
			return `{"choice":"pdf"}`
		}
		return `{"choice":null}`
	}
	return "All merged."
}

func badModel(system, user string) string {
	if strings.Contains(system, "You route requests") {
		return `{"choice":"docx"}`
	}
	return "Sorry."
}

func TestEvalInitScaffoldsEveryTarget(t *testing.T) {
	for _, target := range []string{"skill", "prompt", "agent", "mcp", "app"} {
		dir := t.TempDir()
		out, err := runCommand(t, EvalCmd(), "init", "--target", target, "--dir", dir)
		if err != nil || !strings.Contains(out, brand.EvalsConfigFile) {
			t.Fatalf("%s: %q %v", target, out, err)
		}
		cfg, err := evals.Load(filepath.Join(dir, brand.EvalsConfigFile))
		if err != nil {
			t.Fatalf("%s: the scaffold does not load: %v", target, err)
		}
		if _, err := evals.LoadDataset(cfg.Resolve(cfg.Suites[0].Dataset.Path), nil); err != nil {
			t.Errorf("%s: the sample dataset is invalid: %v", target, err)
		}
		if target == "app" {
			continue
		}
		// Targets with files of their own are built; app needs a running hook.
		if target != "agent" && target != "skill" {
			if _, err := evals.NewHarness(cfg, &cfg.Suites[0], func(string) string { return "x" }); err != nil {
				t.Errorf("%s: %v", target, err)
			}
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, EvalCmd(), "init", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, EvalCmd(), "init", "--dir", dir); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("a second init overwrote: %v", err)
	}
	if _, err := runCommand(t, EvalCmd(), "init", "--dir", dir, "--force"); err != nil {
		t.Fatal(err)
	}
	gi, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if strings.Count(string(gi), ".zuano/evals/results/") != 1 || !strings.Contains(string(gi), "node_modules\n") {
		t.Errorf(".gitignore = %q", gi)
	}
	if _, err := runCommand(t, EvalCmd(), "init", "--target", "toaster", "--dir", dir); err == nil {
		t.Error("an unknown target")
	}
}

func TestEvalRunOfflineWritesResultsAndPasses(t *testing.T) {
	compatServer(t, goodModel)
	dir := evalProject(t, "    gates: { min_pass_rate: 0.9 }\n")
	out := filepath.Join(dir, "out")
	text, err := runCommand(t, EvalCmd(), "run", "--offline", "--config", filepath.Join(dir, brand.EvalsConfigFile), "--out", out)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, text)
	}
	for _, want := range []string{"Zuano Evals: pdf", "**Passed**", "openai_compatible/test | 24", "Results in " + out} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	for _, f := range []string{"results.json", "report.md", "report.html"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(out, "results.json"))
	run, err := evals.ReadRun(raw)
	if err != nil || len(run.Results) != 24 || run.Gate == nil || !run.Gate.Passed {
		t.Fatalf("results.json: %v %+v", err, run)
	}
	js, err := runCommand(t, EvalCmd(), "run", "--offline", "--json", "--config", filepath.Join(dir, brand.EvalsConfigFile), "--out", out, "--repeats", "1")
	if err != nil || !strings.Contains(js, `"format": "`+evals.ResultsFormat+`"`) {
		t.Errorf("--json: %v %q", err, js[:min(200, len(js))])
	}
}

func TestEvalRunOfflineExitCodes(t *testing.T) {
	dir := evalProject(t, "    gates: { min_pass_rate: 0.9 }\n")
	cfgPath := filepath.Join(dir, brand.EvalsConfigFile)
	compatServer(t, badModel)
	text, err := runCommand(t, EvalCmd(), "run", "--offline", "--config", cfgPath, "--out", filepath.Join(dir, "o1"))
	if exitCodeOf(err) != exitEvalThreshold || !strings.Contains(text, "FAILED (threshold)") {
		t.Errorf("threshold: exit %d (%v)\n%s", exitCodeOf(err), err, text)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer failing.Close()
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", failing.URL)
	_, err = runCommand(t, EvalCmd(), "run", "--offline", "--config", cfgPath, "--out", filepath.Join(dir, "o2"), "--repeats", "1")
	if exitCodeOf(err) != exitRunFailed {
		t.Errorf("a model that rejects every call: exit %d (%v)", exitCodeOf(err), err)
	}

	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "")
	if _, err := runCommand(t, EvalCmd(), "run", "--offline", "--config", cfgPath, "--out", filepath.Join(dir, "o3")); err == nil || !strings.Contains(err.Error(), "OPENAI_COMPATIBLE_BASE_URL") {
		t.Errorf("missing provider setup: %v", err)
	}
	if _, err := runCommand(t, EvalCmd(), "run", "--offline", "--config", filepath.Join(dir, "none.yaml")); err == nil || !strings.Contains(err.Error(), "eval init") {
		t.Errorf("missing config: %v", err)
	}
	if _, err := runCommand(t, EvalCmd(), "run", "--offline", "--config", cfgPath, "--suite", "nope"); err == nil {
		t.Error("an unknown suite")
	}
}

func TestEvalBaselineCompareAndRegression(t *testing.T) {
	dir := evalProject(t, "    gates: { no_regression_vs: main }\n")
	cfgPath := filepath.Join(dir, brand.EvalsConfigFile)
	good, bad := filepath.Join(dir, "good"), filepath.Join(dir, "bad")

	compatServer(t, goodModel)
	// No baseline yet: the run passes and says why it did not check.
	text, err := runCommand(t, EvalCmd(), "run", "--offline", "--config", cfgPath, "--out", good)
	if err != nil || !strings.Contains(text, "No regression check") {
		t.Fatalf("first run: %v\n%s", err, text)
	}
	if out, err := runCommand(t, EvalCmd(), "baseline", "set", "main", "--config", cfgPath, "--run", filepath.Join(good, "results.json")); err != nil || !strings.Contains(out, "baselines/main.json") {
		t.Fatalf("baseline set: %v %q", err, out)
	}
	if out, err := runCommand(t, EvalCmd(), "baseline", "show", "main", "--config", cfgPath); err != nil || !strings.Contains(out, "Baseline main: suite pdf") || !strings.Contains(out, "100.0%") {
		t.Errorf("baseline show: %v %q", err, out)
	}
	if out, err := runCommand(t, EvalCmd(), "baseline", "show", "main", "--config", cfgPath, "--json"); err != nil || !strings.Contains(out, `"suite": "pdf"`) {
		t.Errorf("baseline show --json: %v", err)
	}
	if _, err := runCommand(t, EvalCmd(), "baseline", "show", "nope", "--config", cfgPath); err == nil {
		t.Error("a missing baseline")
	}
	if _, err := runCommand(t, EvalCmd(), "baseline", "set", "../x", "--config", cfgPath, "--run", filepath.Join(good, "results.json")); err == nil {
		t.Error("a path-like ref")
	}
	if _, err := runCommand(t, EvalCmd(), "baseline", "set", "x", "--config", cfgPath, "--run", filepath.Join(dir, "SKILL.md")); err == nil {
		t.Error("a file that is not a results document")
	}

	// Unchanged model: still passes against the baseline.
	if text, err = runCommand(t, EvalCmd(), "run", "--offline", "--config", cfgPath, "--out", filepath.Join(dir, "same")); err != nil || !strings.Contains(text, "vs main") {
		t.Fatalf("same model: %v\n%s", err, text)
	}

	compatServer(t, badModel)
	text, err = runCommand(t, EvalCmd(), "run", "--offline", "--config", cfgPath, "--out", bad)
	if exitCodeOf(err) != exitEvalRegression || !strings.Contains(text, "FAILED (regression)") {
		t.Fatalf("regression: exit %d (%v)\n%s", exitCodeOf(err), err, text)
	}

	goodRun, badRun := filepath.Join(good, "results.json"), filepath.Join(bad, "results.json")
	out, err := runCommand(t, EvalCmd(), "compare", goodRun, badRun)
	if err != nil || !strings.Contains(out, "regression") || !strings.Contains(out, "McNemar p") {
		t.Errorf("compare: %v\n%s", err, out)
	}
	if _, err := runCommand(t, EvalCmd(), "compare", goodRun, badRun, "--fail-on-regression"); exitCodeOf(err) != exitEvalRegression {
		t.Errorf("--fail-on-regression: %v", err)
	}
	if _, err := runCommand(t, EvalCmd(), "compare", badRun, "--against", "baseline:main", "--config", cfgPath, "--fail-on-regression"); exitCodeOf(err) != exitEvalRegression {
		t.Errorf("--against: %v", err)
	}
	if out, err := runCommand(t, EvalCmd(), "compare", goodRun, badRun, "--json"); err != nil || !strings.Contains(out, `"regression": true`) {
		t.Errorf("compare --json: %v", err)
	}
	for name, args := range map[string][]string{
		"one run":       {"compare", goodRun},
		"bad against":   {"compare", goodRun, "--against", "main"},
		"not a results": {"compare", goodRun, filepath.Join(dir, "SKILL.md")},
		"missing":       {"compare", goodRun, filepath.Join(dir, "missing.json")},
		"no baseline":   {"compare", goodRun, "--against", "baseline:ghost", "--config", cfgPath},
		"two + against": {"compare", goodRun, badRun, "--against", "baseline:main", "--config", cfgPath},
	} {
		if _, err := runCommand(t, EvalCmd(), args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEvalComparePairwiseUsesAJudge(t *testing.T) {
	dir := evalProject(t, "")
	cfgPath := filepath.Join(dir, brand.EvalsConfigFile)
	compatServer(t, goodModel)
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if _, err := runCommand(t, EvalCmd(), "run", "--offline", "--config", cfgPath, "--out", a, "--repeats", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, EvalCmd(), "run", "--offline", "--config", cfgPath, "--out", b, "--repeats", "1"); err != nil {
		t.Fatal(err)
	}
	// A judge that always prefers the first position: position bias, so every pair is a tie.
	var calls atomic.Int32
	compatServer(t, func(system, user string) string { calls.Add(1); return `{"winner":"A","reason":"first"}` })
	out, err := runCommand(t, EvalCmd(), "compare", filepath.Join(a, "results.json"), filepath.Join(b, "results.json"), "--pairwise", "--judge", "openai_compatible/judge", "--max-pairs", "3")
	if err != nil || !strings.Contains(out, "Pairwise (3 pairs, both orders): A wins 0, B wins 0, ties 3") || calls.Load() != 6 {
		t.Errorf("pairwise: %v calls %d\n%s", err, calls.Load(), out)
	}
	if _, err := runCommand(t, EvalCmd(), "compare", filepath.Join(a, "results.json"), filepath.Join(b, "results.json"), "--pairwise"); err == nil || !strings.Contains(err.Error(), "--judge") {
		t.Errorf("--pairwise without a judge: %v", err)
	}
	if out, err := runCommand(t, EvalCmd(), "compare", filepath.Join(a, "results.json"), filepath.Join(b, "results.json"), "--pairwise", "--judge", "openai_compatible/judge", "--json", "--max-pairs", "1"); err != nil || !strings.Contains(out, `"pairwise"`) {
		t.Errorf("pairwise --json: %v", err)
	}
}

func TestEvalReportFormats(t *testing.T) {
	compatServer(t, goodModel)
	dir := evalProject(t, "")
	cfgPath := filepath.Join(dir, brand.EvalsConfigFile)
	out := filepath.Join(dir, "out")
	if _, err := runCommand(t, EvalCmd(), "run", "--offline", "--config", cfgPath, "--out", out, "--repeats", "1"); err != nil {
		t.Fatal(err)
	}
	results := filepath.Join(out, "results.json")
	if text, err := runCommand(t, EvalCmd(), "report", results); err != nil || !strings.Contains(text, "### Zuano Evals: pdf") {
		t.Errorf("md: %v %q", err, text)
	}
	if text, err := runCommand(t, EvalCmd(), "report", results, "--format", "html"); err != nil || !strings.Contains(text, "<!doctype html>") {
		t.Errorf("html: %v", err)
	}
	if text, err := runCommand(t, EvalCmd(), "report", results, "--format", "json"); err != nil || !strings.Contains(text, evals.ResultsFormat) {
		t.Errorf("json: %v", err)
	}
	file := filepath.Join(dir, "r.md")
	if _, err := runCommand(t, EvalCmd(), "report", results, "--out", file); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "Zuano Evals") {
		t.Errorf("--out wrote %q", b)
	}
	if _, err := runCommand(t, EvalCmd(), "baseline", "set", "main", "--config", cfgPath, "--run", results); err != nil {
		t.Fatal(err)
	}
	if text, err := runCommand(t, EvalCmd(), "report", results, "--baseline", "main", "--config", cfgPath); err != nil || !strings.Contains(text, "vs main") {
		t.Errorf("--baseline: %v %q", err, text)
	}
	if _, err := runCommand(t, EvalCmd(), "report", results, "--format", "pdf"); err == nil {
		t.Error("an unknown format")
	}
	if _, err := runCommand(t, EvalCmd(), "report", filepath.Join(dir, "none.json")); err == nil {
		t.Error("a missing file")
	}
	if _, err := runCommand(t, EvalCmd(), "report", results, "--baseline", "ghost", "--config", cfgPath); err == nil {
		t.Error("a missing baseline")
	}
}

func TestCIInfoAndJobSummary(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	gh := ciInfo(env(map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "o/r", "GITHUB_SHA": "abc", "GITHUB_REF": "refs/pull/12/merge"}))
	if gh["provider"] != "github" || gh["pr"] != "12" || gh["repo"] != "o/r" {
		t.Errorf("github = %v", gh)
	}
	gl := ciInfo(env(map[string]string{"GITLAB_CI": "true", "CI_PROJECT_PATH": "g/p", "CI_MERGE_REQUEST_IID": "3"}))
	if gl["provider"] != "gitlab" || gl["pr"] != "3" {
		t.Errorf("gitlab = %v", gl)
	}
	if ciInfo(env(nil))["provider"] != "other" {
		t.Error("no CI")
	}

	summary := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)
	t.Setenv("GITHUB_ACTIONS", "true")
	compatServer(t, badModel)
	dir := evalProject(t, "    gates: { min_pass_rate: 0.9 }\n")
	text, err := runCommand(t, EvalCmd(), "run", "--offline", "--ci", "--config", filepath.Join(dir, brand.EvalsConfigFile), "--out", filepath.Join(dir, "o"))
	if exitCodeOf(err) != exitEvalThreshold || !strings.Contains(text, "::error title=Zuano Evals::") {
		t.Errorf("annotation: exit %d\n%s", exitCodeOf(err), text)
	}
	if b, _ := os.ReadFile(summary); !strings.Contains(string(b), "Zuano Evals: pdf") {
		t.Errorf("job summary = %q", b)
	}
}

func TestEvalDatasetsImport(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pf := write("pf.yaml", "tests:\n  - vars: {question: hi}\n    assert:\n      - {type: icontains, value: hello}\n      - {type: javascript, value: x}\n")
	out := filepath.Join(dir, "out", "cases.jsonl")
	text, err := runCommand(t, EvalCmd(), "datasets", "import", pf, "--from", "promptfoo", "--out", out)
	if err != nil || !strings.Contains(text, "Wrote 1 case(s)") || !strings.Contains(text, "javascript: no equivalent assertion") {
		t.Fatalf("promptfoo: %v\n%s", err, text)
	}
	if cs, err := evals.LoadDataset(out, nil); err != nil || len(cs) != 1 {
		t.Errorf("the output is not a dataset: %v", err)
	}
	if text, err := runCommand(t, EvalCmd(), "datasets", "import", pf, "--from", "promptfoo", "--out", out, "--json"); err != nil || !strings.Contains(text, `"unmapped"`) {
		t.Errorf("--json: %v", err)
	}
	csvFile := write("d.csv", "question,answer\nWhat is 2+2?,4\n")
	if text, err := runCommand(t, EvalCmd(), "datasets", "import", csvFile, "--from", "csv", "--column", "input=question", "--column", "contains=answer", "--out", out); err != nil {
		t.Errorf("csv: %v %q", err, text)
	}
	if _, err := runCommand(t, EvalCmd(), "datasets", "import", csvFile, "--from", "csv", "--column", "bad", "--out", out); err == nil {
		t.Error("a malformed --column")
	}
	oa := write("oa.jsonl", `{"input":"q","ideal":"a"}`+"\n")
	crit := write("crit.json", `[{"type":"string_check","operation":"eq","reference":"{{item.ideal}}"}]`)
	if _, err := runCommand(t, EvalCmd(), "datasets", "import", oa, "--from", "openai", "--criteria", crit, "--out", out); err != nil {
		t.Errorf("openai: %v", err)
	}
	if _, err := runCommand(t, EvalCmd(), "datasets", "import", oa, "--from", "openai", "--criteria", filepath.Join(dir, "none"), "--out", out); err == nil {
		t.Error("a missing criteria file")
	}
	sc := write("evals.json", `{"skill_name":"x","evals":[{"id":1,"prompt":"p","expectations":["does x"]}]}`)
	if _, err := runCommand(t, EvalCmd(), "datasets", "import", sc, "--from", "skill-creator", "--out", out); err != nil {
		t.Errorf("skill-creator: %v", err)
	}
	if _, err := runCommand(t, EvalCmd(), "datasets", "import", out, "--from", "jsonl", "--out", filepath.Join(dir, "again.jsonl")); err != nil {
		t.Errorf("jsonl: %v", err)
	}
	plugin := filepath.Join(dir, "plugin-evals", "case-one")
	if err := os.MkdirAll(filepath.Join(plugin, "graders"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("plugin-evals/case-one/prompt.md", "Say hello\n")
	write("plugin-evals/case-one/graders/g.md", "---\ntype: regex\npattern: hello\n---\n")
	if text, err := runCommand(t, EvalCmd(), "datasets", "import", filepath.Join(dir, "plugin-evals"), "--from", "claude-plugin", "--out", out); err != nil || !strings.Contains(text, "Wrote 1 case(s)") {
		t.Errorf("claude-plugin: %v %q", err, text)
	}
	def := filepath.Join(dir, "evals.cases.jsonl")
	if _, err := runCommand(t, EvalCmd(), "datasets", "import", sc, "--from", "skill-creator"); err != nil {
		t.Errorf("default output: %v", err)
	}
	if _, err := os.Stat(def); err != nil {
		t.Errorf("default output file: %v", err)
	}
	for name, args := range map[string][]string{
		"unknown source": {"datasets", "import", pf, "--from", "langsmith"},
		"missing file":   {"datasets", "import", filepath.Join(dir, "none"), "--from", "jsonl"},
		"bad content":    {"datasets", "import", sc, "--from", "promptfoo"},
	} {
		if _, err := runCommand(t, EvalCmd(), args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
