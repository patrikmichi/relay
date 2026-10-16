package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// contractFixture reads a gateway response shape recorded from the billing
// and scalability specs. The gateway's own contract tests must produce the
// same documents.
func contractFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "contracts", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBillingStatus_PrintsThePlanSeatsAndBillingLink(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/billing/status": reply(200, contractFixture(t, "billing-status.json")),
	})
	out, err := runCommand(t, BillingCmd(), "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Acme (acme)", "Pro — subscription", "trialing, trial ends 2026-10-30T12:00:00Z",
		"2026-10-16T12:00:00Z to 2026-11-16T12:00:00Z", "3 used, 5 purchased, 10 at most",
		"Verified:  yes", "Overage:   off", "https://gateway.example/settings/billing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Cancels:") {
		t.Errorf("a plan that is not cancelling printed a cancel line:\n%s", out)
	}
}

func TestBillingStatus_SimulatedPlanWithSpendCapAndMemberView(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/billing/status": reply(200, contractFixture(t, "billing-status-simulated.json")),
	})
	out, err := runCommand(t, BillingCmd(), "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Expert — simulated", "Cancels:   at the end of the period", "Verified:  no", "Overage:   on, spend cap 25.00 EUR", "2 used, 0 purchased", "a member's view"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "at most") {
		t.Errorf("an unlimited seat count printed a maximum:\n%s", out)
	}
}

func TestBillingStatus_JSONIsTheBodyUnchanged(t *testing.T) {
	body := contractFixture(t, "billing-status.json")
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/billing/status": reply(200, body),
	})
	out, err := runCommand(t, BillingCmd(), "status", "--json")
	if err != nil || strings.TrimSpace(out) != strings.TrimSpace(body) {
		t.Fatalf("--json = %q, %v", out, err)
	}
}

func TestBillingUsage_PrintsATableWithPercentAndProjectedCost(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/billing/usage?period=current": reply(200, contractFixture(t, "billing-usage.json")),
	})
	out, err := runCommand(t, BillingCmd(), "usage")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"METER", "PROJECTED COST", "tool_calls", "41000", "50000", "82%", "62000", "6.00 EUR", "trace_spans", "unlimited"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestBillingUsage_ByServiceMatchesTheFixtureAndSendsTheQuery(t *testing.T) {
	body := contractFixture(t, "billing-usage-by-service.json")
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/billing/usage?by=service&meter=tool_calls&period=previous": reply(200, body),
	})
	out, err := runCommand(t, BillingCmd(), "usage", "--by", "service", "--meter", "tool_calls", "--period", "previous")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  stripe", "30000", "  google-workspace", "11000"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	out, err = runCommand(t, BillingCmd(), "usage", "--by", "service", "--meter", "tool_calls", "--period", "previous", "--json")
	if err != nil || strings.TrimSpace(out) != strings.TrimSpace(body) {
		t.Fatalf("--json = %q, %v", out, err)
	}
}

func TestBillingUsage_RejectsBadFlagsWithoutDialing(t *testing.T) {
	_, bodies := approvalsGateway(t, nil)
	for _, args := range [][]string{{"usage", "--period", "next"}, {"usage", "--by", "color"}} {
		if _, err := runCommand(t, BillingCmd(), args...); err == nil || !strings.Contains(err.Error(), "invalid --") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	if len(*bodies) != 0 {
		t.Errorf("reached the gateway: %v", *bodies)
	}
}

func TestBilling_AnAgentSessionMayRunStatusAndUsage(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/billing/status":               reply(200, contractFixture(t, "billing-status.json")),
		"GET /api/billing/usage?period=current": reply(200, contractFixture(t, "billing-usage.json")),
	})
	t.Setenv("RELAY_AGENT", "1")
	for _, verb := range []string{"status", "usage"} {
		if _, err := runCommand(t, BillingCmd(), verb); err != nil {
			t.Errorf("%s as an agent: %v", verb, err)
		}
	}
}

func TestBilling_APlanLimitOnStatusIsExit30WithTheLink(t *testing.T) {
	refusingGateway(t, 402, nil, `{"error":"plan_required","message":"billing details need an admin","upgrade_url":"https://gateway.example/settings/billing/plans"}`)
	_, err := runCommand(t, BillingCmd(), "status")
	if got := exitCodeOf(err); got != 30 || !strings.Contains(err.Error(), "https://gateway.example/settings/billing/plans") {
		t.Fatalf("exit %d, err %v", got, err)
	}
}

func TestBillingOpen_PrintsTheLinkWithoutATerminalAndOpensTheBrowserWithOne(t *testing.T) {
	srv, _ := approvalsGateway(t, nil)
	opened := stubBrowserWith(t, nil)

	out, err := runCommand(t, BillingCmd(), "open")
	if err != nil || strings.TrimSpace(out) != srv.URL+"/settings/billing" {
		t.Fatalf("non-TTY: %q, %v", out, err)
	}
	if len(*opened) != 0 {
		t.Errorf("opened %v without a terminal", *opened)
	}

	out, err = runWithInput(t, BillingCmd(), "", "open")
	if err != nil || !strings.Contains(out, "Opened "+srv.URL+"/settings/billing") || len(*opened) != 1 {
		t.Fatalf("TTY: %q, %v, opened %v", out, err, *opened)
	}

	out, err = runWithInput(t, BillingCmd(), "", "open", "--no-browser")
	if err != nil || strings.TrimSpace(out) != srv.URL+"/settings/billing" || len(*opened) != 1 {
		t.Fatalf("--no-browser: %q, %v, opened %v", out, err, *opened)
	}
}

func TestBillingFixturesAreValidJSON(t *testing.T) {
	for _, name := range []string{"billing-status.json", "billing-status-simulated.json", "billing-usage.json", "billing-usage-by-service.json", "org-usage.json", "run-accepted.json", "job.json", "jobs.json"} {
		var v any
		if err := json.Unmarshal([]byte(contractFixture(t, name)), &v); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	var buf bytes.Buffer
	printBillingUsage(&buf, billingUsage{})
	if !strings.Contains(buf.String(), "No metered usage") {
		t.Errorf("empty usage printed %q", buf.String())
	}
}

func TestUsage_PrintsEveryMetricAndFlagsTheOnesOverTheirLimits(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/org/usage": reply(200, contractFixture(t, "org-usage.json")),
	})
	out, err := runCommand(t, UsageCmd())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Plan: free", "tool_calls", "1650", "2000", "over the soft limit", "agent_runs", "storage_bytes", "seats", "at the hard limit"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestUsage_PassesTheDatesAndPrintsJSONUnchanged(t *testing.T) {
	body := contractFixture(t, "org-usage.json")
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/org/usage?from=2026-10-01&to=2026-10-16": reply(200, body),
	})
	out, err := runCommand(t, UsageCmd(), "--from", "2026-10-01", "--to", "2026-10-16", "--json")
	if err != nil || strings.TrimSpace(out) != strings.TrimSpace(body) {
		t.Fatalf("--json = %q, %v", out, err)
	}
	if _, err := runCommand(t, UsageCmd(), "--from", "yesterday"); err == nil || !strings.Contains(err.Error(), "invalid --from") {
		t.Errorf("err = %v", err)
	}
}

func TestUsage_AQuotaRefusalIsExit13(t *testing.T) {
	refusingGateway(t, 429, nil, `{"error":"quota_exceeded","metric":"api_requests","limit":1000,"resets_at":"2026-10-17T00:00:00Z"}`)
	_, err := runCommand(t, UsageCmd())
	if got := exitCodeOf(err); got != 13 {
		t.Fatalf("exit %d (%v), want 13", got, err)
	}
}
