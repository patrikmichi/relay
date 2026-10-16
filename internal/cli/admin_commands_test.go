package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSessions_ListMarksTheCurrentSessionAndPrintsJSONUnchanged(t *testing.T) {
	body := contractFixture(t, "sessions.json")
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/me/sessions": reply(200, body),
	})
	out, err := runCommand(t, SessionsCmd(), "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ses_browser1", "dashboard", "ses_cli1 (this one)", "cli_personal", "relay/0.3.0 (linux/amd64) 203.0.113.0/24"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	out, err = runCommand(t, SessionsCmd(), "list", "--json")
	if err != nil || strings.TrimSpace(out) != strings.TrimSpace(body) {
		t.Fatalf("--json = %q, %v", out, err)
	}
}

func TestSessions_RevokeUsesDELETEAndRevokeAllUsesPOST(t *testing.T) {
	_, bodies := approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"DELETE /api/me/sessions/ses_browser1": reply(200, `{"ok":true}`),
		"POST /api/me/sessions/revoke-all":     reply(200, `{"ok":true,"revoked":3}`),
	})
	out, err := runCommand(t, SessionsCmd(), "revoke", "ses_browser1")
	if err != nil || !strings.Contains(out, "Revoked ses_browser1") {
		t.Fatalf("revoke: %q, %v", out, err)
	}
	out, err = runCommand(t, SessionsCmd(), "revoke-all")
	if err != nil || !strings.Contains(out, "Signed out everywhere") {
		t.Fatalf("revoke-all: %q, %v", out, err)
	}
	if len(*bodies) != 1 {
		t.Errorf("posts = %d, want only the revoke-all", len(*bodies))
	}
}

func TestSessions_RevokeAllCompletesAStepUpAndRetries(t *testing.T) {
	var posts atomic.Int32
	var retryHeader atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/me/sessions/revoke-all":
			posts.Add(1)
			if id := r.Header.Get("X-Step-Up"); id != "" {
				retryHeader.Store(id)
				_, _ = io.WriteString(w, `{"ok":true}`)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"step_up_required","step_up_id":"su_7","step_up_url":"http://`+r.Host+`/stepup/su_7"}`)
		case "/api/stepup/su_7":
			_, _ = io.WriteString(w, `{"status":"approved"}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	withAPIKeyGateway(t, srv)
	_, opened := stepUpEnv(t, true)

	out, err := runCommand(t, SessionsCmd(), "revoke-all")
	if err != nil || !strings.Contains(out, "Signed out everywhere") {
		t.Fatalf("revoke-all: %q, %v", out, err)
	}
	if posts.Load() != 2 || retryHeader.Load() != "su_7" || len(*opened) != 1 || (*opened)[0] != srv.URL+"/stepup/su_7" {
		t.Errorf("posts %d, retry header %v, opened %v", posts.Load(), retryHeader.Load(), *opened)
	}
}

func TestAudit_VerifyReportsAGoodAndABrokenLog(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/admin/audit/verify?from=2026-09-16&to=2026-10-16": reply(200, contractFixture(t, "audit-verify-ok.json")),
		"GET /api/admin/audit/verify":                               reply(200, contractFixture(t, "audit-verify-broken.json")),
	})
	out, err := runCommand(t, AuditCmd(), "verify", "--from", "2026-09-16", "--to", "2026-10-16")
	if err != nil || !strings.Contains(out, "Audit log verified: 18234 events, 30 checkpoints") {
		t.Fatalf("good log: %q, %v", out, err)
	}
	out, err = runCommand(t, AuditCmd(), "verify")
	if got := exitCodeOf(err); got != 1 || err == nil || !strings.Contains(err.Error(), "first broken event is seq 9021") || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("broken log: exit %d, err %v\n%s", got, err, out)
	}
	if !strings.Contains(out, "did NOT verify") {
		t.Errorf("output = %q", out)
	}
	if _, err := runCommand(t, AuditCmd(), "verify", "--from", "soon"); err == nil || !strings.Contains(err.Error(), "invalid --from") {
		t.Errorf("err = %v", err)
	}
}

func TestAudit_ExportStreamsToStdoutAndToANewFile(t *testing.T) {
	events := `{"seq":1,"action":"tool_call"}` + "\n" + `{"seq":2,"action":"approval_decided"}` + "\n"
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/admin/audit/export?actor=boss&format=csv&service=stripe": reply(200, events),
		"GET /api/admin/audit/export?format=ndjson":                        reply(200, events),
	})
	out, err := runCommand(t, AuditCmd(), "export")
	if err != nil || out != events {
		t.Fatalf("stdout export: %q, %v", out, err)
	}

	path := filepath.Join(t.TempDir(), "audit.csv")
	out, err = runCommand(t, AuditCmd(), "export", "--format", "csv", "--actor", "boss", "--service", "stripe", "--output", path)
	if err != nil || !strings.Contains(out, fmt.Sprintf("Wrote %d bytes", len(events))) {
		t.Fatalf("file export: %q, %v", out, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != events {
		t.Fatalf("file = %q, %v", got, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := runCommand(t, AuditCmd(), "export", "--output", path); err == nil {
		t.Error("an existing file was overwritten")
	}
	if _, err := runCommand(t, AuditCmd(), "export", "--format", "xml"); err == nil || !strings.Contains(err.Error(), "invalid --format") {
		t.Errorf("err = %v", err)
	}
}

func TestAudit_ExportRefusalCarriesTheGatewaysSentence(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/admin/audit/export?format=ndjson": reply(403, `{"error":"forbidden","message":"audit export needs the Business plan"}`),
	})
	if _, err := runCommand(t, AuditCmd(), "export"); err == nil || !strings.Contains(err.Error(), "audit export needs the Business plan (HTTP 403)") {
		t.Fatalf("err = %v", err)
	}
}

func TestPolicyExplain_PrintsTheDecisionItsSourceAndWhoCanChangeIt(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		tool    string
		args    []string
		query   string
		want    []string
	}{
		{"policy-preview-off.json", "create_refund", nil, "params=%7B%7D&principal=cli_personal&service=stripe&tool=create_refund", []string{"stripe.create_refund (destructive) is Off for cli_personal", "from the built-in default", "Can be changed by: org admins"}},
		{"policy-preview-approval.json", "create_refund", []string{"--arg-json", "amount=80", "--arg", "currency=EUR"}, "params=%7B%22amount%22%3A80%2C%22currency%22%3A%22EUR%22%7D&principal=cli_personal&service=stripe&tool=create_refund", []string{"is Approval required", "from an organisation tool override ovr_7 when amount <= 100"}},
		{"policy-preview-platform.json", "delete_customer", []string{"--principal", "cli_agent"}, "params=%7B%7D&principal=cli_agent&service=stripe&tool=delete_customer", []string{"Off for cli_agent", "from a platform block", "Can be changed by: platform admins"}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
				"GET /api/admin/risk-policy/preview?" + tc.query: reply(200, contractFixture(t, tc.fixture)),
			})
			out, err := runCommand(t, PolicyCmd(), append([]string{"explain", "stripe", tc.tool}, tc.args...)...)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestPolicyExplain_AnAgentSessionAsksAsAnAgentAndBadPrincipalsAreRefused(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/admin/risk-policy/preview?params=%7B%7D&principal=cli_agent&service=stripe&tool=create_refund": reply(200, contractFixture(t, "policy-preview-off.json")),
	})
	t.Setenv("RELAY_AGENT", "1")
	if _, err := runCommand(t, PolicyCmd(), "explain", "stripe", "create_refund"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, PolicyCmd(), "explain", "stripe", "create_refund", "--principal", "root"); err == nil || !strings.Contains(err.Error(), "invalid --principal") {
		t.Errorf("err = %v", err)
	}
}

func TestPolicyExplain_JSONIsTheBodyUnchanged(t *testing.T) {
	body := contractFixture(t, "policy-preview-off.json")
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/admin/risk-policy/preview?params=%7B%7D&principal=cli_personal&service=stripe&tool=create_refund": reply(200, body),
	})
	out, err := runCommand(t, PolicyCmd(), "explain", "stripe", "create_refund", "--json")
	if err != nil || strings.TrimSpace(out) != strings.TrimSpace(body) {
		t.Fatalf("--json = %q, %v", out, err)
	}
}
