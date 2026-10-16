package cli

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDLPPolicyGet(t *testing.T) {
	packGateway(t, routeMap{"GET /api/admin/dlp/policy": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") == "yaml" {
			_, _ = io.WriteString(w, "apiVersion: policy.zuano.ai/v1\nkind: DlpPolicy\n")
			return
		}
		_, _ = io.WriteString(w, `{"policy":{"version":4,"spec":{"mode":"monitor","rules":[{},{}]}}}`)
	}})
	out, err := runCommand(t, DLPCmd(), "policy", "get")
	if err != nil || !strings.Contains(out, "version 4: mode monitor, 2 rule(s)") {
		t.Errorf("get: %q %v", out, err)
	}
	if out, err := runCommand(t, DLPCmd(), "policy", "get", "--yaml"); err != nil || !strings.HasPrefix(out, "apiVersion:") {
		t.Errorf("yaml: %q %v", out, err)
	}
	if out, err := runCommand(t, DLPCmd(), "policy", "get", "--json"); err != nil || !strings.Contains(out, `"version":4`) {
		t.Errorf("json: %q %v", out, err)
	}
}

func TestDLPPolicyApply(t *testing.T) {
	var gotType, gotBody, gotQuery string
	ps := packGateway(t, routeMap{"PUT /api/admin/dlp/policy": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotType, gotBody, gotQuery = r.Header.Get("Content-Type"), string(raw), r.URL.RawQuery
		if r.URL.Query().Get("dryRun") == "true" {
			_, _ = io.WriteString(w, `{"diff":{"changes":[{"op":"added","path":"spec.rules.0","file":"p.yaml","line":9}]},"impact":{"days":7,"wouldBlock":2,"wouldHold":1,"wouldMask":0,"wouldTokenize":4,"byRule":[{"rule":"no-iban","count":2}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"version":5}`)
	}})
	dir := t.TempDir()
	file := filepath.Join(dir, "dlp.yaml")
	_ = os.WriteFile(file, []byte("kind: DlpPolicy\n"), 0o600)
	out, err := runCommand(t, DLPCmd(), "policy", "apply", file, "--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"+ spec.rules.0  p.yaml:9", "blocked 2, held 1, masked 0 and tokenized 4", "no-iban: 2", "[dry-run] nothing was changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, out)
		}
	}
	if gotType != "application/yaml" || gotBody != "kind: DlpPolicy\n" || gotQuery != "dryRun=true" {
		t.Errorf("request = %q %q %q", gotType, gotBody, gotQuery)
	}
	if out, err := runCommand(t, DLPCmd(), "policy", "apply", file); err != nil || !strings.Contains(out, "now version 5") {
		t.Errorf("apply: %q %v", out, err)
	}
	jf := filepath.Join(dir, "dlp.JSON")
	_ = os.WriteFile(jf, []byte(`{"kind":"DlpPolicy"}`), 0o600)
	if _, err := runCommand(t, DLPCmd(), "policy", "apply", jf); err != nil || gotType != "application/json" {
		t.Errorf("json file: %v %q", err, gotType)
	}
	if out, err := runCommand(t, DLPCmd(), "policy", "apply", file, "--json"); err != nil || !strings.Contains(out, `"version":5`) {
		t.Errorf("json: %q %v", out, err)
	}
	if _, err := runCommand(t, DLPCmd(), "policy", "apply", filepath.Join(dir, "none")); err == nil {
		t.Error("a missing file was applied")
	}
	t.Setenv("ZUANO_AGENT", "1")
	before := ps.count("PUT /api/admin/dlp/policy")
	if _, err := runCommand(t, DLPCmd(), "policy", "apply", file); exitCodeOf(err) != 10 {
		t.Errorf("agent apply: %v", err)
	}
	if _, err := runCommand(t, DLPCmd(), "policy", "apply", file, "--dry-run"); err != nil {
		t.Errorf("agent dry run: %v", err)
	}
	if ps.count("PUT /api/admin/dlp/policy") != before+1 {
		t.Error("an agent's apply reached the gateway")
	}
}

func TestDLPPolicyApply_RefusalsAndTooBig(t *testing.T) {
	packGateway(t, routeMap{"PUT /api/admin/dlp/policy": jsonReply(422, `{"error":"policy_invalid","errors":[{"message":"unknown detector","file":"dlp.yaml","line":3}]}`)})
	dir := t.TempDir()
	file := filepath.Join(dir, "dlp.yaml")
	_ = os.WriteFile(file, []byte("x: 1\n"), 0o600)
	out, err := runCommand(t, DLPCmd(), "policy", "apply", file)
	if exitCodeOf(err) != 40 || !strings.Contains(out, "dlp.yaml:3: unknown detector") {
		t.Errorf("invalid: %d %q %v", exitCodeOf(err), out, err)
	}
	big := filepath.Join(dir, "big.yaml")
	_ = os.WriteFile(big, []byte(strings.Repeat("a", 1<<20+1)), 0o600)
	if _, err := runCommand(t, DLPCmd(), "policy", "apply", big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("big: %v", err)
	}
}

func TestDLPPolicyVersions(t *testing.T) {
	packGateway(t, routeMap{"GET /api/admin/dlp/policy/versions": jsonReply(200, `{"versions":[{"version":5,"createdAt":"2026-10-15","actor":"ann@example.com","mode":"enforce","source":"git"},{"version":4}]}`)})
	out, err := runCommand(t, DLPCmd(), "policy", "versions")
	if err != nil || !strings.Contains(out, "enforce") || !strings.Contains(out, "ann@example.com") {
		t.Errorf("%q %v", out, err)
	}
	if out, err := runCommand(t, DLPCmd(), "policy", "versions", "--json"); err != nil || !strings.Contains(out, `"versions"`) {
		t.Errorf("json: %q %v", out, err)
	}
}

func TestDLPTestReadsStandardInput(t *testing.T) {
	ps := packGateway(t, routeMap{"POST /api/admin/dlp/test": jsonReply(200, `{"findings":[{"class":"pii.financial.iban","confidence":"high","count":2,"masked":"SK31****7541"}],"decision":{"action":"block","rule":"no-financial-to-outward"}}`)})
	cmd := DLPCmd()
	cmd.SetIn(strings.NewReader(`{"to":"x","iban":"SK3112000000198742637541"}`))
	out, err := runCommand(t, cmd, "test", "--service", "gmail", "--tool", "send_email", "--direction", "out")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"pii.financial.iban", "x2", "SK31****7541", "Decision: block by rule no-financial-to-outward"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	b := ps.body("POST /api/admin/dlp/test", 0)
	if b["direction"] != "outbound" || b["service"] != "gmail" || b["tool"] != "send_email" || b["json"] == nil || b["text"] != nil {
		t.Errorf("body = %v", b)
	}
	cmd = DLPCmd()
	cmd.SetIn(strings.NewReader("plain text with an email ann@example.com"))
	if _, err := runCommand(t, cmd, "test", "--direction", "in"); err != nil {
		t.Fatal(err)
	}
	if b := ps.body("POST /api/admin/dlp/test", 1); b["direction"] != "inbound" || b["text"] == nil || b["json"] != nil {
		t.Errorf("text body = %v", b)
	}
	cmd = DLPCmd()
	cmd.SetIn(strings.NewReader("12345"))
	if _, err := runCommand(t, cmd, "test"); err != nil || ps.body("POST /api/admin/dlp/test", 2)["text"] != "12345" {
		t.Errorf("a bare number is text: %v", err)
	}
	for name, in := range map[string]string{"empty": "  \n", "": ""} {
		cmd = DLPCmd()
		cmd.SetIn(strings.NewReader(in))
		if _, err := runCommand(t, cmd, "test"); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Errorf("%q: %v", name, err)
		}
	}
	cmd = DLPCmd()
	cmd.SetIn(strings.NewReader(strings.Repeat("a", maxDLPInput+1)))
	if _, err := runCommand(t, cmd, "test"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("big: %v", err)
	}
	if _, err := runCommand(t, DLPCmd(), "test", "--direction", "sideways"); err == nil {
		t.Error("bad direction")
	}
	cmd = DLPCmd()
	cmd.SetIn(strings.NewReader("x"))
	if out, err := runCommand(t, cmd, "test", "--json"); err != nil || !strings.Contains(out, `"decision"`) {
		t.Errorf("json: %q %v", out, err)
	}
}

func TestDLPTestWithNothingFound(t *testing.T) {
	packGateway(t, routeMap{"POST /api/admin/dlp/test": jsonReply(200, `{"findings":[],"decision":{"action":"allow"}}`)})
	cmd := DLPCmd()
	cmd.SetIn(strings.NewReader("hello"))
	if out, err := runCommand(t, cmd, "test"); err != nil || !strings.Contains(out, "Nothing sensitive found.") || !strings.Contains(out, "Decision: allow") {
		t.Errorf("%q %v", out, err)
	}
}

func TestDLPFindingsAndStats(t *testing.T) {
	var fq, sq string
	packGateway(t, routeMap{
		"GET /api/admin/dlp/findings": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			fq = r.URL.RawQuery
			_, _ = io.WriteString(w, `{"findings":[{"id":"fnd_1","at":"2026-10-15T10:00:00Z","class":"pii.financial.iban","service":"gmail","tool":"send_email","outcome":"blocked","enforced":true,"count":2},{"id":"fnd_2","class":"secret.credential","service":"s","tool":"t","outcome":"flagged","enforced":false,"count":1}]}`)
		},
		"GET /api/admin/dlp/stats": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			sq = r.URL.RawQuery
			_, _ = io.WriteString(w, `{"stats":[{"key":"pii.contact.email","count":12}]}`)
		},
	})
	out, err := runCommand(t, DLPCmd(), "findings", "--since", "7d", "--class", "pii.*", "--service", "gmail")
	if err != nil || !strings.Contains(out, "fnd_1") || !strings.Contains(out, "gmail.send_email") || !strings.Contains(out, "flagged (monitor)") {
		t.Errorf("findings: %q %v", out, err)
	}
	for _, want := range []string{"class=pii.%2A", "service=gmail", "from=20"} {
		if !strings.Contains(fq, want) {
			t.Errorf("query %q lacks %q", fq, want)
		}
	}
	if out, err := runCommand(t, DLPCmd(), "stats", "--group-by", "service"); err != nil || !strings.Contains(out, "pii.contact.email") || !strings.Contains(sq, "groupBy=service") {
		t.Errorf("stats: %q %q %v", out, sq, err)
	}
	for _, args := range [][]string{{"findings", "--json"}, {"stats", "--json"}} {
		if out, err := runCommand(t, DLPCmd(), args...); err != nil || !strings.Contains(out, "{") {
			t.Errorf("%v: %q %v", args, out, err)
		}
	}
	for _, args := range [][]string{{"findings", "--since", "soon"}, {"findings", "--since", "0h"}, {"stats", "--group-by", "color"}, {"stats", "--since", "xd"}} {
		if _, err := runCommand(t, DLPCmd(), args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if _, err := parseSince("36h"); err != nil {
		t.Error(err)
	}
	if got, _ := parseSince("1d"); time.Since(got) < 23*time.Hour {
		t.Errorf("1d = %v", got)
	}
}

func TestDLPEmptyFindingsAndStats(t *testing.T) {
	packGateway(t, routeMap{
		"GET /api/admin/dlp/findings": jsonReply(200, `{"findings":[]}`),
		"GET /api/admin/dlp/stats":    jsonReply(200, `{"stats":[]}`),
	})
	for _, v := range []string{"findings", "stats"} {
		if out, err := runCommand(t, DLPCmd(), v); err != nil || !strings.Contains(out, "No findings.") {
			t.Errorf("%s: %q %v", v, out, err)
		}
	}
}

func TestDLPFeedback(t *testing.T) {
	ps := packGateway(t, routeMap{"POST /api/dlp/findings/fnd_1/feedback": jsonReply(200, `{"ok":true}`)})
	if out, err := runCommand(t, DLPCmd(), "feedback", "fnd_1", "--false-positive", "--scope", "tool"); err != nil || !strings.Contains(out, "Recorded false positive for fnd_1.") {
		t.Errorf("%q %v", out, err)
	}
	if b := ps.body("POST /api/dlp/findings/fnd_1/feedback", 0); b["verdict"] != "false_positive" || b["scope"] != "tool" {
		t.Errorf("body = %v", b)
	}
	if _, err := runCommand(t, DLPCmd(), "feedback", "fnd_1", "--true-positive"); err != nil {
		t.Error(err)
	}
	if out, err := runCommand(t, DLPCmd(), "feedback", "fnd_1", "--true-positive", "--json"); err != nil || !strings.Contains(out, `"ok":true`) {
		t.Errorf("json: %q %v", out, err)
	}
	for _, args := range [][]string{{"feedback", "fnd_1"}, {"feedback", "fnd_1", "--false-positive", "--true-positive"}, {"feedback", "fnd_1", "--false-positive", "--scope", "everything"}} {
		if _, err := runCommand(t, DLPCmd(), args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestDLPHashReadsTheValueFromStandardInput(t *testing.T) {
	ps := packGateway(t, routeMap{"POST /api/admin/dlp/hash": jsonReply(200, `{"key":"vk1_abc"}`)})
	cmd := DLPCmd()
	cmd.SetIn(strings.NewReader("SK3112000000198742637541\n"))
	out, err := runCommand(t, cmd, "hash", "--detector", "iban")
	if err != nil || strings.TrimSpace(out) != "vk1_abc" {
		t.Fatalf("%q %v", out, err)
	}
	if b := ps.body("POST /api/admin/dlp/hash", 0); b["value"] != "SK3112000000198742637541" || b["detector"] != "iban" {
		t.Errorf("body = %v", b)
	}
	cmd = DLPCmd()
	cmd.SetIn(strings.NewReader("v"))
	if out, err := runCommand(t, cmd, "hash", "--detector", "iban", "--json"); err != nil || !strings.Contains(out, `"key"`) {
		t.Errorf("json: %q %v", out, err)
	}
	if _, err := runCommand(t, DLPCmd(), "hash"); err == nil {
		t.Error("no detector")
	}
	cmd = DLPCmd()
	cmd.SetIn(strings.NewReader(""))
	if _, err := runCommand(t, cmd, "hash", "--detector", "iban"); err == nil {
		t.Error("empty value")
	}
}

func TestConnectorsProfile(t *testing.T) {
	ps := packGateway(t, routeMap{
		"GET /api/admin/connectors/data-profiles":      jsonReply(200, `{"profiles":[{"service":"fakturoid","category":"financial","residency":"eu","basis":"org_verified"},{"service":"gmail","category":"customer_pii"}]}`),
		"PUT /api/admin/connectors/gmail/data-profile": jsonReply(200, `{}`),
	})
	out, err := runCommand(t, ConnectorsCmd(), "profile", "fakturoid")
	if err != nil || !strings.Contains(out, "fakturoid: financial data, residency eu (org_verified)") {
		t.Errorf("get: %q %v", out, err)
	}
	if out, err := runCommand(t, ConnectorsCmd(), "profile", "gmail", "--set-residency", "eu", "--basis", "org_verified"); err != nil || !strings.Contains(out, "gmail: customer_pii data") {
		t.Errorf("set: %q %v", out, err)
	}
	if b := ps.body("PUT /api/admin/connectors/gmail/data-profile", 0); b["residency"] != "eu" || b["basis"] != "org_verified" {
		t.Errorf("body = %v", b)
	}
	if out, err := runCommand(t, ConnectorsCmd(), "profile", "fakturoid", "--json"); err != nil || !strings.Contains(out, `"residency":"eu"`) {
		t.Errorf("json: %q %v", out, err)
	}
	if _, err := runCommand(t, ConnectorsCmd(), "profile", "unknown"); err == nil || !strings.Contains(err.Error(), "no data profile") {
		t.Errorf("unknown: %v", err)
	}
	if _, err := runCommand(t, ConnectorsCmd(), "profile", "gmail", "--set-residency", "eu"); err == nil || !strings.Contains(err.Error(), "--basis") {
		t.Errorf("no basis: %v", err)
	}
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, ConnectorsCmd(), "profile", "gmail", "--set-residency", "eu", "--basis", "x"); exitCodeOf(err) != 10 {
		t.Errorf("agent: %v", err)
	}
}
