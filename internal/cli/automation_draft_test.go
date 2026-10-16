package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/client"
)

const draftReply = `{"automationId":"atm_dr4ft","customManifestId":"acm_1","wizardUrl":"/automations/atm_dr4ft/wizard","analysis":{"trigger":{"type":"github.issue_opened"},"steps":[{},{}],"derivedPermissions":["github.add_labels (write)","slack.post_message (outward)"],"maxRisk":"outward","preapprovableSteps":["steps[1]"],"missingConnections":["slack"],"minimumPlan":"pro"}}`

func TestAutomationDraftFromAGoalAndAManifest(t *testing.T) {
	ps := automationGateway(t, routeMap{"POST /api/automations/draft": jsonReply(201, draftReply)})
	opened := stubBrowserWith(t, nil)
	manifest := filepath.Join(t.TempDir(), "m.json")
	if err := os.WriteFile(manifest, []byte(`{"trigger_type":"github.issue_opened","steps":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCommand(t, AutomationCmd(), "draft", "Label new issues and post to #triage", "--manifest", manifest, "--name", "Triage", "--param", "channel=C1", "--bind", "slack=org", "--automation-id", "atm_old1", "--no-browser")
	if err != nil {
		t.Fatalf("draft: %v\n%s", err, out)
	}
	b := ps.body("POST /api/automations/draft", 0)
	if b["goal"] != "Label new issues and post to #triage" || b["name"] != "Triage" || b["automationId"] != "atm_old1" ||
		b["manifest"].(map[string]any)["trigger_type"] != "github.issue_opened" || b["params"].(map[string]any)["channel"] != "C1" || b["bindings"].(map[string]any)["slack"] != "org" {
		t.Errorf("body = %v", b)
	}
	for _, want := range []string{"Saved draft atm_dr4ft", "2 step(s), highest risk outward", "needs: github.add_labels (write), slack.post_message (outward)", "steps that can be pre-approved: steps[1]", "connect first: slack", "plan: pro or above", ps.URL + "/automations/atm_dr4ft/wizard"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(*opened) != 0 {
		t.Errorf("--no-browser opened %v", *opened)
	}
	if out, err := runCommand(t, AutomationCmd(), "draft", "a goal", "--json"); err != nil || !strings.Contains(out, "customManifestId") {
		t.Errorf("--json: %v %q", err, out)
	}
	if _, err := runCommand(t, AutomationCmd(), "draft", "a goal", "--no-browser"); err != nil {
		t.Errorf("goal only: %v", err)
	}
}

func TestAutomationDraftPrintsEveryIssueAndExits16(t *testing.T) {
	ps := automationGateway(t, routeMap{"POST /api/automations/draft": jsonReply(422, `{"error":"invalid_manifest","issues":[{"path":"steps[0].tool","code":"unknown_tool","message":"github.add_label does not exist"},{"path":"","code":"no_trigger","message":"the manifest has no trigger"}]}`)})
	out, err := runCommand(t, AutomationCmd(), "draft", "x")
	if exitCodeOf(err) != exitInvalidManifest || !strings.Contains(err.Error(), "2 issue(s); nothing was saved") {
		t.Fatalf("exit %d (%v)", exitCodeOf(err), err)
	}
	for _, want := range []string{"steps[0].tool: github.add_label does not exist (unknown_tool)", "manifest: the manifest has no trigger (no_trigger)"} {
		if !strings.Contains(out, want) {
			t.Errorf("issues lack %q:\n%s", want, out)
		}
	}
	if _, err := runCommand(t, AutomationCmd(), "draft", "x", "--json"); exitCodeOf(err) != exitInvalidManifest {
		t.Errorf("--json: exit %d", exitCodeOf(err))
	}
	ps.replace(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":"generation_failed","message":"The model did not return a manifest twice."}`))
	})
	if _, err := runCommand(t, AutomationCmd(), "draft", "x"); err == nil || exitCodeOf(err) == exitInvalidManifest || !strings.Contains(err.Error(), "did not return a manifest") {
		t.Errorf("generation_failed: %v", err)
	}
}

func TestAutomationDraftRefusalsAndHolds(t *testing.T) {
	ps := automationGateway(t, routeMap{"POST /api/automations/draft": jsonReply(402, `{"error":"plan_required","feature":"llm.platform_model","message":"Connect a model key to describe an automation."}`)})
	if _, err := runCommand(t, AutomationCmd(), "draft", "x"); exitCodeOf(err) != client.ExitPlanRequired {
		t.Errorf("plan_required: exit %d (%v)", exitCodeOf(err), err)
	}
	ps.replace(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
	})
	if _, err := runCommand(t, AutomationCmd(), "draft", "x"); exitCodeOf(err) != client.ExitRateLimited {
		t.Errorf("rate_limited: exit %d (%v)", exitCodeOf(err), err)
	}
	ps.replace(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"status":"pending_approval","approval_id":"apr_1","service":"automations","tool":"automation_draft","risk":"write","expires_at":"2026-10-16T13:00:00Z"}`))
	})
	t.Setenv("ZUANO_AGENT", "1")
	out, err := runCommand(t, AutomationCmd(), "draft", "x")
	if exitCodeOf(err) != exitPending || !strings.Contains(out+err.Error(), "apr_1") {
		t.Errorf("held: exit %d (%v)\n%s", exitCodeOf(err), err, out)
	}
}

func TestAutomationDraftChecksItsInput(t *testing.T) {
	automationGateway(t, routeMap{"POST /api/automations/draft": jsonReply(201, draftReply)})
	dir := t.TempDir()
	notObject := filepath.Join(dir, "a.json")
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(notObject, []byte(`[1]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, []byte(`{"a":"`+strings.Repeat("x", maxDraftManifest)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"nothing":       {"draft"},
		"empty goal":    {"draft", "  "},
		"missing file":  {"draft", "--manifest", filepath.Join(dir, "none.json")},
		"not an object": {"draft", "--manifest", notObject},
		"too large":     {"draft", "--manifest", big},
		"bad id":        {"draft", "x", "--automation-id", "nope"},
		"bad bind":      {"draft", "x", "--bind", "slack=both"},
		"bad param":     {"draft", "x", "--param", "novalue"},
	} {
		if _, err := runCommand(t, AutomationCmd(), args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAutomationValidateAcceptsTypedTriggers(t *testing.T) {
	ps := automationGateway(t, routeMap{
		"POST /api/automations/templates/validate": jsonReply(200, `{"errors":[],"analysis":{"permissions":[],"minimumPlan":"free","preapprovableSteps":[]}}`),
	})
	typed := func(trigger string) string {
		return `{"schema":1,"slug":"issue-triage","version":"1.0.0","title":"Triage","trigger":` + trigger + `,"steps":[{"id":"label","kind":"tool"}]}`
	}
	if out, err := runCommand(t, AutomationCmd(), "validate", writeAutomation(t, typed(`{"kind":"trigger_type","type":"github.issue_opened","config":{"owner":"{{ params.owner }}","repo":"web"}}`))); err != nil {
		t.Fatalf("typed trigger: %v\n%s", err, out)
	}
	for name, trigger := range map[string]string{
		"no type":    `{"kind":"trigger_type","config":{}}`,
		"bad config": `{"kind":"trigger_type","type":"github.push","config":"x"}`,
	} {
		if _, err := runCommand(t, AutomationCmd(), "validate", writeAutomation(t, typed(trigger))); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if ps.count("POST /api/automations/templates/validate") != 1 {
		t.Errorf("gateway requests = %d, want only the valid manifest's", ps.count("POST /api/automations/templates/validate"))
	}
}
