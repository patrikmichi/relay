package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodPolicy = `version: 1
org: acme
risk:
  matrix:
    stripe: { write: approval }
`

func policyDir(t *testing.T, content string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pol")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const planJSON = `{"plan":{"planId":"pln_1","parentId":"rel_9","classification":"mixed","changes":[{"section":"risk","op":"added","path":"risk.matrix.stripe","file":"policy.yaml","line":4}],
"impact":[{"service":"stripe","risk":"write","from":"off","to":"approval","identities":3}],"loosening":["x"],"url":"https://gw/policy/plans/pln_1"}}`

const noChangePlan = `{"plan":{"planId":"pln_2","parentId":"rel_9","classification":"neutral","changes":[]}}`

func policyGateway(t *testing.T, routes routeMap) *packServer {
	t.Helper()
	ps := packGateway(t, routes)
	old := policyPollDelay
	policyPollDelay = time.Millisecond
	t.Cleanup(func() { policyPollDelay = old })
	return ps
}

func TestPolicyInit(t *testing.T) {
	ps := policyGateway(t, routeMap{"GET /api/policy/document": func(_ *packServer, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "version: 1\norg: live\n")
	}})
	root := t.TempDir()
	dir := filepath.Join(root, "p")
	out, err := runCommand(t, PolicyCmd(), "init", "--dir", dir)
	if err != nil || !strings.Contains(out, "Wrote") {
		t.Fatalf("%q %v", out, err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "policy.yaml"))
	if !strings.HasPrefix(string(got), "# yaml-language-server: $schema="+ps.URL+"/schemas/policy/v1.json\nversion: 1\norg: my-org") {
		t.Errorf("policy.yaml = %q", got)
	}
	if _, err := runCommand(t, PolicyCmd(), "init", "--dir", dir); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second init: %v", err)
	}
	live := filepath.Join(root, "live")
	if _, err := runCommand(t, PolicyCmd(), "init", "--dir", live, "--from-live"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(live, "policy.yaml")); !strings.Contains(string(got), "org: live") {
		t.Errorf("from live = %q", got)
	}
	if _, err := runCommand(t, PolicyCmd(), "init", "--dir", filepath.Join(root, "x"), "--ci", "bitbucket"); err == nil {
		t.Error("an unknown CI provider was accepted")
	}
}

func TestPolicyInit_WritesAPipelineInTheWorkingDirectory(t *testing.T) {
	policyGateway(t, routeMap{})
	work := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	out, err := runCommand(t, PolicyCmd(), "init", "--dir", "pol", "--ci", "github")
	if err != nil || !strings.Contains(out, ".github/workflows/zuano-policy.yml") || !strings.Contains(out, "ZUANO_URL") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(work, ".github", "workflows", "zuano-policy.yml")); err != nil {
		t.Error(err)
	}
	if _, err := runCommand(t, PolicyCmd(), "init", "--dir", "pol2", "--ci", "github"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("existing pipeline: %v", err)
	}
	if _, err := runCommand(t, PolicyCmd(), "init", "--dir", "pol3", "--ci", "gitlab"); err != nil {
		t.Error(err)
	}
}

func TestPolicyFmt(t *testing.T) {
	policyGateway(t, routeMap{})
	dir := policyDir(t, "version: 1\norg:    acme\nrisk:\n      matrix:\n            stripe: {write: approval}\n")
	if out, err := runCommand(t, PolicyCmd(), "fmt", "--dir", dir, "--check"); exitCodeOf(err) != 1 || !strings.Contains(out, "policy.yaml") {
		t.Errorf("check: exit %d %q %v", exitCodeOf(err), out, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "policy.yaml")); !strings.Contains(string(raw), "org:    acme") {
		t.Error("--check changed the file")
	}
	if out, err := runCommand(t, PolicyCmd(), "fmt", "--dir", dir); err != nil || !strings.Contains(out, "policy.yaml") {
		t.Errorf("fmt: %q %v", out, err)
	}
	if out, err := runCommand(t, PolicyCmd(), "fmt", "--dir", dir, "--check"); err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("formatted: %q %v", out, err)
	}
	bad := policyDir(t, "a: [\n")
	if _, err := runCommand(t, PolicyCmd(), "fmt", "--dir", bad); exitCodeOf(err) != 40 {
		t.Errorf("syntax: %v", err)
	}
	if _, err := runCommand(t, PolicyCmd(), "fmt", "--dir", filepath.Join(t.TempDir(), "none")); exitCodeOf(err) != 40 {
		t.Errorf("missing: %v", err)
	}
}

func TestPolicyValidate(t *testing.T) {
	ps := policyGateway(t, routeMap{"POST /api/policy/validate": jsonReply(200, `{"valid":true}`)})
	good := policyDir(t, goodPolicy)
	if out, err := runCommand(t, PolicyCmd(), "validate", "--dir", good); err != nil || !strings.Contains(out, "The policy is valid.") {
		t.Errorf("online: %q %v", out, err)
	}
	if b := ps.body("POST /api/policy/validate", 0); len(b["files"].([]any)) != 1 {
		t.Errorf("body = %v", b)
	}
	if out, err := runCommand(t, PolicyCmd(), "validate", "--dir", good, "--offline"); err != nil || !strings.Contains(out, "match the schema") || ps.count("POST /api/policy/validate") != 1 {
		t.Errorf("offline: %q %v", out, err)
	}
	bad := policyDir(t, "version: 1\norg: X\nstray: 1\n")
	out, err := runCommand(t, PolicyCmd(), "validate", "--dir", bad)
	if exitCodeOf(err) != 40 || !strings.Contains(out, "policy.yaml:2:") || !strings.Contains(out, "policy.yaml:3: stray: unknown property") {
		t.Errorf("invalid: exit %d %q %v", exitCodeOf(err), out, err)
	}
	if _, err := runCommand(t, PolicyCmd(), "validate", "--dir", filepath.Join(t.TempDir(), "none")); exitCodeOf(err) != 40 {
		t.Errorf("missing: %v", err)
	}
	ps2 := policyGateway(t, routeMap{"POST /api/policy/validate": jsonReply(422, `{"error":"policy_invalid","errors":[{"code":"unknown_service","message":"unknown service nope","file":"policy.yaml","line":4}]}`)})
	_ = ps2
	out, err = runCommand(t, PolicyCmd(), "validate", "--dir", good)
	if exitCodeOf(err) != 40 || !strings.Contains(out, "policy.yaml:4: unknown service nope") {
		t.Errorf("online invalid: exit %d %q %v", exitCodeOf(err), out, err)
	}
	SetOffline(true)
	t.Cleanup(func() { SetOffline(false) })
	if out, err := runCommand(t, PolicyCmd(), "validate", "--dir", good); err != nil || !strings.Contains(out, "match the schema") {
		t.Errorf("--offline root flag: %q %v", out, err)
	}
}

func TestPolicyValidate_WithoutAGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZUANO_URL", "")
	t.Setenv("ZUANO_API_KEY", "")
	out, err := runCommand(t, PolicyCmd(), "validate", "--dir", policyDir(t, goodPolicy))
	if err != nil || !strings.Contains(out, "No gateway is configured") {
		t.Errorf("%q %v", out, err)
	}
}

func TestPolicyPlan(t *testing.T) {
	ps := policyGateway(t, routeMap{"POST /api/policy/plan": jsonReply(200, planJSON), "GET /api/policy/document": jsonReply(200, "x: 1\n")})
	dir := policyDir(t, goodPolicy)
	out, err := runCommand(t, PolicyCmd(), "plan", "--dir", dir)
	if err != nil || !strings.Contains(out, "Policy plan (mixed): 1 change(s)") || !strings.Contains(out, "+ risk.matrix.stripe  policy.yaml:4") {
		t.Fatalf("table: %q %v", out, err)
	}
	if b := ps.body("POST /api/policy/plan", 0); b["base"] != "live" || len(b["files"].([]any)) != 1 {
		t.Errorf("body = %v", b)
	}
	if out, err := runCommand(t, PolicyCmd(), "plan", "--dir", dir, "--format", "markdown"); err != nil || !strings.Contains(out, "### Policy plan: mixed") {
		t.Errorf("markdown: %q %v", out, err)
	}
	if out, err := runCommand(t, PolicyCmd(), "plan", "--dir", dir, "--format", "json"); err != nil || !strings.Contains(out, `"planId":"pln_1"`) {
		t.Errorf("json: %q %v", out, err)
	}
	outFile := filepath.Join(t.TempDir(), "plan.md")
	if out, err := runCommand(t, PolicyCmd(), "plan", "--dir", dir, "--format", "markdown", "--out", outFile); err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("out: %q %v", out, err)
	}
	if raw, _ := os.ReadFile(outFile); !strings.Contains(string(raw), "Policy plan") {
		t.Errorf("file = %q", raw)
	}
	if _, err := runCommand(t, PolicyCmd(), "plan", "--dir", dir, "--fail-on-loosening"); exitCodeOf(err) != 1 {
		t.Errorf("fail-on-loosening: %v", err)
	}
	if _, err := runCommand(t, PolicyCmd(), "plan", "--dir", dir, "--detailed-exitcode"); exitCodeOf(err) != 2 {
		t.Errorf("detailed: %v", err)
	}
	if _, err := runCommand(t, PolicyCmd(), "plan", "--dir", dir, "--format", "xml"); err == nil {
		t.Error("bad format accepted")
	}
	if _, err := runCommand(t, PolicyCmd(), "plan", "--dir", policyDir(t, "version: 2\norg: acme\n")); exitCodeOf(err) != 40 {
		t.Errorf("local invalid: %v", err)
	}
}

func TestPolicyPlan_NoChangesAndServerRefusals(t *testing.T) {
	policyGateway(t, routeMap{"POST /api/policy/plan": jsonReply(200, noChangePlan)})
	dir := policyDir(t, goodPolicy)
	if out, err := runCommand(t, PolicyCmd(), "plan", "--dir", dir, "--detailed-exitcode"); err != nil || !strings.Contains(out, "No changes") {
		t.Errorf("no changes: %q %v", out, err)
	}
	policyGateway(t, routeMap{"POST /api/policy/plan": jsonReply(422, `{"error":"policy_invalid","errors":[{"message":"secret in policy","file":"policy.yaml","line":2}]}`)})
	out, err := runCommand(t, PolicyCmd(), "plan", "--dir", dir)
	if exitCodeOf(err) != 40 || !strings.Contains(out, "policy.yaml:2: secret in policy") {
		t.Errorf("invalid: %d %q %v", exitCodeOf(err), out, err)
	}
	policyGateway(t, routeMap{"POST /api/policy/plan": jsonReply(200, `{}`)})
	if _, err := runCommand(t, PolicyCmd(), "plan", "--dir", dir); err == nil {
		t.Error("an empty plan was accepted")
	}
	policyGateway(t, routeMap{"POST /api/policy/plan": jsonReply(402, `{"error":"plan_required","message":"needs Expert"}`)})
	if _, err := runCommand(t, PolicyCmd(), "plan", "--dir", dir); exitCodeOf(err) != 30 {
		t.Errorf("402: %v", err)
	}
}

func applyRoutes2(applyStatus int, applyBody string) routeMap {
	return routeMap{
		"POST /api/policy/plan":  jsonReply(200, planJSON),
		"POST /api/policy/apply": jsonReply(applyStatus, applyBody),
	}
}

func TestPolicyApply(t *testing.T) {
	ps := policyGateway(t, applyRoutes2(200, `{"release":{"id":"rel_10"}}`))
	dir := policyDir(t, goodPolicy)
	out, err := runCommand(t, PolicyCmd(), "apply", "--dir", dir, "--auto-approve")
	if err != nil || !strings.Contains(out, "Applied as release rel_10.") {
		t.Fatalf("%q %v", out, err)
	}
	b := ps.body("POST /api/policy/apply", 0)
	if b["expectedParentId"] != "rel_9" || b["planId"] != "pln_1" || len(b["files"].([]any)) != 1 {
		t.Errorf("body = %v", b)
	}
	if _, err := runCommand(t, PolicyCmd(), "apply", "--dir", dir); err == nil {
		t.Error("applied without confirmation")
	}
	cmd := PolicyCmd()
	cmd.SetIn(strings.NewReader("y\n"))
	if out, err := runCommand(t, cmd, "apply", "--dir", dir); err != nil || !strings.Contains(out, "Apply this policy?") {
		t.Errorf("confirmed: %q %v", out, err)
	}
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, PolicyCmd(), "apply", "--dir", dir, "--auto-approve"); exitCodeOf(err) != 10 {
		t.Errorf("agent: %v", err)
	}
}

func TestPolicyApply_NothingToApplyAndRefusals(t *testing.T) {
	policyGateway(t, routeMap{"POST /api/policy/plan": jsonReply(200, noChangePlan)})
	dir := policyDir(t, goodPolicy)
	if out, err := runCommand(t, PolicyCmd(), "apply", "--dir", dir, "--auto-approve"); err != nil || !strings.Contains(out, "No changes") {
		t.Errorf("%q %v", out, err)
	}
	for name, c := range map[string]struct {
		status int
		body   string
		exit   int
	}{
		"conflict": {409, `{"error":"policy_conflict"}`, 41},
		"git":      {409, `{"error":"policy_managed_in_git"}`, 42},
		"invalid":  {422, `{"error":"policy_invalid","errors":[{"message":"bad"}]}`, 40},
		"other":    {500, `{"error":"boom"}`, 1},
	} {
		policyGateway(t, applyRoutes2(c.status, c.body))
		if _, err := runCommand(t, PolicyCmd(), "apply", "--dir", dir, "--auto-approve"); exitCodeOf(err) != c.exit {
			t.Errorf("%s: exit %d, %v", name, exitCodeOf(err), err)
		}
	}
}

func TestPolicyApply_HeldForApproval(t *testing.T) {
	dir := policyDir(t, goodPolicy)
	status := "pending"
	routes := applyRoutes2(202, `{"pending_apply_id":"pap_1","url":"/admin/policy/pending/pap_1"}`)
	routes["GET /api/policy/pending/pap_1"] = func(_ *packServer, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"pending":{"status":"`+status+`","releaseId":"rel_11"}}`)
	}
	ps := policyGateway(t, routes)

	_, err := runCommand(t, PolicyCmd(), "apply", "--dir", dir, "--auto-approve")
	if exitCodeOf(err) != 12 || !strings.Contains(err.Error(), "apply --resume pap_1") {
		t.Errorf("not waiting: exit %d, %v", exitCodeOf(err), err)
	}
	out, err := runCommand(t, PolicyCmd(), "apply", "--dir", dir, "--auto-approve", "--wait", "1")
	if exitCodeOf(err) != 12 || !strings.Contains(out, ps.URL+"/admin/policy/pending/pap_1") {
		t.Errorf("wait ran out: exit %d %q", exitCodeOf(err), out)
	}
	status = "approved"
	if out, err := runCommand(t, PolicyCmd(), "apply", "--dir", dir, "--auto-approve", "--wait", "5"); err != nil || !strings.Contains(out, "Approved and applied as release rel_11.") {
		t.Errorf("approved: %q %v", out, err)
	}
	status = "denied"
	if _, err := runCommand(t, PolicyCmd(), "apply", "--resume", "pap_1"); exitCodeOf(err) != 10 {
		t.Errorf("denied: %v", err)
	}
	status = "expired"
	if _, err := runCommand(t, PolicyCmd(), "apply", "--resume", "pap_1"); exitCodeOf(err) != 11 {
		t.Errorf("expired: %v", err)
	}
	status = "applied"
	if _, err := runCommand(t, PolicyCmd(), "apply", "--resume", "pap_1"); err != nil {
		t.Errorf("resume: %v", err)
	}
}

func TestPolicyApply_HeldWithoutAnID(t *testing.T) {
	policyGateway(t, applyRoutes2(202, `{}`))
	if _, err := runCommand(t, PolicyCmd(), "apply", "--dir", policyDir(t, goodPolicy), "--auto-approve"); err == nil || !strings.Contains(err.Error(), "did not say which") {
		t.Errorf("%v", err)
	}
}

func TestPolicyStatusDiffReleasesRollbackFreezeExport(t *testing.T) {
	ps := policyGateway(t, routeMap{
		"GET /api/policy/status": jsonReply(200, `{"status":{"management":"git","repo":"acme/policy","branch":"main","activeRelease":{"id":"rel_5","source":"git","appliedAt":"2026-10-01"},
		 "drift":{"kind":"live","message":"changed by break-glass"},"breakGlass":{"actor":"ann@example.com","reason":"incident","expiresAt":"2026-10-02"}}}`),
		"GET /api/policy/releases/rel_5/diff":      jsonReply(200, `{"diff":{"changes":[{"op":"changed","path":"risk.matrix.stripe","file":"policy.yaml","line":4}]}}`),
		"GET /api/policy/releases":                 jsonReply(200, `{"releases":[{"id":"rel_5","source":"git","appliedAt":"2026-10-01","actor":"ci","commit":"abc123","classification":"tightening"}]}`),
		"GET /api/policy/releases/rel_5":           jsonReply(200, `{"release":{"id":"rel_5","source":"git","appliedAt":"2026-10-01","actor":"ci","commit":"abc123","provenance":{"pr":"#12","approvers":["bob"]},"signed":true}}`),
		"POST /api/policy/releases/rel_4/rollback": jsonReply(200, `{"release":{"id":"rel_6"},"revertPr":"https://git/pr/13"}`),
		"POST /api/policy/releases/rel_3/rollback": jsonReply(200, `{"release":{"id":"rel_7"},"hint":"git revert abc123"}`),
		"POST /api/policy/freeze":                  jsonReply(200, `{"ok":true}`),
		"GET /api/policy/document":                 jsonReply(200, `{"version":1}`),
	})
	out, err := runCommand(t, PolicyCmd(), "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Managed in: git (acme/policy@main)", "Active release: rel_5 (git, 2026-10-01)", "Drift: live: changed by break-glass", "Break-glass: ann@example.com until 2026-10-02: incident"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if out, err := runCommand(t, PolicyCmd(), "diff", "--live"); err != nil || !strings.Contains(out, "~ risk.matrix.stripe  policy.yaml:4") {
		t.Errorf("diff: %q %v", out, err)
	}
	if _, err := runCommand(t, PolicyCmd(), "diff"); err == nil {
		t.Error("diff without --live")
	}
	if out, err := runCommand(t, PolicyCmd(), "releases"); err != nil || !strings.Contains(out, "rel_5") || !strings.Contains(out, "tightening") {
		t.Errorf("releases: %q %v", out, err)
	}
	if out, err := runCommand(t, PolicyCmd(), "releases", "list"); err != nil || !strings.Contains(out, "abc123") {
		t.Errorf("releases list: %q %v", out, err)
	}
	if out, err := runCommand(t, PolicyCmd(), "releases", "show", "rel_5"); err != nil || !strings.Contains(out, "pull request #12, approved by bob") || !strings.Contains(out, "signed: true") {
		t.Errorf("show: %q %v", out, err)
	}
	if out, err := runCommand(t, PolicyCmd(), "releases", "diff", "rel_5", "rel_4"); err != nil || !strings.Contains(out, "risk.matrix.stripe") {
		t.Errorf("release diff: %q %v", out, err)
	}
	if got := ps.count("GET /api/policy/releases/rel_5/diff"); got != 2 {
		t.Errorf("diff requests = %d", got)
	}
	for _, args := range [][]string{{"status", "--json"}, {"releases", "--json"}, {"releases", "show", "rel_5", "--json"}, {"releases", "diff", "rel_5", "rel_4", "--json"}, {"diff", "--live", "--json"}} {
		if out, err := runCommand(t, PolicyCmd(), args...); err != nil || !strings.Contains(out, "{") {
			t.Errorf("%v: %q %v", args, out, err)
		}
	}
	if out, err := runCommand(t, PolicyCmd(), "rollback", "rel_4", "--yes"); err != nil || !strings.Contains(out, "rel_6") || !strings.Contains(out, "https://git/pr/13") {
		t.Errorf("rollback: %q %v", out, err)
	}
	if out, err := runCommand(t, PolicyCmd(), "rollback", "rel_3", "--yes"); err != nil || !strings.Contains(out, "git revert abc123") {
		t.Errorf("rollback hint: %q %v", out, err)
	}
	if _, err := runCommand(t, PolicyCmd(), "rollback", "rel_4"); err == nil {
		t.Error("rolled back without confirmation")
	}
	if out, err := runCommand(t, PolicyCmd(), "freeze", "--yes"); err != nil || !strings.Contains(out, "Frozen.") {
		t.Errorf("freeze: %q %v", out, err)
	}
	if out, err := runCommand(t, PolicyCmd(), "export", "--format", "json"); err != nil || !strings.Contains(out, `"version":1`) {
		t.Errorf("export: %q %v", out, err)
	}
	if _, err := runCommand(t, PolicyCmd(), "export", "--format", "toml"); err == nil {
		t.Error("bad export format")
	}
	t.Setenv("ZUANO_AGENT", "1")
	for _, args := range [][]string{{"rollback", "rel_4", "--yes"}, {"freeze", "--yes"}} {
		if _, err := runCommand(t, PolicyCmd(), args...); exitCodeOf(err) != 10 {
			t.Errorf("%v under an agent: %v", args, err)
		}
	}
	if out, err := runCommand(t, PolicyCmd(), "status"); err != nil || out == "" {
		t.Errorf("agent may read status: %q %v", out, err)
	}
}

func TestPolicyExplainShowsWhereTheRuleIsWritten(t *testing.T) {
	policyGateway(t, routeMap{"GET /api/admin/risk-policy/preview": jsonReply(200, `{"service":"salesforce","tool":"update","risk":"write","principal":"mcp_client",
	 "decision":{"mode":"off","source":"org_tool_override","ruleId":"marketing-no-crm-writes","changeableBy":"org_admin","sourceRef":{"path":"agents.rules[marketing-no-crm-writes]","file":"zuano/agents.yaml","line":14}}}`)})
	out, err := runCommand(t, PolicyCmd(), "explain", "salesforce", "update")
	if err != nil || !strings.Contains(out, "decided by agents.rules[marketing-no-crm-writes] (zuano/agents.yaml:14)") {
		t.Errorf("%q %v", out, err)
	}
}

func TestCISignInExchangesTheJobsOIDCToken(t *testing.T) {
	var exchanged map[string]any
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oidc":
			if r.Header.Get("Authorization") != "Bearer reqtoken" || r.URL.Query().Get("audience") == "" {
				w.WriteHeader(401)
				return
			}
			_, _ = io.WriteString(w, `{"value":"gh.jwt.token"}`)
		case "/api/policy/ci/token":
			_ = json.NewDecoder(r.Body).Decode(&exchanged)
			if exchanged["idToken"] == "bad" {
				w.WriteHeader(403)
				_, _ = io.WriteString(w, `{"error":"repository not connected"}`)
				return
			}
			_, _ = io.WriteString(w, `{"token":"workload-token","expiresIn":600}`)
		case "/api/policy/status":
			if r.Header.Get("Authorization") != "Bearer workload-token" {
				w.WriteHeader(401)
				return
			}
			_, _ = io.WriteString(w, `{"status":{"management":"git","activeRelease":{"id":"rel_1"}}}`)
		}
	}))
	t.Cleanup(gw.Close)
	oldHTTP := ciHTTP
	ciHTTP = func() *http.Client { return http.DefaultClient }
	t.Cleanup(func() { ciHTTP = oldHTTP })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZUANO_URL", gw.URL)
	t.Setenv("ZUANO_API_KEY", "")
	t.Setenv("ZUANO_EMAIL", "")
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", gw.URL+"/oidc?x=1")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "reqtoken")
	out, err := runCommand(t, PolicyCmd(), "status")
	if err != nil || !strings.Contains(out, "rel_1") {
		t.Fatalf("github: %q %v", out, err)
	}
	if exchanged["provider"] != "github" || exchanged["idToken"] != "gh.jwt.token" {
		t.Errorf("exchange = %v", exchanged)
	}

	t.Setenv("ZUANO_API_KEY", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("GITLAB_CI", "true")
	t.Setenv("ZUANO_ID_TOKEN", "bad")
	if _, err := runCommand(t, PolicyCmd(), "status"); err == nil || !strings.Contains(err.Error(), "did not accept this job's OIDC token") || !strings.Contains(err.Error(), "repository not connected") {
		t.Errorf("rejected: %v", err)
	}
	t.Setenv("ZUANO_API_KEY", "")
	t.Setenv("ZUANO_ID_TOKEN", "gl.jwt")
	if _, err := runCommand(t, PolicyCmd(), "status"); err != nil || exchanged["provider"] != "gitlab" {
		t.Errorf("gitlab: %v %v", err, exchanged)
	}
}

func TestCISignInWithoutAToken(t *testing.T) {
	policyDirectory := policyDir(t, goodPolicy)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZUANO_URL", "https://gateway.invalid")
	t.Setenv("ZUANO_API_KEY", "")
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("ZUANO_ID_TOKEN", "")
	out, err := runCommand(t, PolicyCmd(), "plan", "--dir", policyDirectory)
	if err != nil || !strings.Contains(out, "Plan skipped: this CI job has no OIDC token") {
		t.Errorf("plan: %q %v", out, err)
	}
	if _, err := runCommand(t, PolicyCmd(), "status"); err == nil || !strings.Contains(err.Error(), "no OIDC token") {
		t.Errorf("status: %v", err)
	}
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("CI", "")
	t.Setenv("GITLAB_CI", "")
	if runningInCI() {
		t.Error("not in CI")
	}
}
