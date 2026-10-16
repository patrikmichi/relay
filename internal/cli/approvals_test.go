package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	pendingApprovalJSON = `{"approval_id":"apr_1","status":"pending","service":"stripe","tool":"create_refund","risk":"destructive","requester":"dev@example.com","created_at":"2026-10-16T12:00:00.000Z","expires_at":"2026-10-16T12:15:00.000Z","reason":"risk"}`
	approvedJSON        = `{"approval_id":"apr_1","status":"approved","service":"stripe","tool":"create_refund","risk":"write","approver":"boss@example.com","grant_minutes":30}`
)

// approvalsGateway serves the approvals API from handlers keyed by "METHOD path"
// and records the JSON body of each POST.
func approvalsGateway(t *testing.T, handlers map[string]func(w http.ResponseWriter, body map[string]any)) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
			bodies = append(bodies, body)
		}
		h, ok := handlers[r.Method+" "+r.URL.RequestURI()]
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		h(w, body)
	}))
	t.Cleanup(srv.Close)
	withAPIKeyGateway(t, srv)
	t.Setenv("RELAY_AGENT", "")
	awaitRetryDelay = time.Millisecond
	t.Cleanup(func() { awaitRetryDelay = time.Second })
	return srv, &bodies
}

func reply(status int, body string) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	return ExitCode(err)
}

func TestApprovalsList_PrintsATableAndPassesTheStatus(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/approvals?status=pending": reply(200, `{"approvals":[`+pendingApprovalJSON+`]}`),
		"GET /api/approvals?status=all":     reply(200, `{"approvals":[]}`),
	})

	out, err := runCommand(t, ApprovalsCmd(), "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"apr_1", "pending", "stripe.create_refund", "destructive", "dev@example.com", "2026-10-16T12:15:00.000Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	out, err = runCommand(t, ApprovalsCmd(), "list", "--status", "all")
	if err != nil || !strings.Contains(out, "No all approvals") {
		t.Errorf("empty list: %q, %v", out, err)
	}
}

func TestApprovalsList_JSONIsTheBodyUnchangedAndStatusIsValidated(t *testing.T) {
	body := `{"approvals":[` + pendingApprovalJSON + `]}`
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/approvals?status=denied": reply(200, body),
	})

	out, err := runCommand(t, ApprovalsCmd(), "list", "--status", "denied", "--json")
	if err != nil || strings.TrimSpace(out) != body {
		t.Errorf("--json = %q, %v", out, err)
	}
	if _, err := runCommand(t, ApprovalsCmd(), "list", "--status", "bogus"); err == nil || !strings.Contains(err.Error(), "invalid --status") {
		t.Errorf("err = %v, want an invalid status error", err)
	}
}

func TestApprovalsShow_PrintsTheApprovalAndArgumentPreview(t *testing.T) {
	doc := `{"approval":` + pendingApprovalJSON + `,"args_preview":{"charge":"ch_1","amount":4200}}`
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/approvals/apr_1":    reply(200, doc),
		"GET /api/approvals/apr_gone": reply(404, `{"error":"Approval not found"}`),
	})

	out, err := runCommand(t, ApprovalsCmd(), "show", "apr_1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"apr_1 (pending)", "stripe create_refund", "destructive (risk)", "dev@example.com", `"charge": "ch_1"`, `"amount": 4200`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	out, err = runCommand(t, ApprovalsCmd(), "show", "apr_1", "--json")
	if err != nil || strings.TrimSpace(out) != doc {
		t.Errorf("--json = %q, %v", out, err)
	}
	if _, err := runCommand(t, ApprovalsCmd(), "show", "apr_gone"); err == nil || !strings.Contains(err.Error(), "Approval not found (HTTP 404)") {
		t.Errorf("err = %v", err)
	}
}

const pendingShowJSON = `{"approval":` + pendingApprovalJSON + `,"args_preview":{"charge":"ch_1"}}`

// decisionGateway serves the approval lookup only, and fails the test on any
// other request, including a decision.
func decisionGateway(t *testing.T, show func(http.ResponseWriter, map[string]any)) *[]string {
	t.Helper()
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		if r.Method != http.MethodGet || r.URL.Path != "/api/approvals/apr_1" {
			t.Errorf("a decision must be made in the browser, but relay sent %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		show(w, nil)
	}))
	t.Cleanup(srv.Close)
	withAPIKeyGateway(t, srv)
	t.Setenv("RELAY_AGENT", "")
	return &requests
}

func stubBrowserWith(t *testing.T, err error) *[]string {
	t.Helper()
	var opened []string
	old := openBrowserURL
	openBrowserURL = func(u string) error { opened = append(opened, u); return err }
	t.Cleanup(func() { openBrowserURL = old })
	return &opened
}

func TestApprovalsDecide_OpensTheApprovalPageAndSendsNoDecision(t *testing.T) {
	for _, decision := range []string{"approve", "deny"} {
		t.Run(decision, func(t *testing.T) {
			requests := decisionGateway(t, reply(200, pendingShowJSON))
			opened := stubBrowserWith(t, nil)

			out, err := runWithInput(t, ApprovalsCmd(), "", decision, "apr_1")
			if err != nil {
				t.Fatal(err)
			}
			if len(*opened) != 1 || !strings.HasSuffix((*opened)[0], "/approvals/apr_1?decision="+decision) {
				t.Fatalf("opened %v", *opened)
			}
			if !strings.Contains(out, "stripe create_refund") || !strings.Contains(out, "passkey") {
				t.Errorf("output lacks the call summary or the passkey hint:\n%s", out)
			}
			if len(*requests) != 1 {
				t.Errorf("requests = %v, want only the approval lookup", *requests)
			}
		})
	}
}

func TestApprovalsDecide_NoBrowserPrintsTheLinkAndAQRCode(t *testing.T) {
	requests := decisionGateway(t, reply(200, pendingShowJSON))
	opened := stubBrowserWith(t, nil)

	out, err := runCommand(t, ApprovalsCmd(), "approve", "apr_1", "--no-browser")
	if err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 0 {
		t.Errorf("--no-browser opened %v", *opened)
	}
	if !strings.Contains(out, "/approvals/apr_1?decision=approve") || !strings.Contains(out, "\x1b[30;47m") {
		t.Errorf("output lacks the link or the QR code:\n%s", out)
	}
	if len(*requests) != 1 {
		t.Errorf("requests = %v", *requests)
	}
}

func TestApprovalsDecide_AgentSessionPrintsTheLinkInsteadOfOpeningABrowser(t *testing.T) {
	decisionGateway(t, reply(200, pendingShowJSON))
	opened := stubBrowserWith(t, nil)
	t.Setenv("RELAY_AGENT", "1")

	out, err := runWithInput(t, ApprovalsCmd(), "", "approve", "apr_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 0 || !strings.Contains(out, "decision=approve") {
		t.Errorf("opened %v, output %q", *opened, out)
	}
}

func TestApprovalsDecide_FallsBackToTheLinkWhenNoBrowserOpens(t *testing.T) {
	decisionGateway(t, reply(200, pendingShowJSON))
	stubBrowserWith(t, errors.New("no display"))
	out, err := runWithInput(t, ApprovalsCmd(), "", "deny", "apr_1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no display") || !strings.Contains(out, "decision=deny") {
		t.Errorf("output = %q", out)
	}
}

func TestApprovalsDecide_RefusesAnApprovalThatIsNotPending(t *testing.T) {
	decisionGateway(t, reply(200, `{"approval":`+approvedJSON+`}`))
	opened := stubBrowserWith(t, nil)
	if _, err := runCommand(t, ApprovalsCmd(), "approve", "apr_1"); err == nil || !strings.Contains(err.Error(), "already approved") {
		t.Errorf("err = %v, want an already-decided refusal", err)
	}
	if len(*opened) != 0 {
		t.Errorf("opened %v", *opened)
	}
}

func TestApprovalsDecide_UnknownApprovalIsAnError(t *testing.T) {
	decisionGateway(t, reply(404, `{"error":"Approval not found"}`))
	opened := stubBrowserWith(t, nil)
	if _, err := runCommand(t, ApprovalsCmd(), "approve", "apr_1"); err == nil || !strings.Contains(err.Error(), "Approval not found") {
		t.Errorf("err = %v", err)
	}
	if len(*opened) != 0 {
		t.Errorf("opened %v", *opened)
	}
}

func TestApprovalsDecide_WaitReportsTheOutcomeWithTheDocumentedExitStatus(t *testing.T) {
	approvalPollInterval = time.Millisecond
	t.Cleanup(func() { approvalPollInterval = 2 * time.Second })
	for _, tc := range []struct {
		status string
		code   int
	}{{"approved", 0}, {"executed", 0}, {"denied", 10}, {"expired", 11}} {
		t.Run(tc.status, func(t *testing.T) {
			var calls atomic.Int32
			decisionGateway(t, func(w http.ResponseWriter, _ map[string]any) {
				if calls.Add(1) == 1 {
					_, _ = io.WriteString(w, pendingShowJSON)
					return
				}
				_, _ = io.WriteString(w, `{"approval":{"approval_id":"apr_1","status":"`+tc.status+`","approver":"boss@example.com"}}`)
			})
			stubBrowserWith(t, nil)
			out, err := runCommand(t, ApprovalsCmd(), "approve", "apr_1", "--wait")
			if got := exitCodeOf(err); got != tc.code {
				t.Fatalf("exit = %d (%v), want %d", got, err, tc.code)
			}
			if !strings.Contains(out, tc.status) {
				t.Errorf("output lacks %q:\n%s", tc.status, out)
			}
		})
	}
}

func TestApprovalsDecide_WaitTimesOutAsStillPending(t *testing.T) {
	approvalPollInterval = time.Millisecond
	t.Cleanup(func() { approvalPollInterval = 2 * time.Second })
	decisionGateway(t, reply(200, pendingShowJSON))
	stubBrowserWith(t, nil)
	_, err := runCommand(t, ApprovalsCmd(), "deny", "apr_1", "--wait", "--wait-timeout", "0")
	if got := exitCodeOf(err); got != exitPending {
		t.Fatalf("exit = %d (%v), want %d", got, err, exitPending)
	}
}

func TestApprovalsDecide_JSONIsOneDocumentWithTheLink(t *testing.T) {
	decisionGateway(t, reply(200, pendingShowJSON))
	stubBrowserWith(t, nil)
	out, err := runCommand(t, ApprovalsCmd(), "approve", "apr_1", "--json", "--no-browser")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ApprovalID string `json:"approval_id"`
		Decision   string `json:"decision"`
		URL        string `json:"url"`
		Opened     bool   `json:"opened"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not one JSON document: %v\n%s", err, out)
	}
	if doc.ApprovalID != "apr_1" || doc.Decision != "approve" || doc.Opened || doc.Status != "pending" || !strings.HasSuffix(doc.URL, "/approvals/apr_1?decision=approve") {
		t.Errorf("document = %+v", doc)
	}
}

func TestApprovalsDecide_HasNoNoteOrGrantFlags(t *testing.T) {
	for _, verb := range []string{"approve", "deny"} {
		for _, flag := range []string{"--note", "--grant"} {
			if _, err := runCommand(t, ApprovalsCmd(), verb, "apr_1", flag, "1"); err == nil || !strings.Contains(err.Error(), "unknown flag") {
				t.Errorf("%s %s: err = %v, want an unknown-flag error: the page takes the note and the grant", verb, flag, err)
			}
		}
	}
}

func TestParseWaitTimeout(t *testing.T) {
	for in, want := range map[string]time.Duration{"0": 0, "45": 45 * time.Second, "90s": 90 * time.Second, "5m": 5 * time.Minute, " 7 ": 7 * time.Second} {
		if got, err := parseWaitTimeout(in); err != nil || got != want {
			t.Errorf("parseWaitTimeout(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "soon", "-5", "-1m"} {
		if _, err := parseWaitTimeout(in); err == nil {
			t.Errorf("parseWaitTimeout(%q): expected an error", in)
		}
	}
}

func awaitHandler(replies ...string) func(http.ResponseWriter, map[string]any) {
	var n atomic.Int32
	return func(w http.ResponseWriter, _ map[string]any) {
		i := int(n.Add(1)) - 1
		i = min(i, len(replies)-1)
		status := 200
		if strings.Contains(replies[i], `"not_found"`) {
			status = 404
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, replies[i])
	}
}

func TestApprovalsWait_PrintsTheResultOfAnApprovedCall(t *testing.T) {
	_, bodies := approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/approvals/apr_1/await": awaitHandler(
			`{"ok":true,"status":"pending","approval_id":"apr_1","expires_at":"2026-10-16T12:15:00.000Z"}`,
			`{"ok":true,"status":"approved","approval_id":"apr_1","executed":true,"result":{"refund":"re_1"}}`,
		),
	})

	out, err := runCommand(t, ApprovalsCmd(), "wait", "apr_1", "--wait-timeout", "60")
	if err != nil {
		t.Fatalf("exit %d: %v", exitCodeOf(err), err)
	}
	if !strings.Contains(out, `"refund": "re_1"`) {
		t.Errorf("output = %q", out)
	}
	if len(*bodies) != 2 || (*bodies)[0]["timeout_s"] != float64(30) {
		t.Errorf("await requests = %v, want two polls capped at 30s", *bodies)
	}
}

func TestApprovalsWait_ExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   string
		timeout string
		code    int
		output  string
		msg     string
	}{
		{"executed", `{"ok":true,"status":"approved","approval_id":"apr_1","executed":true,"result":"done"}`, "5", 0, "done", ""},
		{"denied", `{"ok":true,"status":"denied","approval_id":"apr_1","approver":"boss@example.com","note":"too much"}`, "5", 10, "Denied by boss@example.com", ""},
		{"expired", `{"ok":true,"status":"expired","approval_id":"apr_1"}`, "5", 11, "Expired", ""},
		{"pending at the timeout", `{"ok":true,"status":"pending","approval_id":"apr_1","expires_at":"soon"}`, "0", 12, "Still pending", ""},
		{"approved but not run", `{"ok":true,"status":"approved","approval_id":"apr_1","executed":false,"message":"rate limit exceeded"}`, "0", 12, "has not run yet: rate limit exceeded", ""},
		{"call failed", `{"ok":true,"status":"approved","approval_id":"apr_1","executed":true,"error":"tool execution failed"}`, "5", 1, "", "the call failed: tool execution failed"},
		{"unknown id", `{"ok":false,"status":"not_found","approval_id":"apr_1"}`, "5", 1, "", "not found, or it was made by another session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
				"POST /api/approvals/apr_1/await": awaitHandler(tc.reply),
			})
			out, err := runCommand(t, ApprovalsCmd(), "wait", "apr_1", "--wait-timeout", tc.timeout)
			if got := exitCodeOf(err); got != tc.code {
				t.Fatalf("exit code = %d (%v), want %d", got, err, tc.code)
			}
			if !strings.Contains(out, tc.output) {
				t.Errorf("output = %q, want %q", out, tc.output)
			}
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if tc.msg == "" && msg != "" || !strings.Contains(msg, tc.msg) {
				t.Errorf("error message = %q, want %q", msg, tc.msg)
			}
		})
	}
}

func TestApprovalsWait_JSONPrintsTheBodyUnchangedAndKeepsTheExitCode(t *testing.T) {
	denied := `{"ok":true,"status":"denied","approval_id":"apr_1","approver":"boss@example.com"}`
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/approvals/apr_1/await": awaitHandler(denied),
	})
	out, err := runCommand(t, ApprovalsCmd(), "wait", "apr_1", "--json", "--wait-timeout", "0")
	if strings.TrimSpace(out) != denied || exitCodeOf(err) != exitDenied {
		t.Errorf("output %q, exit %d (%v)", out, exitCodeOf(err), err)
	}
	if err != nil && err.Error() != "" {
		t.Errorf("a --json outcome must not print an error message, got %q", err)
	}
}

func TestApprovalsWait_HTTPFailuresExitOne(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/approvals/apr_1/await": reply(500, `{"ok":false,"error":"tool execution failed"}`),
	})
	_, err := runCommand(t, ApprovalsCmd(), "wait", "apr_1", "--wait-timeout", "0")
	if exitCodeOf(err) != 1 || err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("exit %d, err %v", exitCodeOf(err), err)
	}
}
