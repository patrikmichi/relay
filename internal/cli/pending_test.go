package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patrikmichi/relay/internal/client"
)

const (
	heldStructured = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"status\":\"pending_approval\"}"}],"structuredContent":{"status":"pending_approval","approval_id":"apr_1","service":"stripe","tool":"create_refund","risk":"destructive","args_digest":"9b2e","expires_at":"2026-10-16T12:15:00.000Z"},"isError":false}}`
	heldTextOnly   = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"status\":\"pending_approval\",\"approval_id\":\"apr_1\",\"service\":\"stripe\",\"tool\":\"create_refund\",\"risk\":\"write\",\"expires_at\":\"soon\"}"}],"isError":false}}`
	heldREST       = `{"ok":true,"status":"pending_approval","approval_id":"apr_1","service":"stripe","tool":"create_refund","risk":"outward","args_digest":"9b2e","expires_at":"2026-10-16T12:15:00.000Z"}`
	executedJSON   = `{"ok":true,"status":"approved","approval_id":"apr_1","executed":true,"result":{"refund":"re_1"}}`
)

// heldCallGateway answers the tool call with held and the await with awaitReply.
func heldCallGateway(t *testing.T, status int, held, awaitReply string) *httptest.Server {
	t.Helper()
	srv, _ := approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/stripe/mcp":            reply(status, held),
		"POST /api/approvals/apr_1/await": awaitHandler(awaitReply),
	})
	return srv
}

func runCall(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runCommand(t, CallCmd(), append([]string{"stripe", "create_refund", "--arg", "amount=4200"}, args...)...)
}

func TestCall_HeldCallPrintsTheApprovalAndExitsPending(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		risk   string
	}{
		"mcp structuredContent": {200, heldStructured, "destructive"},
		"mcp text content":      {200, heldTextOnly, "write"},
		"rest 202":              {202, heldREST, "outward"},
	} {
		t.Run(name, func(t *testing.T) {
			heldCallGateway(t, tc.status, tc.body, executedJSON)
			out, err := runCall(t)
			if exitCodeOf(err) != exitPending {
				t.Fatalf("exit code = %d (%v), want %d", exitCodeOf(err), err, exitPending)
			}
			if err.Error() != "" {
				t.Errorf("the held call is reported on stdout, not as an error: %q", err)
			}
			for _, want := range []string{"apr_1", "stripe create_refund", tc.risk, "relay approvals wait apr_1"} {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
		})
	}
	heldCallGateway(t, 200, heldStructured, executedJSON)
	out, _ := runCall(t)
	if !strings.Contains(out, "2026-10-16T12:15:00.000Z") {
		t.Errorf("output has no expiry:\n%s", out)
	}
}

func TestCall_HeldCallWithJSONPrintsTheEnvelope(t *testing.T) {
	heldCallGateway(t, 200, heldStructured, executedJSON)
	out, err := runCall(t, "--json")
	if exitCodeOf(err) != exitPending || strings.TrimSpace(out) != heldStructured {
		t.Errorf("exit %d (%v), output %q", exitCodeOf(err), err, out)
	}
}

func TestCall_WaitPrintsTheResultOfTheApprovedCall(t *testing.T) {
	heldCallGateway(t, 200, heldStructured, executedJSON)
	out, err := runCall(t, "--wait=5")
	if err != nil {
		t.Fatalf("exit %d: %v", exitCodeOf(err), err)
	}
	if !strings.Contains(out, `"refund": "re_1"`) || strings.Contains(out, "pending_approval") {
		t.Errorf("output = %q", out)
	}
}

func TestCall_WaitWithJSONPrintsOnlyTheFinalResponse(t *testing.T) {
	heldCallGateway(t, 200, heldStructured, executedJSON)
	out, err := runCall(t, "--wait=5", "--json")
	if err != nil || strings.TrimSpace(out) != executedJSON {
		t.Errorf("output %q, err %v", out, err)
	}
}

func TestCall_WaitFollowsTheDecision(t *testing.T) {
	for reply, code := range map[string]int{
		`{"ok":true,"status":"denied","approval_id":"apr_1","approver":"boss@example.com"}`: exitDenied,
		`{"ok":true,"status":"expired","approval_id":"apr_1"}`:                              exitExpired,
		`{"ok":true,"status":"pending","approval_id":"apr_1","expires_at":"soon"}`:          exitPending,
	} {
		heldCallGateway(t, 200, heldStructured, reply)
		_, err := runCall(t, "--wait=0")
		if exitCodeOf(err) != code {
			t.Errorf("%s: exit code = %d (%v), want %d", reply, exitCodeOf(err), err, code)
		}
	}
}

func TestCall_BareWaitUsesTheDefaultLimit(t *testing.T) {
	cmd := CallCmd()
	if f := cmd.Flags().Lookup("wait"); f == nil || f.NoOptDefVal != "300" {
		t.Fatalf("--wait = %+v, want a bare flag that waits 300 seconds", f)
	}
	if err := cmd.ParseFlags([]string{"--wait"}); err != nil {
		t.Fatal(err)
	}
	opts, err := callOptsFor(cmd, cmd.Flags().Lookup("wait").Value.String())
	if err != nil || !opts.wait || opts.waitFor.Seconds() != defaultWaitSeconds {
		t.Errorf("opts = %+v, %v", opts, err)
	}
	notWaiting, _ := callOptsFor(CallCmd(), "")
	if notWaiting.wait {
		t.Error("a call without --wait must not wait")
	}
}

func TestCall_ResultsThatAreNotHeldAreUnchanged(t *testing.T) {
	ordinary := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"status\":\"ok\"}"}],"structuredContent":{"status":"ok"},"isError":false}}`
	heldCallGateway(t, 200, ordinary, executedJSON)
	out, err := runCall(t, "--wait=5")
	if err != nil || !strings.Contains(out, `"status": "ok"`) {
		t.Errorf("output %q, err %v", out, err)
	}
}

func TestCallTool_HeldCallDirectly(t *testing.T) {
	srv := heldCallGateway(t, 200, heldStructured, executedJSON)
	var out bytes.Buffer
	err := callToolWithContext(context.Background(), client.New(srv.URL, "tok"), "stripe", "create_refund", nil, false, &out, callOpts{wait: true, waitFor: 5 * time.Second})
	if err != nil || !strings.Contains(out.String(), "re_1") {
		t.Errorf("output %q, err %v", out.String(), err)
	}
}
