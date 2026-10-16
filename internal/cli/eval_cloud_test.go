package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/evals"
)

func fastPolling(t *testing.T) {
	t.Helper()
	old := evalPollInterval
	evalPollInterval = time.Millisecond
	t.Cleanup(func() { evalPollInterval = old })
}

const cloudCells = `[{"cell":"anthropic/m","split":"all","n":40,"errors":1,"mean":0.82,"pass_rate":0.8,"pass_low":0.66,"pass_high":0.9,"cost_nano":120000000}]`

func runDoc(status, gate string) string {
	return `{"run":{"id":"evr_1","status":"` + status + `","url":"https://gw.example/evals/runs/evr_1","summary":{"cells":` + cloudCells + `},"totals":{"units":40,"cached":4},"gate_result":` + gate + `}}`
}

func TestEvalRunCloudWaitsAndPrintsTheSummary(t *testing.T) {
	fastPolling(t)
	var polls atomic.Int32
	gw := a2aGateway(t, routeFns{
		"POST /api/evals/runs": jsonReply(202, `{"runId":"evr_1","estimate":{"units":40,"cost_nano":1000}}`),
		"GET /api/evals/runs/evr_1": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			status := "running"
			if polls.Add(1) >= 3 {
				status = "succeeded"
			}
			jsonReply(200, runDoc(status, `{"passed":true}`))(nil, w, r)
		},
	})
	out, err := runCommand(t, EvalCmd(), "run", "--suite", "pr-triage", "--models", "anthropic/m, openai/n", "--repeats", "3", "--no-cache")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	for _, want := range []string{"Started run evr_1 (40 case runs reserved)", "Run evr_1 succeeded", "80.0% (66.0% to 90.0%)", "$0.1200", "40 case runs (4 cached)", "https://gw.example/evals/runs/evr_1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	body := gw.body("POST /api/evals/runs", 0)
	cells, _ := body["cells"].([]any)
	if body["suiteId"] != "pr-triage" || body["repeats"] != 3.0 || body["cache"] != false || body["trigger"] != "manual" || len(cells) != 2 || polls.Load() != 3 {
		t.Errorf("body = %v polls %d", body, polls.Load())
	}
	if out, err := runCommand(t, EvalCmd(), "run", "--suite", "s", "--json"); err != nil || !strings.Contains(out, `"gate_result"`) {
		t.Errorf("--json: %v %q", err, out)
	}
}

func TestEvalRunCloudGateExitCodes(t *testing.T) {
	fastPolling(t)
	gate := `{"passed":true}`
	status := "succeeded"
	a2aGateway(t, routeFns{
		"POST /api/evals/runs": jsonReply(202, `{"runId":"evr_1","estimate":{"units":1}}`),
		"GET /api/evals/runs/evr_1": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			jsonReply(200, runDoc(status, gate))(nil, w, r)
		},
	})
	for _, tc := range []struct {
		status, gate string
		want         int
	}{
		{"succeeded", `{"passed":true}`, 0},
		{"succeeded", `{"passed":false,"failures":[{"code":"regression","message":"m dropped"}]}`, exitEvalRegression},
		{"succeeded", `{"passed":false,"failures":[{"code":"threshold","message":"below"}]}`, exitEvalThreshold},
		{"succeeded", `{"passed":false,"failures":[{"code":"inconclusive","message":"unclear"}]}`, exitEvalInconclusive},
		{"succeeded", `{"passed":false,"failures":[{"code":"threshold","message":"t"},{"code":"regression","message":"r"}]}`, exitEvalRegression},
		{"succeeded", `{"passed":false,"failures":[{"code":"other","message":"x"}]}`, exitRunFailed},
		{"failed", `null`, exitRunFailed},
		{"cancelled", `null`, exitRunFailed},
	} {
		status, gate = tc.status, tc.gate
		out, err := runCommand(t, EvalCmd(), "run", "--suite", "s")
		if exitCodeOf(err) != tc.want {
			t.Errorf("%s %s: exit %d (%v)\n%s", tc.status, tc.gate, exitCodeOf(err), err, out)
		}
	}
}

func TestEvalRunCloudRefusalsAndLimits(t *testing.T) {
	gw := a2aGateway(t, routeFns{"POST /api/evals/runs": jsonReply(402, `{"error":"plan_required","feature":"evals.matrix_models","message":"This plan runs two models."}`)})
	if _, err := runCommand(t, EvalCmd(), "run", "--suite", "s"); exitCodeOf(err) != client.ExitPlanRequired {
		t.Errorf("plan_required: exit %d (%v)", exitCodeOf(err), err)
	}
	gw.replace(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
	})
	if _, err := runCommand(t, EvalCmd(), "run", "--suite", "s"); exitCodeOf(err) != client.ExitRateLimited {
		t.Errorf("rate_limited: exit %d (%v)", exitCodeOf(err), err)
	}
	gw.replace(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"error":"quota_exceeded","message":"No case runs left."}`))
	})
	if _, err := runCommand(t, EvalCmd(), "run", "--suite", "s"); exitCodeOf(err) != client.ExitUsageExhausted {
		t.Errorf("quota_exceeded: exit %d (%v)", exitCodeOf(err), err)
	}
}

func TestEvalRunCloudTimeoutAndNoWait(t *testing.T) {
	fastPolling(t)
	gw := a2aGateway(t, routeFns{
		"POST /api/evals/runs":      jsonReply(202, `{"runId":"evr_1","estimate":{"units":1}}`),
		"GET /api/evals/runs/evr_1": jsonReply(200, runDoc("running", `null`)),
	})
	out, err := runCommand(t, EvalCmd(), "run", "--suite", "s", "--timeout", "20ms")
	if exitCodeOf(err) != exitWaitTimeout || !strings.Contains(err.Error(), "evr_1") {
		t.Errorf("timeout: exit %d (%v)\n%s", exitCodeOf(err), err, out)
	}
	before := gw.count("GET /api/evals/runs/evr_1")
	if out, err := runCommand(t, EvalCmd(), "run", "--suite", "s", "--no-wait"); err != nil || !strings.Contains(out, "Started run evr_1") || gw.count("GET /api/evals/runs/evr_1") != before {
		t.Errorf("--no-wait: %v %q", err, out)
	}
	if out, err := runCommand(t, EvalCmd(), "run", "--suite", "s", "--no-wait", "--json"); err != nil || !strings.Contains(out, `"runId"`) {
		t.Errorf("--no-wait --json: %v %q", err, out)
	}
}

func TestEvalRunCloudSendsTheCIJob(t *testing.T) {
	fastPolling(t)
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_REPOSITORY", "acme/app")
	t.Setenv("GITHUB_SHA", "abc123")
	t.Setenv("GITHUB_REF", "refs/pull/7/merge")
	gw := a2aGateway(t, routeFns{
		"POST /api/evals/runs":      jsonReply(202, `{"runId":"evr_1","estimate":{"units":1}}`),
		"GET /api/evals/runs/evr_1": jsonReply(200, runDoc("succeeded", `{"passed":true}`)),
	})
	if _, err := runCommand(t, EvalCmd(), "run", "--suite", "s", "--ci"); err != nil {
		t.Fatal(err)
	}
	body := gw.body("POST /api/evals/runs", 0)
	ci, _ := body["ci"].(map[string]any)
	if body["trigger"] != "ci" || ci["pr"] != "7" || ci["repo"] != "acme/app" || ci["sha"] != "abc123" {
		t.Errorf("body = %v", body)
	}
}

func TestEvalAgentSessionsAreRefusedOnSpendingCommands(t *testing.T) {
	fastPolling(t)
	allowed := false
	gw := a2aGateway(t, routeFns{
		"GET /api/evals/settings": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			body := `{"agents_may_run":false}`
			if allowed {
				body = `{"agents_may_run":true}`
			}
			jsonReply(200, body)(nil, w, r)
		},
		"POST /api/evals/runs":          jsonReply(202, `{"runId":"evr_1","estimate":{"units":1}}`),
		"GET /api/evals/runs/evr_1":     jsonReply(200, runDoc("succeeded", `{"passed":true}`)),
		"POST /api/evals/optimize":      jsonReply(202, `{"optimizeId":"evo_1"}`),
		"GET /api/evals/optimize/evo_1": jsonReply(200, `{"optimize":{"id":"evo_1","status":"succeeded","result":{"winner":"c1","base":{"train":0.5,"validation":0.5},"candidates":[{"id":"c1","train":0.7,"validation":0.68}]}}}`),
	})
	t.Setenv("ZUANO_AGENT", "1")
	for _, args := range [][]string{{"run", "--suite", "s"}, {"optimize", "--suite", "s", "--slot", "description"}} {
		_, err := runCommand(t, EvalCmd(), args...)
		if exitCodeOf(err) != exitDenied || !strings.Contains(err.Error(), "evals.agents_may_run") {
			t.Errorf("%v: exit %d (%v)", args, exitCodeOf(err), err)
		}
	}
	if gw.count("POST /api/evals/runs") != 0 || gw.count("POST /api/evals/optimize") != 0 {
		t.Error("a refused command still spent budget")
	}
	allowed = true
	if _, err := runCommand(t, EvalCmd(), "run", "--suite", "s"); err != nil {
		t.Errorf("allowed by the organisation: %v", err)
	}
	gw.replace(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	if _, err := runCommand(t, EvalCmd(), "run", "--suite", "s"); exitCodeOf(err) != exitDenied {
		t.Errorf("an unreadable setting must refuse: %v", err)
	}
}

func TestEvalSuiteByTarget(t *testing.T) {
	fastPolling(t)
	reply := `{"suites":[{"id":"evs_1","slug":"triage"}]}`
	gw := a2aGateway(t, routeFns{
		"GET /api/evals/suites":     func(_ *packServer, w http.ResponseWriter, r *http.Request) { jsonReply(200, reply)(nil, w, r) },
		"POST /api/evals/runs":      jsonReply(202, `{"runId":"evr_1","estimate":{"units":1}}`),
		"GET /api/evals/runs/evr_1": jsonReply(200, runDoc("succeeded", `{"passed":true}`)),
	})
	if _, err := runCommand(t, EvalCmd(), "run", "--target", "skill:pr-triage"); err != nil {
		t.Fatal(err)
	}
	if gw.body("POST /api/evals/runs", 0)["suiteId"] != "evs_1" {
		t.Error("the suite found by target was not used")
	}
	reply = `{"suites":[]}`
	if _, err := runCommand(t, EvalCmd(), "run", "--target", "skill:none"); err == nil || !strings.Contains(err.Error(), "no suite evaluates skill:none") {
		t.Errorf("no suite: %v", err)
	}
	reply = `{"suites":[{"id":"a","slug":"one"},{"id":"b","slug":"two"}]}`
	if _, err := runCommand(t, EvalCmd(), "run", "--target", "skill:x"); err == nil || !strings.Contains(err.Error(), "one, two") {
		t.Errorf("two suites: %v", err)
	}
	if _, err := runCommand(t, EvalCmd(), "run", "--target", "skill"); err == nil {
		t.Error("a target without a slug")
	}
	if _, err := runCommand(t, EvalCmd(), "run", "--config", filepath.Join(t.TempDir(), "none.yaml")); err == nil || !strings.Contains(err.Error(), "--suite") {
		t.Errorf("nothing names a suite: %v", err)
	}
}

func TestEvalRunCloudUsesTheConfigSuite(t *testing.T) {
	fastPolling(t)
	gw := a2aGateway(t, routeFns{
		"POST /api/evals/runs":      jsonReply(202, `{"runId":"evr_1","estimate":{"units":1}}`),
		"GET /api/evals/runs/evr_1": jsonReply(200, runDoc("succeeded", `{"passed":true}`)),
	})
	dir := evalProject(t, "")
	if _, err := runCommand(t, EvalCmd(), "run", "--config", filepath.Join(dir, "zuano.evals.yaml")); err != nil {
		t.Fatal(err)
	}
	if gw.body("POST /api/evals/runs", 0)["suiteId"] != "pdf" {
		t.Error("the config's suite was not used")
	}
	if _, err := runCommand(t, EvalCmd(), "run", "--suite", "s", "--models", "nobody/x"); err == nil {
		t.Error("an unknown provider in --models")
	}
}

func TestEvalCompareReportAndBaselineOnTheGateway(t *testing.T) {
	fastPolling(t)
	cmp := `{"comparison":[{"a":"anthropic/m","b":"anthropic/m","split":"all","pairs":40,"mean_delta":-0.2,"ci_low":-0.3,"ci_high":-0.1,"adjusted_p":0.001,"wins":2,"losses":20,"ties":18,"regression":true,"significant":true}]}`
	gw := a2aGateway(t, routeFns{
		"GET /api/evals/runs/evr_2/compare": jsonReply(200, cmp),
		"GET /api/evals/runs/evr_1":         jsonReply(200, runDoc("succeeded", `{"passed":true}`)),
		"PUT /api/evals/baselines/s/main":   jsonReply(200, `{}`),
		"GET /api/evals/baselines/s/main":   jsonReply(200, `{"baseline":{"ref":"main","run_id":"evr_1","set_by":"patrik","set_at":"2026-10-16"}}`),
	})
	out, err := runCommand(t, EvalCmd(), "compare", "evr_1", "evr_2")
	if err != nil || !strings.Contains(out, "regression") || !strings.Contains(out, "-0.200") {
		t.Errorf("compare ids: %v\n%s", err, out)
	}
	if _, err := runCommand(t, EvalCmd(), "compare", "evr_2", "--against", "baseline:main", "--fail-on-regression"); exitCodeOf(err) != exitEvalRegression {
		t.Errorf("compare against: %v", err)
	}
	if out, err := runCommand(t, EvalCmd(), "compare", "evr_2", "--against", "baseline:main", "--json"); err != nil || !strings.Contains(out, `"comparison"`) {
		t.Errorf("--json: %v", err)
	}
	if _, err := runCommand(t, EvalCmd(), "compare", "evr_2"); err == nil {
		t.Error("one id and no baseline")
	}
	if q := gw.count("GET /api/evals/runs/evr_2/compare"); q != 3 {
		t.Errorf("compare requests = %d", q)
	}

	if out, err := runCommand(t, EvalCmd(), "report", "evr_1"); err != nil || !strings.Contains(out, "Run evr_1 succeeded") {
		t.Errorf("report id: %v %q", err, out)
	}
	if out, err := runCommand(t, EvalCmd(), "baseline", "set", "main", "--cloud", "--suite", "s", "--run-id", "evr_1"); err != nil || !strings.Contains(out, "Baseline main of s is now run evr_1") {
		t.Errorf("baseline set: %v %q", err, out)
	}
	if b := gw.body("PUT /api/evals/baselines/s/main", 0); b["runId"] != "evr_1" {
		t.Errorf("baseline body = %v", b)
	}
	if out, err := runCommand(t, EvalCmd(), "baseline", "show", "main", "--cloud", "--suite", "s"); err != nil || !strings.Contains(out, "run evr_1, set by patrik") {
		t.Errorf("baseline show: %v %q", err, out)
	}
	if _, err := runCommand(t, EvalCmd(), "baseline", "set", "main", "--cloud"); err == nil {
		t.Error("--cloud without a suite")
	}
	if _, err := runCommand(t, EvalCmd(), "baseline", "show", "main", "--cloud"); err == nil {
		t.Error("--cloud show without a suite")
	}
	gw.replace(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"protected_ref","message":"Only an administrator can move main."}`))
	})
	if _, err := runCommand(t, EvalCmd(), "baseline", "set", "main", "--cloud", "--suite", "s", "--run-id", "evr_1"); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Errorf("protected ref: %v", err)
	}
}

func TestEvalReportOfARunStillGoing(t *testing.T) {
	a2aGateway(t, routeFns{"GET /api/evals/runs/evr_9": jsonReply(200, runDoc("running", `null`))})
	out, err := runCommand(t, EvalCmd(), "report", "evr_9")
	if err != nil || !strings.Contains(out, "still running") {
		t.Errorf("report of a running run: %v %q", err, out)
	}
}

func TestEvalDatasetsPushAndPull(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"POST /api/evals/datasets":                       jsonReply(201, `{"dataset":{"id":"evd_1"}}`),
		"POST /api/evals/datasets/evd_1/versions":        jsonReply(201, `{"version":{"seq":3,"case_count":2}}`),
		"GET /api/evals/datasets/evd_1/versions":         jsonReply(200, `{"versions":[{"seq":1},{"seq":3},{"seq":2}]}`),
		"GET /api/evals/datasets/evd_1/versions/3/cases": jsonReply(200, `{"cases":[{"kind":"task","key":"a","name":"n","input":"q","expected":{"rubric":"r"}}]}`),
		"GET /api/evals/datasets/evd_1/versions/1/cases": jsonReply(200, `{"cases":[]}`),
	})
	dir := t.TempDir()
	file := filepath.Join(dir, "cases.jsonl")
	if err := os.WriteFile(file, []byte(`{"kind":"task","key":"a","name":"n","input":"q","expected":{"rubric":"r"}}`+"\n"+`{"kind":"trigger","key":"b","name":"t","input":"x","should_trigger":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCommand(t, EvalCmd(), "datasets", "push", file, "--name", "triage", "--sensitivity", "confidential")
	if err != nil || !strings.Contains(out, "Dataset evd_1 is at version 3 with 2 case(s)") {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if b := gw.body("POST /api/evals/datasets", 0); b["name"] != "triage" || b["sensitivity"] != "confidential" {
		t.Errorf("create body = %v", b)
	}
	if cases, _ := gw.body("POST /api/evals/datasets/evd_1/versions", 0)["cases"].([]any); len(cases) != 2 {
		t.Errorf("version body = %v", gw.body("POST /api/evals/datasets/evd_1/versions", 0))
	}
	if _, err := runCommand(t, EvalCmd(), "datasets", "push", file, "--dataset", "evd_1"); err != nil || gw.count("POST /api/evals/datasets") != 1 {
		t.Errorf("push to an existing dataset: %v", err)
	}
	if out, err := runCommand(t, EvalCmd(), "datasets", "push", file, "--dataset", "evd_1", "--json"); err != nil || !strings.Contains(out, `"version"`) {
		t.Errorf("push --json: %v", err)
	}
	for name, args := range map[string][]string{
		"neither": {"datasets", "push", file},
		"both":    {"datasets", "push", file, "--name", "x", "--dataset", "y"},
		"missing": {"datasets", "push", filepath.Join(dir, "none"), "--name", "x"},
	} {
		if _, err := runCommand(t, EvalCmd(), args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	dst := filepath.Join(dir, "pulled", "cases.jsonl")
	if out, err := runCommand(t, EvalCmd(), "datasets", "pull", "evd_1", "--out", dst); err != nil || !strings.Contains(out, "Wrote 1 case(s) of version 3") {
		t.Errorf("pull: %v %q", err, out)
	}
	if cs, err := evals.LoadDataset(dst, nil); err != nil || len(cs) != 1 {
		t.Errorf("pulled file: %v", err)
	}
	if out, err := runCommand(t, EvalCmd(), "datasets", "pull", "evd_1"); err != nil || !strings.Contains(out, evals.CaseFormat) {
		t.Errorf("pull to stdout: %v %q", err, out)
	}
	if _, err := runCommand(t, EvalCmd(), "datasets", "pull", "evd_1", "--version", "1"); err == nil {
		t.Error("an empty version")
	}
	gw.replace(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"versions":[]}`)) })
	if _, err := runCommand(t, EvalCmd(), "datasets", "pull", "evd_1"); err == nil {
		t.Error("a dataset without versions")
	}
}

func TestEvalOptimize(t *testing.T) {
	fastPolling(t)
	var polls atomic.Int32
	job := func(status, winner string) string {
		return `{"optimize":{"id":"evo_1","status":"` + status + `","error":"boom","result":{"winner":"` + winner + `","base":{"train":0.5,"validation":0.5},"candidates":[{"id":"c1","train":0.8,"validation":0.7,"test":0.69,"has_test":true},{"id":"c2","train":0.95,"validation":0.5,"overfit":true},{"id":"c3","train":0.7,"validation":0.7,"leakage":true}]}}}`
	}
	current := job("succeeded", "c1")
	gw := a2aGateway(t, routeFns{
		"POST /api/evals/optimize": jsonReply(202, `{"optimizeId":"evo_1","estimate":{"units":96}}`),
		"GET /api/evals/optimize/evo_1": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			polls.Add(1)
			jsonReply(200, current)(nil, w, r)
		},
		"POST /api/evals/optimize/evo_1/apply": jsonReply(200, `{}`),
		"GET /api/evals/suites":                jsonReply(200, `{"suites":[{"id":"evs_1","slug":"triage"}]}`),
	})
	out, err := runCommand(t, EvalCmd(), "optimize", "--target", "skill:pr-triage", "--slot", "description", "--candidates", "3", "--test-split", "--models", "anthropic/m", "--apply", "--sink", "draft")
	if err != nil {
		t.Fatalf("optimize: %v\n%s", err, out)
	}
	for _, want := range []string{"Started optimization evo_1 (96 case runs reserved)", "Base: train 0.500, validation 0.500", "* c1  train 0.800, validation 0.700, test 0.690", "c2  train 0.950, validation 0.500 overfit", "leakage", "Applied candidate c1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	b := gw.body("POST /api/evals/optimize", 0)
	if b["suiteId"] != "evs_1" || b["slot"] != "description" || b["candidates"] != 3.0 || b["useTestSplit"] != true || b["overfitThreshold"] != 0.15 {
		t.Errorf("body = %v", b)
	}
	if a := gw.body("POST /api/evals/optimize/evo_1/apply", 0); a["candidate"] != "c1" || a["sink"] != "draft" {
		t.Errorf("apply body = %v", a)
	}

	current = job("succeeded", "")
	if out, err := runCommand(t, EvalCmd(), "optimize", "--suite", "s", "--slot", "system"); err != nil || !strings.Contains(out, "No candidate beat the base") {
		t.Errorf("no winner: %v\n%s", err, out)
	}
	if _, err := runCommand(t, EvalCmd(), "optimize", "--suite", "s", "--slot", "system", "--apply"); err == nil || !strings.Contains(err.Error(), "nothing to apply") {
		t.Errorf("apply without a winner: %v", err)
	}
	current = job("failed", "")
	if _, err := runCommand(t, EvalCmd(), "optimize", "--suite", "s", "--slot", "system"); exitCodeOf(err) != exitRunFailed || !strings.Contains(err.Error(), "boom") {
		t.Errorf("failed job: %v", err)
	}
	if out, err := runCommand(t, EvalCmd(), "optimize", "--suite", "s", "--slot", "system", "--no-wait"); err != nil || strings.Contains(out, "Base:") {
		t.Errorf("--no-wait: %v %q", err, out)
	}
	if out, err := runCommand(t, EvalCmd(), "optimize", "--suite", "s", "--slot", "system", "--json"); exitCodeOf(err) != exitRunFailed || !strings.Contains(out, `"optimize"`) {
		t.Errorf("--json: %v", err)
	}
	for name, args := range map[string][]string{
		"no slot":   {"optimize", "--suite", "s"},
		"no suite":  {"optimize", "--slot", "system"},
		"bad scope": {"optimize", "--target", "x", "--slot", "system"},
	} {
		if _, err := runCommand(t, EvalCmd(), args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	gw.replace(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"error":"plan_required","feature":"evals.optimize","message":"Optimization needs the Scale plan."}`))
	})
	if _, err := runCommand(t, EvalCmd(), "optimize", "--suite", "s", "--slot", "system"); exitCodeOf(err) != client.ExitPlanRequired {
		t.Errorf("plan: exit %d (%v)", exitCodeOf(err), err)
	}
}

func TestEvalReview(t *testing.T) {
	queues := `{"queues":[{"id":"evq_closed","name":"old","status":"closed","open_items":0},{"id":"evq_1","name":"Failures","status":"open","open_items":12}]}`
	a2aGateway(t, routeFns{
		"GET /api/evals/review-queues": func(_ *packServer, w http.ResponseWriter, r *http.Request) { jsonReply(200, queues)(nil, w, r) },
	})
	out, err := runCommand(t, EvalCmd(), "review", "--no-browser")
	if err != nil || !strings.Contains(out, "evq_1  Failures  12 open item(s)") || strings.Contains(out, "evq_closed") || !strings.Contains(out, "/evals/review/evq_1") {
		t.Errorf("review: %v\n%s", err, out)
	}
	if out, err := runCommand(t, EvalCmd(), "review", "--suite", "s", "--json"); err != nil || !strings.Contains(out, `"queues"`) {
		t.Errorf("review --json: %v", err)
	}
	queues = `{"queues":[]}`
	if out, err := runCommand(t, EvalCmd(), "review"); err != nil || !strings.Contains(out, "No review queue is open") {
		t.Errorf("no queues: %v %q", err, out)
	}
}

func TestEvalLeaderboard(t *testing.T) {
	entries := `{"entries":[{"entry_key":"v2 x m","metric":"pass_rate","value":0.91,"ci_low":0.85,"ci_high":0.95,"rating":1520,"n":120,"cost_per_pass_nano":2500000},{"entry_key":"v1 x m","metric":"pass_rate","value":0.8,"n":120}]}`
	gw := a2aGateway(t, routeFns{
		"GET /api/evals/leaderboards/s": func(_ *packServer, w http.ResponseWriter, r *http.Request) { jsonReply(200, entries)(nil, w, r) },
		"GET /api/evals/suites":         jsonReply(200, `{"suites":[{"id":"s","slug":"triage"}]}`),
	})
	out, err := runCommand(t, EvalCmd(), "leaderboard", "--suite", "s", "--split", "validation", "--tag", "billing")
	if err != nil {
		t.Fatalf("leaderboard: %v", err)
	}
	for _, want := range []string{" 1. v2 x m  pass_rate 0.910 (0.850 to 0.950)  n=120  rating 1520  $0.0025 per pass", " 2. v1 x m  pass_rate 0.800  n=120"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if gw.count("GET /api/evals/leaderboards/s") != 1 {
		t.Error("the board was not requested")
	}
	if out, err := runCommand(t, EvalCmd(), "leaderboard", "--target", "skill:triage", "--json"); err != nil || !strings.Contains(out, `"entries"`) {
		t.Errorf("--json by target: %v", err)
	}
	if _, err := runCommand(t, EvalCmd(), "leaderboard"); err == nil {
		t.Error("no suite")
	}
	entries = `{"entries":[]}`
	if out, err := runCommand(t, EvalCmd(), "leaderboard", "--suite", "s"); err != nil || !strings.Contains(out, "No entries yet") {
		t.Errorf("empty board: %v %q", err, out)
	}
}
