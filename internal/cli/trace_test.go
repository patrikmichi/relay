package cli

import (
	"net/http"
	"strings"
	"testing"
)

const traceLookupPath = "GET /api/org/current/observability/lookup?id=req_01J9ZZ0000000000000000REQ1"

func TestTrace_PrintsTheSpanTreeAuditAndErrors(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		traceLookupPath: reply(200, contractFixture(t, "trace-lookup.json")),
	})
	out, err := runCommand(t, TraceCmd(), "req_01J9ZZ0000000000000000REQ1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Trace:   4bf92f3577b34da6a3ce929d0e0e4736",
		"  tool_call stripe.create_refund [tool_call]  pending_approval  412ms",
		"    approval [approval]  pending  3ms",
		"tool_call  stripe.create_refund  outcome=pending_approval  risk=destructive  user=dev@example.com  approval=apr_1",
		"warning  /api/mcp  upstream slow",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestTrace_JSONPrintsTheBodyUnchanged(t *testing.T) {
	body := contractFixture(t, "trace-lookup.json")
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){traceLookupPath: reply(200, body)})
	out, err := runCommand(t, TraceCmd(), "req_01J9ZZ0000000000000000REQ1", "--json")
	if err != nil || strings.TrimSpace(out) != strings.TrimSpace(body) {
		t.Fatalf("out = %q, %v", out, err)
	}
}

func TestTrace_ExplainsAForbiddenAndAMissingId(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		traceLookupPath: reply(403, `{"error":"forbidden"}`),
		"GET /api/org/org_9/observability/lookup?id=apr_missing1": reply(404, `{"error":"not_found"}`),
	})
	if _, err := runCommand(t, TraceCmd(), "req_01J9ZZ0000000000000000REQ1"); err == nil || !strings.Contains(err.Error(), "needs an organisation admin or owner") {
		t.Errorf("403 err = %v", err)
	}
	if _, err := runCommand(t, TraceCmd(), "apr_missing1", "--org", "org_9"); err == nil || !strings.Contains(err.Error(), "nothing recorded for apr_missing1") {
		t.Errorf("404 err = %v", err)
	}
	if _, err := runCommand(t, TraceCmd(), "no spaces!"); err == nil || !strings.Contains(err.Error(), "is not a request id") {
		t.Errorf("bad id err = %v", err)
	}
}

func TestTrace_ASpanWithAMissingParentIsARoot(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/org/current/observability/lookup?id=trace0001": reply(200, `{"spans":[{"span_id":"b","parent_span_id":"gone","kind":"code_run","name":"run","start_ts":"1","duration_ms":5,"status":"ok"},{"span_id":"c","parent_span_id":"c","kind":"x","name":"self","start_ts":"2","duration_ms":1,"status":"ok"}]}`),
	})
	out, err := runCommand(t, TraceCmd(), "trace0001")
	if err != nil || !strings.Contains(out, "\n  run [code_run]") || !strings.Contains(out, "\n  self [x]") {
		t.Fatalf("out = %q, %v", out, err)
	}
}
