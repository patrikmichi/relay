package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type routeMap = map[string]func(*packServer, http.ResponseWriter, *http.Request)

func automationGateway(t *testing.T, routes routeMap) *packServer {
	t.Helper()
	ps := packGateway(t, routes)
	old := automationPollDelay
	automationPollDelay = time.Millisecond
	t.Cleanup(func() { automationPollDelay = old })
	return ps
}

const (
	automationJSON = `{"id":"atm_k3j2h1","name":"Invoice check","status":"enabled","template":{"slug":"invoice-check","version":"1.2.0"},"autoUpdate":"patch","lastRun":{"id":"atr_a1b2c3","status":"succeeded"}}`
	pausedJSON     = `{"id":"atm_p4u5s6","name":"Weekly","status":"paused","pausedReason":"run_as_left","template":{"slug":"weekly","version":"1.0.0"},"reapprovalNeeded":2}`
)

func TestAutomationList(t *testing.T) {
	var q string
	automationGateway(t, routeMap{
		"GET /api/automations": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"automations":[`+automationJSON+`,`+pausedJSON+`]}`)
		},
		"GET /api/automations/templates": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			q = r.URL.RawQuery
			_, _ = io.WriteString(w, `{"templates":[{"slug":"invoice-check","title":"Invoice check","summary":"Checks invoices","category":"finance","version":"1.2.0","ready":true},{"slug":"x-y-z","title":"T","version":"0.1.0","ready":false},{"slug":"plain","version":"1.0.0"}]}`)
		},
	})
	out, err := runCommand(t, AutomationCmd(), "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"atm_k3j2h1", "invoice-check@1.2.0", "succeeded", "paused: run_as_left 2 step(s) need re-approval"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	out, err = runCommand(t, AutomationCmd(), "list", "--gallery", "--category", "finance", "--service", "slack")
	if err != nil || q != "category=finance&service=slack" {
		t.Fatalf("gallery: %q %q %v", q, out, err)
	}
	for _, want := range []string{"invoice-check", "yes", "needs connections", "checks invoices"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("gallery lacks %q:\n%s", want, out)
		}
	}
	for _, args := range [][]string{{"list", "--json"}, {"list", "--gallery", "--json"}} {
		if out, err := runCommand(t, AutomationCmd(), args...); err != nil || !strings.Contains(out, "{") {
			t.Errorf("%v: %q %v", args, out, err)
		}
	}
}

func TestAutomationList_Empty(t *testing.T) {
	automationGateway(t, routeMap{
		"GET /api/automations":           jsonReply(200, `{"automations":[]}`),
		"GET /api/automations/templates": jsonReply(200, `{"templates":[]}`),
	})
	if out, err := runCommand(t, AutomationCmd(), "list"); err != nil || !strings.Contains(out, "No automations installed") {
		t.Errorf("%q %v", out, err)
	}
	if out, err := runCommand(t, AutomationCmd(), "list", "--gallery"); err != nil || !strings.Contains(out, "No templates.") {
		t.Errorf("%q %v", out, err)
	}
}

func TestAutomationShow(t *testing.T) {
	var tmplPath string
	automationGateway(t, routeMap{
		"GET /api/automations/atm_k3j2h1": jsonReply(200, `{"automation":`+automationJSON+`}`),
		"GET /api/automations/atm_k3j2h1/preapprovals": jsonReply(200, `{"preapprovals":[
			{"id":"pre_1","stepPath":"post","status":"active","callsToday":3,"maxCallsPerDay":50,"expiresAt":"2026-11-01"},
			{"id":"pre_2","stepPath":"mail","status":"void","voidReason":"param_changed"}]}`),
		"GET /api/automations/templates/invoice-check/versions/1.2.0": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			tmplPath = r.URL.Path
			_, _ = io.WriteString(w, `{"template":{"slug":"invoice-check","title":"Invoice check","version":"1.2.0","summary":"s","connectors":["slack"],"minimumPlan":"pro",
			 "permissions":[{"service":"slack","tool":"post_message","risk":"outward"}],"preapprovableSteps":["post"]}}`)
		},
	})
	out, err := runCommand(t, AutomationCmd(), "show", "atm_k3j2h1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"atm_k3j2h1 Invoice check (enabled)", "template invoice-check@1.2.0", "auto-update: patch", "3 of 50 calls today", "void (param_changed); re-approve with `zuano automation preapprove atm_k3j2h1 --step mail`"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	out, err = runCommand(t, AutomationCmd(), "show", "invoice-check@1.2.0")
	if err != nil || tmplPath == "" {
		t.Fatalf("%q %v", out, err)
	}
	for _, want := range []string{"Invoice check 1.2.0", "Connections: slack", "pro or higher", "outward", "slack.post_message", "Steps you can pre-approve: post", "zuano automation install invoice-check"} {
		if !strings.Contains(out, want) {
			t.Errorf("template lacks %q:\n%s", want, out)
		}
	}
	if out, err := runCommand(t, AutomationCmd(), "show", "atm_k3j2h1", "--json"); err != nil || !strings.Contains(out, "pre_1") {
		t.Errorf("json: %q %v", out, err)
	}
	if out, err := runCommand(t, AutomationCmd(), "show", "invoice-check@1.2.0", "--json"); err != nil || !strings.Contains(out, "minimumPlan") {
		t.Errorf("template json: %q %v", out, err)
	}
	for _, bad := range []string{"Bad Slug", "atm_"} {
		if _, err := runCommand(t, AutomationCmd(), "show", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAutomationInstall(t *testing.T) {
	ps := automationGateway(t, routeMap{
		"POST /api/automations": jsonReply(201, `{"automation":{"id":"atm_n3w1","name":"Mine"},"wizardUrl":"/automations/atm_n3w1/wizard?step=preapprove"}`),
	})
	out, err := runCommand(t, AutomationCmd(), "install", "invoice-check@1.2.0", "--name", "Mine", "--param", "channel=C1", "--bind", "slack=org", "--run-as", "a@example.com")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b := ps.body("POST /api/automations", 0)
	if b["template"] != "invoice-check" || b["version"] != "1.2.0" || b["name"] != "Mine" || b["runAsEmail"] != "a@example.com" ||
		b["bindings"].(map[string]any)["slack"] != "org" || b["params"].(map[string]any)["channel"] != "C1" {
		t.Errorf("body = %v", b)
	}
	for _, want := range []string{"Created draft atm_n3w1 (Mine)", ps.URL + "/automations/atm_n3w1/wizard?step=preapprove", "zuano automation run atm_n3w1 --dry-run"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if out, err := runCommand(t, AutomationCmd(), "install", "invoice-check", "--json"); err != nil || !strings.Contains(out, "wizardUrl") {
		t.Errorf("json: %q %v", out, err)
	}
	for name, args := range map[string][]string{
		"bad bind": {"install", "invoice-check", "--bind", "slack=both"},
		"bad slug": {"install", "X"},
		"bad file": {"install", "invoice-check", "--params-file", filepath.Join(t.TempDir(), "none")},
	} {
		if _, err := runCommand(t, AutomationCmd(), args...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestAutomationInstall_IncompleteAnswersAreErrors(t *testing.T) {
	for _, body := range []string{`{"automation":{"id":"atm_n3w1"}}`, `{"automation":{"id":"atm_n3w1"},"wizardUrl":"//evil.example/x"}`} {
		automationGateway(t, routeMap{"POST /api/automations": jsonReply(201, body)})
		if _, err := runCommand(t, AutomationCmd(), "install", "invoice-check"); err == nil {
			t.Errorf("%s accepted", body)
		}
	}
}

func runRoutes(status, steps string) routeMap {
	return routeMap{
		"POST /api/automations/atm_k3j2h1/run": jsonReply(202, `{"run_id":"atr_a1b2c3","job_id":"job_1"}`),
		"GET /api/automations/runs/atr_a1b2c3": jsonReply(200, `{"run":{"id":"atr_a1b2c3","status":"`+status+`","mode":"live","error":"boom","steps":[`+steps+`]}}`),
	}
}

func TestAutomationRun_ModesAndInputs(t *testing.T) {
	ps := automationGateway(t, runRoutes("running", ""))
	out, err := runCommand(t, AutomationCmd(), "run", "atm_k3j2h1", "--dry-run", "--sample", "invoice-created")
	if err != nil || !strings.Contains(out, "Started atr_a1b2c3. Follow it with `zuano automation logs atr_a1b2c3 --follow`") {
		t.Fatalf("%q %v", out, err)
	}
	if b := ps.body("POST /api/automations/atm_k3j2h1/run", 0); b["mode"] != "dry_run" || b["sample"] != "invoice-created" {
		t.Errorf("dry-run body = %v", b)
	}
	in := filepath.Join(t.TempDir(), "in.json")
	if err := os.WriteFile(in, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, AutomationCmd(), "run", "atm_k3j2h1", "--test", "--yes", "--input", in); err != nil {
		t.Fatal(err)
	}
	if b := ps.body("POST /api/automations/atm_k3j2h1/run", 1); b["mode"] != "test" || b["input"].(map[string]any)["a"] != float64(1) {
		t.Errorf("test body = %v", b)
	}
	if _, err := runCommand(t, AutomationCmd(), "run", "atm_k3j2h1", "--event", "wev_x1y2z3"); err != nil {
		t.Fatal(err)
	}
	if b := ps.body("POST /api/automations/atm_k3j2h1/run", 2); b["mode"] != "live" || b["eventId"] != "wev_x1y2z3" {
		t.Errorf("live body = %v", b)
	}
	if out, err := runCommand(t, AutomationCmd(), "run", "atm_k3j2h1", "--json"); err != nil || !strings.Contains(out, "run_id") {
		t.Errorf("json: %q %v", out, err)
	}
	before := ps.count("POST /api/automations/atm_k3j2h1/run")
	for name, args := range map[string][]string{
		"both modes":   {"run", "atm_k3j2h1", "--dry-run", "--test"},
		"two inputs":   {"run", "atm_k3j2h1", "--sample", "a", "--event", "wev_1"},
		"test no yes":  {"run", "atm_k3j2h1", "--test"},
		"bad id":       {"run", "nope"},
		"bad wait":     {"run", "atm_k3j2h1", "--wait=soon"},
		"missing file": {"run", "atm_k3j2h1", "--input", filepath.Join(t.TempDir(), "none")},
	} {
		if _, err := runCommand(t, AutomationCmd(), args...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("nope"), 0o600)
	if _, err := runCommand(t, AutomationCmd(), "run", "atm_k3j2h1", "--input", bad); err == nil {
		t.Error("a non-JSON input was accepted")
	}
	if ps.count("POST /api/automations/atm_k3j2h1/run") != before {
		t.Error("a run started although the command was refused")
	}
}

func TestAutomationRun_WaitExitCodes(t *testing.T) {
	held := `{"seq":2,"path":"post","status":"held","approvalUrl":"/approvals/apr_1"}`
	for name, c := range map[string]struct {
		status, steps string
		exit          int
		want          string
	}{
		"succeeded": {"succeeded", `{"path":"check","status":"succeeded","preapprovalId":"pre_1"}`, 0, "pre-approved pre_1"},
		"failed":    {"failed", `{"path":"check","status":"failed","error":"bad"}`, 20, "check"},
		"denied":    {"failed", `{"path":"post","status":"denied"}`, 10, "denied"},
		"expired":   {"failed", `{"path":"post","status":"expired"}`, 11, "expired"},
		"held":      {"running", held, 12, "/approvals/apr_1"},
		"cancelled": {"cancelled", ``, 1, "cancelled"},
		"skipped":   {"skipped", ``, 1, "skipped"},
	} {
		automationGateway(t, runRoutes(c.status, c.steps))
		out, err := runCommand(t, AutomationCmd(), "run", "atm_k3j2h1", "--wait")
		if exitCodeOf(err) != c.exit || !strings.Contains(out+errText(err), c.want) {
			t.Errorf("%s: exit %d, %v\n%s", name, exitCodeOf(err), err, out)
		}
	}
	automationGateway(t, runRoutes("running", `{"path":"a","status":"running"}`))
	_, err := runCommand(t, AutomationCmd(), "run", "atm_k3j2h1", "--wait=1ms")
	if exitCodeOf(err) != 12 {
		t.Errorf("timeout: exit %d, %v", exitCodeOf(err), err)
	}
	automationGateway(t, runRoutes("failed", ``))
	if out, err := runCommand(t, AutomationCmd(), "run", "atm_k3j2h1", "--wait", "--json"); exitCodeOf(err) != 20 || !strings.Contains(out, `"status":"failed"`) || !strings.Contains(err.Error(), "boom") {
		t.Errorf("json failed: %d %v\n%s", exitCodeOf(err), err, out)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestAutomationRun_WaitStopsWhenTheContextEnds(t *testing.T) {
	ps := automationGateway(t, runRoutes("running", `{"path":"a","status":"running"}`))
	c, err := approvalsClient("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForRun(ctx, c, "atr_a1b2c3", time.Hour, io.Discard, true); err == nil {
		t.Error("a cancelled wait returned no error")
	}
	_ = ps
}

func TestAutomationLogs(t *testing.T) {
	polls := 0
	var afters []string
	automationGateway(t, routeMap{
		"GET /api/automations/atm_k3j2h1/runs": jsonReply(200, `{"runs":[{"id":"atr_a1b2c3"}]}`),
		"GET /api/automations/runs/atr_a1b2c3": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			afters = append(afters, r.URL.Query().Get("after"))
			polls++
			switch polls {
			case 1:
				_, _ = io.WriteString(w, `{"run":{"id":"atr_a1b2c3","status":"running","mode":"live","seq":1,"steps":[{"seq":1,"path":"check","status":"running"}]}}`)
			case 2:
				_, _ = io.WriteString(w, `{"run":{"id":"atr_a1b2c3","status":"running","seq":2,"steps":[{"seq":2,"path":"check","status":"succeeded"},{"seq":2,"path":"post","status":"held","approvalUrl":"/approvals/apr_9"}]}}`)
			default:
				_, _ = io.WriteString(w, `{"run":{"id":"atr_a1b2c3","status":"succeeded","seq":3,"steps":[{"seq":3,"path":"post","status":"succeeded"}]}}`)
			}
		},
	})
	out, err := runCommand(t, AutomationCmd(), "logs", "atm_k3j2h1", "--follow")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Run atr_a1b2c3 (live): running", "check", "waiting for approval", "/approvals/apr_9"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
	if len(afters) != 3 || afters[0] != "" || afters[1] != "1" || afters[2] != "2" {
		t.Errorf("after = %q", afters)
	}

	polls = 0
	out, err = runCommand(t, AutomationCmd(), "logs", "atr_a1b2c3", "--step", "check")
	if err != nil || strings.Contains(out, "post") || !strings.Contains(out, "check") {
		t.Errorf("single read: %q %v", out, err)
	}
	polls = 5
	if out, err = runCommand(t, AutomationCmd(), "logs", "atr_a1b2c3", "--json"); err != nil || !strings.Contains(out, `"succeeded"`) {
		t.Errorf("json: %q %v", out, err)
	}
	if _, err := runCommand(t, AutomationCmd(), "logs", "nonsense"); err == nil {
		t.Error("a bad id was accepted")
	}
}

func TestAutomationLogs_NoRunsAndFailedFollow(t *testing.T) {
	automationGateway(t, routeMap{"GET /api/automations/atm_k3j2h1/runs": jsonReply(200, `{"runs":[]}`)})
	if out, err := runCommand(t, AutomationCmd(), "logs", "atm_k3j2h1"); err != nil || !strings.Contains(out, "No runs yet.") {
		t.Errorf("%q %v", out, err)
	}
	automationGateway(t, runRoutes("failed", `{"path":"a","status":"failed"}`))
	if _, err := runCommand(t, AutomationCmd(), "logs", "atr_a1b2c3", "--follow"); exitCodeOf(err) != 20 {
		t.Errorf("failed follow: exit %d, %v", exitCodeOf(err), err)
	}
}

func TestAutomationBrowserCommandsOpenPagesAndRefuseAgents(t *testing.T) {
	ps := automationGateway(t, routeMap{})
	out, err := runCommand(t, AutomationCmd(), "enable", "atm_k3j2h1")
	if err != nil || !strings.Contains(out, ps.URL+"/automations/atm_k3j2h1/enable") {
		t.Errorf("enable: %q %v", out, err)
	}
	out, err = runCommand(t, AutomationCmd(), "preapprove", "atm_k3j2h1", "--step", "post")
	if err != nil || !strings.Contains(out, "/automations/atm_k3j2h1/preapprove?step=post") {
		t.Errorf("preapprove: %q %v", out, err)
	}
	if out, err = runCommand(t, AutomationCmd(), "preapprove", "atm_k3j2h1"); err != nil || strings.Contains(out, "step=") {
		t.Errorf("preapprove all: %q %v", out, err)
	}
	if _, err := runCommand(t, AutomationCmd(), "enable", "bad"); err == nil {
		t.Error("bad id accepted")
	}
	t.Setenv("ZUANO_AGENT", "1")
	for _, args := range [][]string{{"enable", "atm_k3j2h1"}, {"preapprove", "atm_k3j2h1"}, {"uninstall", "atm_k3j2h1", "--yes"}} {
		_, err := runCommand(t, AutomationCmd(), args...)
		if exitCodeOf(err) != 14 || !strings.Contains(err.Error(), "passkey") {
			t.Errorf("%v: exit %d, %v", args, exitCodeOf(err), err)
		}
	}
	if len(ps.hits) != 0 {
		t.Errorf("an agent refusal still reached the gateway: %v", ps.hits)
	}
}

func TestAutomationPauseDisableUninstall(t *testing.T) {
	ps := automationGateway(t, routeMap{
		"POST /api/automations/atm_k3j2h1/pause":   jsonReply(200, `{}`),
		"POST /api/automations/atm_k3j2h1/disable": jsonReply(200, `{"ok":true}`),
		"DELETE /api/automations/atm_k3j2h1":       jsonReply(200, `{"ok":true}`),
	})
	for verb, want := range map[string]string{"pause": "paused.", "disable": "disabled."} {
		if out, err := runCommand(t, AutomationCmd(), verb, "atm_k3j2h1"); err != nil || !strings.Contains(out, want) {
			t.Errorf("%s: %q %v", verb, out, err)
		}
	}
	if out, err := runCommand(t, AutomationCmd(), "disable", "atm_k3j2h1", "--json"); err != nil || !strings.Contains(out, `"ok":true`) {
		t.Errorf("json: %q %v", out, err)
	}
	if _, err := runCommand(t, AutomationCmd(), "uninstall", "atm_k3j2h1"); err == nil {
		t.Error("uninstalled without confirmation")
	}
	if out, err := runCommand(t, AutomationCmd(), "uninstall", "atm_k3j2h1", "--yes"); err != nil || !strings.Contains(out, "Uninstalled atm_k3j2h1.") {
		t.Errorf("uninstall: %q %v", out, err)
	}
	if out, err := runCommand(t, AutomationCmd(), "uninstall", "atm_k3j2h1", "--yes", "--json"); err != nil || !strings.Contains(out, `"ok"`) {
		t.Errorf("uninstall json: %q %v", out, err)
	}
	if ps.count("DELETE /api/automations/atm_k3j2h1") != 2 {
		t.Error("the delete was not sent")
	}
}

const patchDiff = `{"diff":{"from":"1.2.0","to":"1.2.1","class":"patch","permissionsAdded":[],"preapprovalsVoided":["pre_1"],"stepsChanged":["check"]}}`

func TestAutomationUpgrade(t *testing.T) {
	var q string
	diff := patchDiff
	ps := automationGateway(t, routeMap{
		"GET /api/automations/atm_k3j2h1/upgrade": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			q = r.URL.RawQuery
			_, _ = io.WriteString(w, diff)
		},
		"POST /api/automations/atm_k3j2h1/upgrade": jsonReply(200, `{}`),
	})
	if _, err := runCommand(t, AutomationCmd(), "upgrade", "atm_k3j2h1"); err == nil {
		t.Error("upgraded without confirmation")
	}
	out, err := runCommand(t, AutomationCmd(), "upgrade", "atm_k3j2h1", "--to", "1.2.1", "--yes")
	if err != nil || q != "to=1.2.1" {
		t.Fatalf("%q %q %v", q, out, err)
	}
	for _, want := range []string{"1.2.0 -> 1.2.1 (patch)", "steps changed: check", "pre-approvals voided: pre_1", "Upgraded to 1.2.1.", "zuano automation preapprove atm_k3j2h1"} {
		if !strings.Contains(out, want) {
			t.Errorf("upgrade lacks %q:\n%s", want, out)
		}
	}
	if b := ps.body("POST /api/automations/atm_k3j2h1/upgrade", 0); b["to"] != "1.2.1" {
		t.Errorf("body = %v", b)
	}
	if out, err := runCommand(t, AutomationCmd(), "upgrade", "atm_k3j2h1", "--yes", "--json"); err != nil || !strings.Contains(out, `"class":"patch"`) {
		t.Errorf("json: %q %v", out, err)
	}

	diff = `{"diff":{"from":"1.2.0","to":"2.0.0","class":"major","permissionsAdded":["slack.delete"],"connectorsAdded":["jira"],"permissionsRemoved":["x.y"]}}`
	posts := ps.count("POST /api/automations/atm_k3j2h1/upgrade")
	out, err = runCommand(t, AutomationCmd(), "upgrade", "atm_k3j2h1", "--yes")
	if err != nil || !strings.Contains(out, "/automations/atm_k3j2h1/upgrade?to=2.0.0") || !strings.Contains(out, "permissions added: slack.delete") || ps.count("POST /api/automations/atm_k3j2h1/upgrade") != posts {
		t.Errorf("major: %q %v", out, err)
	}
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, AutomationCmd(), "upgrade", "atm_k3j2h1"); exitCodeOf(err) != 14 {
		t.Errorf("agent major: exit %d, %v", exitCodeOf(err), err)
	}

	t.Setenv("ZUANO_AGENT", "")
	diff = `{"diff":{}}`
	if out, err := runCommand(t, AutomationCmd(), "upgrade", "atm_k3j2h1"); err != nil || !strings.Contains(out, "Already on the latest version.") {
		t.Errorf("latest: %q %v", out, err)
	}
}

func writeAutomation(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "automation.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const goodManifest = `{"schema":1,"slug":"invoice-check","version":"1.0.0","title":"Invoice check",
"params":[{"key":"channel"}],"trigger":{"kind":"manual"},
"steps":[{"id":"check","kind":"agent"},{"id":"each","kind":"foreach","steps":[{"id":"post","kind":"tool"}]}]}`

func TestAutomationValidate(t *testing.T) {
	ps := automationGateway(t, routeMap{
		"POST /api/automations/templates/validate": jsonReply(200, `{"errors":[],"analysis":{"permissions":[{"service":"slack","tool":"post_message","risk":"outward"}],"minimumPlan":"pro","preapprovableSteps":["post"]}}`),
	})
	dir := writeAutomation(t, goodManifest)
	out, err := runCommand(t, AutomationCmd(), "validate", dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"invoice-check 1.0.0 is valid.", "outward", "slack.post_message", "pro plan or higher", "can be pre-approved: post"} {
		if !strings.Contains(out, want) {
			t.Errorf("validate lacks %q:\n%s", want, out)
		}
	}
	if m := ps.body("POST /api/automations/templates/validate", 0)["manifest"].(map[string]any); m["slug"] != "invoice-check" {
		t.Errorf("manifest = %v", m)
	}
	if out, err := runCommand(t, AutomationCmd(), "validate", dir, "--json"); err != nil || !strings.Contains(out, "analysis") {
		t.Errorf("json: %q %v", out, err)
	}
}

func TestAutomationValidate_LocalProblemsNeverReachTheGateway(t *testing.T) {
	ps := automationGateway(t, routeMap{})
	bad := `{"schema":2,"slug":"X","version":"v1","steps":[],"trigger":{"kind":"cron"},"params":[{"key":"api_key"}]}`
	_, err := runCommand(t, AutomationCmd(), "validate", writeAutomation(t, bad))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"schema must be 1", "slug", "must be semver", "title is required", "trigger.kind", "at least one step", "looks like a secret"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	dup := `{"schema":1,"slug":"abc","version":"1.0.0","title":"t","trigger":{"kind":"manual"},"steps":[{"id":"a","kind":"tool"},{"id":"a","kind":"nope"},{"id":"Bad","kind":"tool"}]}`
	_, err = runCommand(t, AutomationCmd(), "validate", writeAutomation(t, dup))
	if err == nil || !strings.Contains(err.Error(), "used twice") || !strings.Contains(err.Error(), "unknown kind") || !strings.Contains(err.Error(), "lower_snake_case") {
		t.Errorf("steps: %v", err)
	}
	if _, err := runCommand(t, AutomationCmd(), "validate", writeAutomation(t, "[1]")); err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Errorf("array: %v", err)
	}
	if _, err := runCommand(t, AutomationCmd(), "validate", t.TempDir()); err == nil {
		t.Error("a directory without automation.json was valid")
	}
	big := writeAutomation(t, `{"x":"`+strings.Repeat("a", maxAutomationManifest)+`"}`)
	if _, err := runCommand(t, AutomationCmd(), "validate", big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("big: %v", err)
	}
	if len(ps.hits) != 0 {
		t.Errorf("the gateway was asked: %v", ps.hits)
	}
}

func TestAutomationValidate_GatewayErrors(t *testing.T) {
	automationGateway(t, routeMap{
		"POST /api/automations/templates/validate": jsonReply(200, `{"errors":[{"path":"steps[0].service","message":"unknown connector"}]}`),
	})
	if _, err := runCommand(t, AutomationCmd(), "validate", writeAutomation(t, goodManifest)); err == nil || !strings.Contains(err.Error(), "steps[0].service: unknown connector") {
		t.Errorf("%v", err)
	}
}

func TestAutomationPublish(t *testing.T) {
	var fields map[string]string
	var manifest string
	automationGateway(t, routeMap{
		"POST /api/automations/templates/validate": jsonReply(200, `{"errors":[]}`),
		"POST /api/marketplace/publish": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			fields = map[string]string{"type": r.FormValue("type"), "channel": r.FormValue("channel"), "slug": r.FormValue("slug")}
			f, _, _ := r.FormFile("manifest")
			raw, _ := io.ReadAll(f)
			manifest = string(raw)
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"resourceId":"res_1","versionId":"rv_1","semver":"1.0.0","state":"in_review","channel":"beta"}`)
		},
	})
	out, err := runCommand(t, AutomationCmd(), "publish", writeAutomation(t, goodManifest), "--channel", "beta")
	if err != nil || !strings.Contains(out, "Published successfully") {
		t.Fatalf("%q %v", out, err)
	}
	if fields["type"] != "automation" || fields["channel"] != "beta" || fields["slug"] != "invoice-check" || !strings.Contains(manifest, `"schema":1`) {
		t.Errorf("fields = %v manifest = %s", fields, manifest)
	}
	if out, err := runCommand(t, AutomationCmd(), "publish", writeAutomation(t, goodManifest), "--json"); err != nil || !strings.Contains(out, `"versionId":"rv_1"`) {
		t.Errorf("json: %q %v", out, err)
	}
	if _, err := runCommand(t, AutomationCmd(), "publish", writeAutomation(t, goodManifest), "--channel", "nightly"); err == nil {
		t.Error("an unknown channel was accepted")
	}
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, AutomationCmd(), "publish", writeAutomation(t, goodManifest)); exitCodeOf(err) != 10 {
		t.Errorf("agent: exit %d, %v", exitCodeOf(err), err)
	}
}

func TestAutomationPublish_Refusals(t *testing.T) {
	automationGateway(t, routeMap{
		"POST /api/automations/templates/validate": jsonReply(200, `{"errors":[]}`),
		"POST /api/marketplace/publish":            jsonReply(409, `{"error":"already published"}`),
	})
	if _, err := runCommand(t, AutomationCmd(), "publish", writeAutomation(t, goodManifest)); err == nil {
		t.Error("a conflict was reported as success")
	}
	if out, err := runCommand(t, AutomationCmd(), "publish", writeAutomation(t, goodManifest), "--json"); err == nil || !strings.Contains(out, "already published") {
		t.Errorf("json conflict: %q %v", out, err)
	}
}

func TestDoctorAutomations(t *testing.T) {
	var body string
	status := 200
	ps := automationGateway(t, routeMap{})
	ps.replace(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	c, err := approvalsClient("")
	if err != nil {
		t.Fatal(err)
	}
	check := func() (bool, string) {
		var ok bool
		var d string
		doctorAutomations(context.Background(), c, func(name string, o bool, detail string) { ok, d = o, detail })
		return ok, d
	}
	body = `{"automations":[` + automationJSON + `]}`
	if ok, d := check(); !ok || !strings.Contains(d, "1 automation(s), none paused") {
		t.Errorf("healthy: %v %q", ok, d)
	}
	body = `{"automations":[` + automationJSON + `,` + pausedJSON + `]}`
	if ok, d := check(); ok || !strings.Contains(d, "atm_p4u5s6 (run_as_left)") || !strings.Contains(d, "automation show atm_p4u5s6") {
		t.Errorf("paused: %v %q", ok, d)
	}
	body = `{"automations":[{"id":"atm_r3a4p5","status":"enabled","reapprovalNeeded":1,"template":{}}]}`
	if ok, d := check(); ok || !strings.Contains(d, "atm_r3a4p5") {
		t.Errorf("reapproval: %v %q", ok, d)
	}
	status = 404
	if ok, d := check(); !ok || !strings.Contains(d, "not available") {
		t.Errorf("unavailable: %v %q", ok, d)
	}
}
