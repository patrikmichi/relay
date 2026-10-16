package cli

import (
	"net/http"
	"strings"
	"testing"
)

func TestApprovalsDecide_RefusesWithoutATerminalBeforeDialing(t *testing.T) {
	_, bodies := approvalsGateway(t, nil)
	for _, verb := range []string{"approve", "deny"} {
		// runCommand leaves stdin as the test process's own, which is not a terminal.
		_, err := runCommand(t, ApprovalsCmd(), verb, "apr_1", "--note", "x")
		if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
			t.Errorf("%s without a terminal = %v, want a refusal", verb, err)
		}
	}
	if len(*bodies) != 0 {
		t.Errorf("a refused decision reached the gateway: %v", *bodies)
	}
}

func TestApprovalsDecide_ShowsTheFullCallAndSendsNothingWhenDeclined(t *testing.T) {
	_, bodies := approvalsGateway(t, reviewable(map[string]func(http.ResponseWriter, map[string]any){}))

	for _, answer := range []string{"n\n", "\n", ""} {
		out, err := decide(t, answer, "approve", "apr_1", "--grant", "30")
		if err == nil || !strings.Contains(err.Error(), "not confirmed") {
			t.Errorf("answer %q: err = %v, want not confirmed", answer, err)
		}
		for _, want := range []string{"stripe create_refund", `"amount": 4200`, `"memo": "refund the whole order"`, "30 minutes", "Approve this call?"} {
			if !strings.Contains(out, want) {
				t.Errorf("answer %q: review lacks %q:\n%s", answer, want, out)
			}
		}
	}
	for _, b := range *bodies {
		if b != nil {
			t.Errorf("a declined decision was sent: %v", b)
		}
	}
}

func TestApprovalsApprove_RefusesToApproveWhatItCannotShow(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/approvals/apr_1?full=1": reply(200, `{"approval":`+pendingApprovalJSON+`,"args_preview":null,"truncated":false}`),
		"POST /api/approvals/apr_1/deny":  reply(200, `{"ok":true,"approval":{"approval_id":"apr_1","status":"denied"}}`),
	})
	if _, err := decide(t, "y\n", "approve", "apr_1"); err == nil || !strings.Contains(err.Error(), "blind") {
		t.Errorf("approve without arguments = %v, want a blind-approval refusal", err)
	}
	if _, err := decide(t, "y\n", "deny", "apr_1"); err != nil {
		t.Errorf("denying without arguments should work: %v", err)
	}
}

func TestApprovalsDecide_StripsControlCharactersFromTheReview(t *testing.T) {
	evil := `{"approval":{"approval_id":"apr_1","status":"pending","service":"stripe\u001b[2J","tool":"x\u009b","requester":"a\r@b"},"args":{"k":"v\u001b[31m"},"truncated":false}`
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/approvals/apr_1?full=1": reply(200, evil),
	})
	out, _ := decide(t, "n\n", "approve", "apr_1")
	for _, r := range out {
		if r == 0x1b || r == 0x9b || r == '\r' {
			t.Fatalf("control character %U reached the terminal: %q", r, out)
		}
	}
}
