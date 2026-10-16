package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

const evalSkillPath = "/api/marketplace/skills/res_ev"

// evalGateway is an in-memory stand-in for the skill eval routes.
type evalGateway struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	bodies   map[string]map[string]any

	runStates []string // statuses returned by successive GETs of a run
	runPolls  int
	summary   string
	runError  string

	jobStates []string
	jobPolls  int
	jobResult string

	refuse map[string]int // "METHOD suffix" -> forced status
}

const triggerSummary = `{"total":6,"completed":6,"variants":[{"label":"1.1.0","versionId":"rev_2",
 "trigger":{"train":{"precision":{"mean":1,"stddev":0,"n":3},"recall":{"mean":0.8,"stddev":0.1,"n":3},"f1":{"mean":0.89,"stddev":0.05,"n":3},"cases":4},
            "validation":{"precision":{"mean":1,"stddev":0,"n":3},"recall":{"mean":0.5,"stddev":0,"n":3},"f1":{"mean":0.67,"stddev":0,"n":3},"cases":2},
            "all":{"precision":{"mean":1,"stddev":0,"n":3},"recall":{"mean":0.7,"stddev":0,"n":3},"f1":{"mean":0.82,"stddev":0,"n":3},"cases":6}},
 "inputTokens":1200,"outputTokens":300,"costUsd":0.0123,"latencyMs":{"mean":420,"stddev":80,"n":18},"errors":1,"skipped":0}],"comparisons":[]}`

func newEvalGateway(t *testing.T) *evalGateway {
	t.Helper()
	g := &evalGateway{
		bodies:    map[string]map[string]any{},
		runStates: []string{"queued", "running", "succeeded"},
		summary:   triggerSummary,
		jobStates: []string{"running", "succeeded"},
		refuse:    map[string]int{},
	}
	evalPollInterval, evalSettleDelay = time.Millisecond, 0
	t.Setenv("RELAY_AGENT", "")
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	withAPIKeyGateway(t, g.Server)
	return g
}

func (g *evalGateway) body(key string) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.bodies[key]
}

func (g *evalGateway) called(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.requests {
		if r == key {
			return true
		}
	}
	return false
}

func (g *evalGateway) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	reply := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	raw := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}

	suffix := strings.TrimPrefix(r.URL.Path, evalSkillPath)
	key := r.Method + " " + suffix
	if !strings.HasPrefix(r.URL.Path, evalSkillPath) {
		key = r.Method + " " + r.URL.Path
	}
	g.requests = append(g.requests, key)
	if data, _ := io.ReadAll(r.Body); len(data) > 0 {
		var doc map[string]any
		_ = json.Unmarshal(data, &doc)
		g.bodies[key] = doc
	}
	if status, ok := g.refuse[key]; ok {
		reply(status, map[string]string{"error": "Forbidden: Agent credentials cannot change this"})
		return
	}

	set := `{"id":"evs_1","name":"smoke","trainPercent":70,"counts":{"trigger":4,"task":0,"train":4,"validation":2}}`
	switch key {
	case "GET /api/catalog/skills/search":
		raw(200, `{"ok":true,"data":[{"id":"res_ev","slug":"evskill","description":"d","version":"1.1.0","trustScore":90}]}`)
	case "GET /api/catalog/res_ev":
		raw(200, `{"versions":[{"id":"rev_1","semver":"1.0.0","state":"published"},{"id":"rev_2","semver":"1.1.0","state":"published"}],"currentVersion":{"id":"rev_2","semver":"1.1.0"}}`)
	case "GET /evals/sets":
		raw(200, `{"sets":[`+set+`]}`)
	case "POST /evals/sets":
		raw(201, `{"set":{"id":"evs_2","name":"new","counts":{}}}`)
	case "GET /evals/sets/evs_1":
		raw(200, `{"set":{"id":"evs_1","name":"smoke","description":"basics","trainPercent":70,"counts":{"trigger":1,"task":0,"train":1,"validation":0},"cases":[{"id":"evc_1","kind":"trigger","name":"asks for an invoice","input":"Please invoice Acme","split":"train","shouldTrigger":true}]}}`)
	case "DELETE /evals/sets/evs_1":
		raw(200, `{"deleted":true}`)
	case "GET /evals/sets/evs_1/export":
		raw(200, `{"format":"relay.skill-evals/v1","set":{"name":"smoke"},"cases":[]}`)
	case "POST /evals/import":
		raw(201, `{"set":{"id":"evs_3","name":"smoke","counts":{"trigger":2,"task":0}}}`)
	case "POST /evals/sets/evs_1/cases":
		raw(201, `{"case":{"id":"evc_9","kind":"trigger","split":"validation"}}`)
	case "DELETE /evals/sets/evs_1/cases/evc_9":
		raw(200, `{"deleted":true}`)
	case "POST /evals/runs":
		raw(202, `{"run":{"id":"evr_1","status":"queued"}}`)
	case "GET /evals/runs/evr_1":
		i := g.runPolls
		if i >= len(g.runStates) {
			i = len(g.runStates) - 1
		}
		g.runPolls++
		status := g.runStates[i]
		run := `{"id":"evr_1","setId":"evs_1","kind":"trigger","status":"` + status + `","repeats":3,"model":"m","judgeModel":"m","inputTokens":1200,"outputTokens":300,"error":`
		if g.runError != "" && status == "failed" {
			run += `"` + g.runError + `"`
		} else {
			run += `null`
		}
		if terminalStatus(status) {
			run += `,"summary":` + g.summary
		} else {
			run += `,"summary":null`
		}
		raw(200, `{"run":`+run+`},"results":[],"progress":{"done":2,"total":6}}`)
	case "DELETE /evals/runs/evr_1":
		raw(200, `{"cancelled":true}`)
	case "POST /optimize":
		raw(202, `{"job":{"id":"opj_1","status":"queued"}}`)
	case "GET /optimize/opj_1":
		i := g.jobPolls
		if i >= len(g.jobStates) {
			i = len(g.jobStates) - 1
		}
		g.jobPolls++
		status := g.jobStates[i]
		result := `null`
		if status == "succeeded" {
			result = g.jobResult
		}
		errText := `null`
		if status == "failed" {
			errText = `"budget spent"`
		}
		raw(200, `{"job":{"id":"opj_1","setId":"evs_1","kind":"description","status":"`+status+`","error":`+errText+`,"inputTokens":10,"outputTokens":5,"costUsd":0.001,"result":`+result+`},"drafts":[{"id":"sdr_1","kind":"description","status":"draft","label":"shorter"}]}`)
	case "DELETE /optimize/opj_1":
		raw(200, `{"cancelled":true}`)
	case "GET /drafts":
		raw(200, `{"drafts":[{"id":"sdr_1","kind":"description","status":"draft","label":"shorter","createdAt":"2026-10-16"}]}`)
	case "GET /drafts/sdr_1":
		raw(200, `{"draft":{"id":"sdr_1","kind":"description","status":"draft","label":"shorter","baseVersionId":"rev_2","description":"Invoices customers.","notes":{"trainGain":0.2,"validationGain":0.1,"overfit":false}}}`)
	case "DELETE /drafts/sdr_1":
		raw(200, `{"draft":{"id":"sdr_1","status":"discarded"}}`)
	case "POST /drafts/sdr_1/promote":
		raw(201, `{"draft":{"id":"sdr_1","status":"promoted"},"published":{"resourceId":"res_ev","versionId":"rev_3","semver":"1.1.1","state":"in_review"}}`)
	case "GET /quality":
		raw(200, `{"score":82.4,"latest":{},"history":[{"runId":"evr_1","kind":"trigger","at":"2026-10-16","versionId":"rev_2","semver":"1.1.0","train":0.9,"validation":0.8,"errors":0}]}`)
	case "GET /api/marketplace/skills/quality":
		raw(200, `{"skills":[{"slug":"evskill","name":"Eval skill","score":40,"evaluatedAt":"2026-10-16"},{"slug":"fresh","name":"Fresh","score":null}]}`)
	default:
		http.NotFound(w, r)
	}
}

func openEvalAPI(t *testing.T, g *evalGateway) *evalAPI {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	api, err := (&evalFlags{gatewayURL: g.URL}).open(cmd, "evskill")
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func evalCmd(t *testing.T, g *evalGateway, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	return runCommand(t, cmd, append(args, "--gateway-url", g.URL)...)
}

func TestEvalSets_ListShowCreateExportImportDelete(t *testing.T) {
	g := newEvalGateway(t)

	out, err := evalCmd(t, g, SkillEvalsCmd(), "sets", "list", "evskill")
	if err != nil || !strings.Contains(out, "evs_1") || !strings.Contains(out, "smoke") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	out, err = evalCmd(t, g, SkillEvalsCmd(), "sets", "show", "evskill", "smoke")
	if err != nil || !strings.Contains(out, "evc_1") || !strings.Contains(out, "asks for an invoice") || !strings.Contains(out, "70% train") {
		t.Fatalf("show: %v\n%s", err, out)
	}
	out, err = evalCmd(t, g, SkillEvalsCmd(), "sets", "create", "evskill", "--name", "new", "--version", "1.0.0", "--train-percent", "60")
	if err != nil || !strings.Contains(out, "Created eval set new (evs_2)") {
		t.Fatalf("create: %v\n%s", err, out)
	}
	if b := g.body("POST /evals/sets"); b["name"] != "new" || b["versionId"] != "rev_1" || b["trainPercent"] != float64(60) {
		t.Errorf("create body = %v", b)
	}

	file := filepath.Join(t.TempDir(), "set.json")
	if out, err = evalCmd(t, g, SkillEvalsCmd(), "sets", "export", "evskill", "evs_1", "--out", file); err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	data, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(data), "relay.skill-evals/v1") {
		t.Fatalf("export file: %v %s", err, data)
	}
	out, err = evalCmd(t, g, SkillEvalsCmd(), "sets", "import", "evskill", file, "--version", "1.1.0")
	if err != nil || !strings.Contains(out, "Imported eval set smoke (evs_3)") {
		t.Fatalf("import: %v\n%s", err, out)
	}
	if !g.called("POST /evals/import") {
		t.Error("import was not posted")
	}
	if b := g.body("POST /evals/import"); b["format"] != "relay.skill-evals/v1" {
		t.Errorf("import body = %v", b)
	}

	if _, err = evalCmd(t, g, SkillEvalsCmd(), "sets", "delete", "evskill", "smoke"); err == nil || g.called("DELETE /evals/sets/evs_1") {
		t.Fatalf("delete without --yes: %v", err)
	}
	if out, err = evalCmd(t, g, SkillEvalsCmd(), "sets", "delete", "evskill", "smoke", "--yes"); err != nil || !strings.Contains(out, "Deleted eval set smoke") {
		t.Fatalf("delete: %v\n%s", err, out)
	}
}

func TestEvalSets_JSONOutput(t *testing.T) {
	g := newEvalGateway(t)
	for _, args := range [][]string{
		{"sets", "list", "evskill"},
		{"sets", "show", "evskill", "evs_1"},
		{"sets", "create", "evskill", "--name", "n"},
		{"sets", "delete", "evskill", "evs_1", "--yes"},
	} {
		out, err := evalCmd(t, g, SkillEvalsCmd(), append(args, "--json")...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var v any
		if json.Unmarshal([]byte(out), &v) != nil {
			t.Errorf("%v did not print JSON:\n%s", args, out)
		}
	}
}

func TestEvalSets_ImportFromStdinRejectsInvalidJSON(t *testing.T) {
	g := newEvalGateway(t)
	cmd := SkillEvalsCmd()
	cmd.SetIn(strings.NewReader("not json"))
	if _, err := evalCmd(t, g, cmd, "sets", "import", "evskill", "-"); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("err = %v", err)
	}
	if g.called("POST /evals/import") {
		t.Error("an invalid document was sent")
	}
}

func TestEvalCase_AddAndRemove(t *testing.T) {
	g := newEvalGateway(t)

	out, err := evalCmd(t, g, SkillEvalsCmd(), "case", "add", "evskill", "smoke", "--kind", "trigger", "--name", "n", "--input", "invoice acme", "--should-trigger=false")
	if err != nil || !strings.Contains(out, "evc_9") {
		t.Fatalf("add trigger: %v\n%s", err, out)
	}
	if b := g.body("POST /evals/sets/evs_1/cases"); b["kind"] != "trigger" || b["shouldTrigger"] != false {
		t.Errorf("trigger body = %v", b)
	}

	if _, err = evalCmd(t, g, SkillEvalsCmd(), "case", "add", "evskill", "smoke", "--kind", "task", "--name", "t", "--input", "total?",
		"--contains", "1 250", "--not-contains", "error", "--regex", "EUR|€", "--rubric", "states currency", "--connector", "fakturoid", "--split", "validation"); err != nil {
		t.Fatalf("add task: %v", err)
	}
	b := g.body("POST /evals/sets/evs_1/cases")
	exp, _ := b["expected"].(map[string]any)
	asserts, _ := exp["assertions"].([]any)
	if len(asserts) != 3 || exp["rubric"] != "states currency" || b["split"] != "validation" || b["requiredConnectors"].([]any)[0] != "fakturoid" {
		t.Errorf("task body = %v", b)
	}

	if out, err = evalCmd(t, g, SkillEvalsCmd(), "case", "remove", "evskill", "smoke", "evc_9"); err != nil || !strings.Contains(out, "Removed case evc_9") {
		t.Fatalf("remove: %v\n%s", err, out)
	}
}

func TestEvalCase_AddValidatesBeforeSending(t *testing.T) {
	g := newEvalGateway(t)
	for name, args := range map[string][]string{
		"no kind":              {"--name", "n", "--input", "i"},
		"trigger without flag": {"--kind", "trigger", "--name", "n", "--input", "i"},
		"task without grader":  {"--kind", "task", "--name", "n", "--input", "i"},
		"no input":             {"--kind", "task", "--name", "n", "--rubric", "r"},
	} {
		if _, err := evalCmd(t, g, SkillEvalsCmd(), append([]string{"case", "add", "evskill", "smoke"}, args...)...); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if g.called("POST /evals/sets/evs_1/cases") {
		t.Error("an invalid case was sent")
	}
}

func TestEvalCase_AddFromFile(t *testing.T) {
	g := newEvalGateway(t)
	file := filepath.Join(t.TempDir(), "case.json")
	if err := os.WriteFile(file, []byte(`{"kind":"task","name":"x","input":"y","expected":{"assertions":[{"type":"json_path","path":"a","exists":true}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := evalCmd(t, g, SkillEvalsCmd(), "case", "add", "evskill", "smoke", "--file", file); err != nil {
		t.Fatal(err)
	}
	if b := g.body("POST /evals/sets/evs_1/cases"); b["name"] != "x" {
		t.Errorf("body = %v", b)
	}
}

func TestEval_StartsRunWithoutWaiting(t *testing.T) {
	g := newEvalGateway(t)
	out, err := evalCmd(t, g, SkillEvalCmd(), "evskill", "--repeats", "5", "--split", "validation", "--model", "m1")
	if err != nil {
		t.Fatalf("eval: %v\n%s", err, out)
	}
	if !strings.Contains(out, "evr_1") || !strings.Contains(out, "--run evr_1 --wait") {
		t.Errorf("output:\n%s", out)
	}
	b := g.body("POST /evals/runs")
	variants, _ := b["variants"].([]any)
	v, _ := variants[0].(map[string]any)
	if b["setId"] != "evs_1" || b["kind"] != "trigger" || b["repeats"] != float64(5) || b["model"] != "m1" || v["versionId"] != "rev_2" {
		t.Errorf("run body = %v", b)
	}
	if g.called("GET /evals/runs/evr_1") {
		t.Error("polled without --wait")
	}
}

func TestEval_WaitPrintsScores(t *testing.T) {
	g := newEvalGateway(t)
	out, err := evalCmd(t, g, SkillEvalCmd(), "evskill", "--wait", "--version", "1.0.0")
	if err != nil {
		t.Fatalf("eval --wait: %v\n%s", err, out)
	}
	for _, want := range []string{"Run evr_1  succeeded", "1.1.0", "train", "validation", "F1 0.89 ±0.05", "recall 0.50", "1 errors", "$0.0123"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if g.runPolls < 3 {
		t.Errorf("polled %d times, want it to follow queued, running, succeeded", g.runPolls)
	}
	variants, _ := g.body("POST /evals/runs")["variants"].([]any)
	if v, _ := variants[0].(map[string]any); v["versionId"] != "rev_1" {
		t.Errorf("--version 1.0.0 sent %v", variants)
	}
}

func TestEval_WaitJSONPrintsOnlyTheFinalDocument(t *testing.T) {
	g := newEvalGateway(t)
	out, err := evalCmd(t, g, SkillEvalCmd(), "evskill", "--wait", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Run struct {
			Status string `json:"status"`
		} `json:"run"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Run.Status != "succeeded" {
		t.Fatalf("not the final run document: %v\n%s", err, out)
	}
}

func TestEval_DraftIsAVariant(t *testing.T) {
	g := newEvalGateway(t)
	if _, err := evalCmd(t, g, SkillEvalCmd(), "evskill", "--draft", "sdr_1"); err != nil {
		t.Fatal(err)
	}
	variants, _ := g.body("POST /evals/runs")["variants"].([]any)
	if v, _ := variants[0].(map[string]any); v["draftId"] != "sdr_1" || v["versionId"] != nil {
		t.Errorf("variants = %v", variants)
	}
	if _, err := evalCmd(t, g, SkillEvalCmd(), "evskill", "--draft", "sdr_1", "--version", "1.0.0"); err == nil {
		t.Error("--draft with --version was accepted")
	}
}

func TestEval_FailedRunExitsNonZero(t *testing.T) {
	g := newEvalGateway(t)
	g.runStates = []string{"running", "failed"}
	g.runError = "token budget reached"
	out, err := evalCmd(t, g, SkillEvalCmd(), "evskill", "--wait")
	if err == nil || ExitCode(err) != exitEvalFailed || !strings.Contains(err.Error(), "token budget reached") {
		t.Fatalf("err = %v (exit %d)\n%s", err, ExitCode(err), out)
	}
	if !strings.Contains(out, "failed") {
		t.Errorf("the failed run was not printed:\n%s", out)
	}
}

func TestEval_FailOnRegression(t *testing.T) {
	regressed := strings.Replace(triggerSummary, `"comparisons":[]`, `"comparisons":[],"regressions":[{"versionId":"rev_2","previousVersionId":"rev_1","kind":"trigger","score":0.67,"previous":0.9}]`, 1)

	g := newEvalGateway(t)
	g.summary = regressed
	out, err := evalCmd(t, g, SkillEvalCmd(), "evskill", "--fail-on-regression")
	if ExitCode(err) != exitEvalRegression || !strings.Contains(err.Error(), "fell from 90% to 67%") {
		t.Fatalf("err = %v (exit %d)\n%s", err, ExitCode(err), out)
	}
	if !strings.Contains(out, "Regressions:") {
		t.Errorf("regressions not printed:\n%s", out)
	}

	g = newEvalGateway(t)
	g.summary = strings.Replace(triggerSummary, `"errors":1`, `"errors":0`, 1)
	if out, err = evalCmd(t, g, SkillEvalCmd(), "evskill", "--fail-on-regression"); err != nil {
		t.Fatalf("clean run failed: %v\n%s", err, out)
	}

	g = newEvalGateway(t)
	g.summary = regressed
	if _, err = evalCmd(t, g, SkillEvalCmd(), "evskill", "--wait"); err != nil {
		t.Fatalf("a regression without --fail-on-regression must not fail: %v", err)
	}
}

func TestEval_FailOnRegressionFailsWhenNothingWasVerified(t *testing.T) {
	clean := strings.Replace(triggerSummary, `"errors":1`, `"errors":0`, 1)
	for name, summary := range map[string]string{
		"nothing scored": strings.Replace(clean, `"completed":6`, `"completed":0`, 1),
		"cases errored":  triggerSummary,
		"no summary":     "null",
	} {
		g := newEvalGateway(t)
		g.summary = summary
		_, err := evalCmd(t, g, SkillEvalCmd(), "evskill", "--fail-on-regression")
		if ExitCode(err) != exitEvalRegression || !strings.Contains(err.Error(), "not verified") {
			t.Errorf("%s: err = %v (exit %d), want exit %d", name, err, ExitCode(err), exitEvalRegression)
		}
		g.runPolls = 0
		if _, err := evalCmd(t, g, SkillEvalCmd(), "evskill", "--wait"); err != nil {
			t.Errorf("%s: without --fail-on-regression the run still reports success: %v", name, err)
		}
	}
}

func TestEval_WaitTimeoutExitsWithTheTimeoutStatus(t *testing.T) {
	g := newEvalGateway(t)
	g.runStates = []string{"running"}
	out, err := evalCmd(t, g, SkillEvalCmd(), "evskill", "--wait", "--wait-timeout", "20ms")
	if err == nil || ExitCode(err) != 4 || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "--run evr_1 --wait") {
		t.Fatalf("err = %v (exit %d)\n%s", err, ExitCode(err), out)
	}
}

func TestEval_ExistingRunAndCancel(t *testing.T) {
	g := newEvalGateway(t)
	if out, err := evalCmd(t, g, SkillEvalCmd(), "evskill", "--run", "evr_1", "--wait"); err != nil || !strings.Contains(out, "succeeded") {
		t.Fatalf("--run: %v\n%s", err, out)
	}
	if g.called("POST /evals/runs") {
		t.Error("--run started a new run")
	}
	if out, err := evalCmd(t, g, SkillEvalCmd(), "evskill", "--run", "evr_1", "--cancel"); err != nil || !strings.Contains(out, "Cancelled run evr_1") {
		t.Fatalf("--cancel: %v\n%s", err, out)
	}
}

func TestEval_KindMustBeClearFromTheSet(t *testing.T) {
	both := evalSet{Name: "s"}
	both.Counts.Trigger, both.Counts.Task = 1, 1
	if _, err := pickKind("", both); err == nil || !strings.Contains(err.Error(), "--kind") {
		t.Errorf("mixed set: %v", err)
	}
	taskOnly := evalSet{Name: "s"}
	taskOnly.Counts.Task = 2
	if k, err := pickKind("", taskOnly); err != nil || k != "task" {
		t.Errorf("task set: %q %v", k, err)
	}
	if k, err := pickKind("trigger", both); err != nil || k != "trigger" {
		t.Errorf("explicit kind: %q %v", k, err)
	}
	if _, err := pickKind("", evalSet{Name: "empty"}); err == nil {
		t.Error("an empty set was accepted")
	}
}

func TestCompare_RunsSeveralVariantsAndFlagsASignificantLoss(t *testing.T) {
	g := newEvalGateway(t)
	g.summary = strings.Replace(triggerSummary, `"comparisons":[]`, `"comparisons":[{"baseline":"1.0.0","variant":"1.1.0","winLoss":{"all":{"wins":0,"losses":4,"ties":2},"validation":{"wins":0,"losses":2,"ties":0}},"significance":{"validation":{"meanDelta":-0.25,"ciLow":-0.4,"ciHigh":-0.1,"pValue":0.01,"significant":true}}}]`, 1)

	out, err := evalCmd(t, g, SkillCompareCmd(), "evskill", "--versions", "1.0.0,1.1.0", "--wait")
	if err != nil {
		t.Fatalf("compare: %v\n%s", err, out)
	}
	for _, want := range []string{"1.1.0 vs 1.0.0", "0 wins, 2 losses, 0 ties", "mean delta -0.25", "significant", "Regressions:", "significantly worse than 1.0.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	variants, _ := g.body("POST /evals/runs")["variants"].([]any)
	if len(variants) != 2 {
		t.Fatalf("variants = %v", variants)
	}

	g.runPolls = 0
	_, err = evalCmd(t, g, SkillCompareCmd(), "evskill", "--versions", "1.0.0,sdr_1", "--fail-on-regression")
	if ExitCode(err) != exitEvalRegression {
		t.Errorf("exit = %d, want %d (%v)", ExitCode(err), exitEvalRegression, err)
	}
	variants, _ = g.body("POST /evals/runs")["variants"].([]any)
	if v, _ := variants[1].(map[string]any); v["draftId"] != "sdr_1" {
		t.Errorf("a sdr_ id should be a draft variant: %v", variants)
	}

	if _, err = evalCmd(t, g, SkillCompareCmd(), "evskill", "--versions", "1.0.0"); err == nil {
		t.Error("a single version was accepted")
	}
}

func TestCompare_KeepsTheOrderTheUserGaveSoTheBaselineIsTheFirst(t *testing.T) {
	g := newEvalGateway(t)
	if _, err := evalCmd(t, g, SkillCompareCmd(), "evskill", "--versions", "sdr_1,1.0.0,1.1.0"); err != nil {
		t.Fatal(err)
	}
	variants, _ := g.body("POST /evals/runs")["variants"].([]any)
	if len(variants) != 3 {
		t.Fatalf("variants = %v", variants)
	}
	first, _ := variants[0].(map[string]any)
	second, _ := variants[1].(map[string]any)
	third, _ := variants[2].(map[string]any)
	if first["draftId"] != "sdr_1" || second["label"] != "1.0.0" || third["label"] != "1.1.0" {
		t.Errorf("order changed: %v", variants)
	}
}

const optimizeResultJSON = `{"baseline":{"train":0.5,"validation":0.6},"candidates":[
 {"index":0,"text":"a","draftId":"sdr_1","train":0.8,"trainGain":0.3,"validation":0.7,"validationGain":0.1,"overfit":false},
 {"index":1,"text":"b","train":0.6,"trainGain":0.1,"overfit":false}],
 "bestIndex":0,"overfit":false,"reasons":["Kept candidate 0."]}`

func TestOptimize_StartWaitAndRender(t *testing.T) {
	g := newEvalGateway(t)
	g.jobResult = optimizeResultJSON

	out, err := evalCmd(t, g, SkillOptimizeCmd(), "evskill", "--kind", "description", "--candidates", "3", "--version", "1.0.0")
	if err != nil || !strings.Contains(out, "opj_1") || !strings.Contains(out, "--job opj_1 --wait") {
		t.Fatalf("start: %v\n%s", err, out)
	}
	if b := g.body("POST /optimize"); b["kind"] != "description" || b["setId"] != "evs_1" || b["candidates"] != float64(3) || b["versionId"] != "rev_1" {
		t.Errorf("body = %v", b)
	}

	g.jobPolls = 0
	out, err = evalCmd(t, g, SkillOptimizeCmd(), "evskill", "--kind", "description", "--wait")
	if err != nil {
		t.Fatalf("wait: %v\n%s", err, out)
	}
	for _, want := range []string{"Baseline: train 0.50, validation 0.60", "0 (best)", "+0.30", "Kept candidate 0.", "relay skill drafts show evskill sdr_1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestOptimize_FailedJobAndCancel(t *testing.T) {
	g := newEvalGateway(t)
	g.jobStates = []string{"failed"}
	out, err := evalCmd(t, g, SkillOptimizeCmd(), "evskill", "--job", "opj_1", "--wait")
	if ExitCode(err) != exitEvalFailed || !strings.Contains(err.Error(), "budget spent") {
		t.Fatalf("err = %v (exit %d)\n%s", err, ExitCode(err), out)
	}
	if out, err = evalCmd(t, g, SkillOptimizeCmd(), "evskill", "--job", "opj_1", "--cancel"); err != nil || !strings.Contains(out, "Cancelled job opj_1") {
		t.Fatalf("cancel: %v\n%s", err, out)
	}
	if _, err = evalCmd(t, g, SkillOptimizeCmd(), "evskill", "--kind", "wording"); err == nil {
		t.Error("an unknown kind was accepted")
	}
}

func TestDrafts_ListShowPromoteDiscard(t *testing.T) {
	g := newEvalGateway(t)

	out, err := evalCmd(t, g, SkillDraftsCmd(), "list", "evskill")
	if err != nil || !strings.Contains(out, "sdr_1") || !strings.Contains(out, "shorter") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	out, err = evalCmd(t, g, SkillDraftsCmd(), "show", "evskill", "sdr_1")
	if err != nil || !strings.Contains(out, "Invoices customers.") || !strings.Contains(out, "trainGain") {
		t.Fatalf("show: %v\n%s", err, out)
	}
	out, err = evalCmd(t, g, SkillDraftsCmd(), "promote", "evskill", "sdr_1", "--version", "1.2.0")
	if err != nil || !strings.Contains(out, "1.1.1") || !strings.Contains(out, "in_review") {
		t.Fatalf("promote: %v\n%s", err, out)
	}
	if b := g.body("POST /drafts/sdr_1/promote"); b["version"] != "1.2.0" {
		t.Errorf("promote body = %v", b)
	}
	if out, err = evalCmd(t, g, SkillDraftsCmd(), "discard", "evskill", "sdr_1"); err != nil || !strings.Contains(out, "Discarded draft sdr_1") {
		t.Fatalf("discard: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"list", "evskill"}, {"show", "evskill", "sdr_1"}, {"promote", "evskill", "sdr_1"}, {"discard", "evskill", "sdr_1"}} {
		out, err := evalCmd(t, g, SkillDraftsCmd(), append(args, "--json")...)
		var v any
		if err != nil || json.Unmarshal([]byte(out), &v) != nil {
			t.Errorf("%v --json: %v\n%s", args, err, out)
		}
	}
}

func TestQuality_SkillAndDashboard(t *testing.T) {
	g := newEvalGateway(t)
	out, err := evalCmd(t, g, SkillQualityCmd(), "evskill")
	if err != nil || !strings.Contains(out, "quality: 82/100") || !strings.Contains(out, "1.1.0") {
		t.Fatalf("quality: %v\n%s", err, out)
	}
	out, err = evalCmd(t, g, SkillQualityCmd(), "--all")
	if err != nil || !strings.Contains(out, "evskill") || !strings.Contains(out, "40") || !strings.Contains(out, "fresh") {
		t.Fatalf("--all: %v\n%s", err, out)
	}
	if _, err = evalCmd(t, g, SkillQualityCmd()); err == nil {
		t.Error("no skill and no --all was accepted")
	}
	out, err = evalCmd(t, g, SkillQualityCmd(), "evskill", "--json")
	var v map[string]any
	if err != nil || json.Unmarshal([]byte(out), &v) != nil || v["score"] == nil {
		t.Errorf("--json: %v\n%s", err, out)
	}
}

func TestEvalAPI_AgentSessionRefusalsPointToThePersonalProfile(t *testing.T) {
	g := newEvalGateway(t)
	t.Setenv("RELAY_AGENT", "1")
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		g.refuse["POST /evals/sets"] = status
		_, err := evalCmd(t, g, SkillEvalsCmd(), "sets", "create", "evskill", "--name", "x")
		if err == nil || !strings.Contains(err.Error(), "agent session") || !strings.Contains(err.Error(), "unset RELAY_AGENT") {
			t.Errorf("status %d: %v", status, err)
		}
	}
	// Reads stay open to the agent profile.
	if _, err := evalCmd(t, g, SkillEvalsCmd(), "sets", "list", "evskill"); err != nil {
		t.Errorf("read: %v", err)
	}
}

func TestEvalAPI_GatewayErrorsExplainThemselves(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"not enabled": {503, `{"error":"Hosted runtime not enabled","code":"NOT_ENABLED"}`, "hosted runtime"},
		"unmet":       {409, `{"error":"Connectors missing","code":"SKILL_REQUIREMENTS_UNMET","missing":["clockify"]}`, "connect clockify"},
		"validation":  {400, `{"error":"Validation failed","code":"VALIDATION_ERROR","issues":[{"path":["name"],"message":"Required"}]}`, "name: Required"},
		"limit":       {422, `{"error":"Too many calls","code":"EVAL_LIMIT"}`, "Too many calls"},
	}
	api := openEvalAPI(t, newEvalGateway(t))
	for name, tc := range cases {
		err := api.errorFor(http.MethodPost, tc.status, []byte(tc.body))
		var ae *apiError
		if !errors.As(err, &ae) || ae.Status != tc.status || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestResolveSet_ByNameIdAndDefault(t *testing.T) {
	api := openEvalAPI(t, newEvalGateway(t))
	if _, err := api.resolveSet(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), `no eval set "missing"`) {
		t.Errorf("unknown set: %v", err)
	}
	if s, err := api.resolveSet(context.Background(), ""); err != nil || s.ID != "evs_1" {
		t.Errorf("only set: %v %v", s, err)
	}
}
