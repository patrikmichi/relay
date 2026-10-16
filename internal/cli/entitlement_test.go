package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func refusingGateway(t *testing.T, status int, headers map[string]string, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	withAPIKeyGateway(t, srv)
	t.Setenv("ZUANO_AGENT", "")
}

func TestEntitlementRefusals_ExitCodesAndUpgradeLinkOnEveryCommand(t *testing.T) {
	const upgrade = "https://gw.example/settings/billing/plans?highlight=sandbox.run_code&org=acme"
	for _, tc := range []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		code    int
		want    string
	}{
		{"gated feature", 402, nil, `{"error":"plan_required","message":"run_code needs the Pro plan","feature":"sandbox.run_code","current_plan":"free","required_plan":"pro","upgrade_url":"` + upgrade + `"}`, 30, upgrade},
		{"limit", 402, nil, `{"error":"limit_reached","message":"seat limit reached","limit":"seats","current":3,"max":3,"upgrade_url":"` + upgrade + `"}`, 30, "seat limit reached"},
		{"quota", 402, nil, `{"error":"quota_exceeded","message":"tool calls used up","meter":"tool_calls","resets_at":"2026-11-01T00:00:00Z","upgrade_url":"` + upgrade + `"}`, 31, "resets 2026-11-01"},
		{"spend cap", 402, nil, `{"error":"spend_cap_reached","message":"spend cap reached"}`, 31, "spend cap reached"},
		{"billing down", 503, map[string]string{"Retry-After": "20"}, `{"error":"billing_unavailable","message":"billing is unavailable"}`, 32, "retry after 20s"},
		{"suspended org", 403, nil, `{"error":"org_suspended","message":"the organisation is suspended"}`, 33, "suspended"},
		{"suspended seat", 403, nil, `{"error":"seat_suspended","message":"your seat is suspended"}`, 33, "your seat is suspended"},
		{"metered quota", 429, nil, `{"error":"quota_exceeded","message":"agent runs used up","metric":"agent_runs","limit":50,"resets_at":"2026-10-17T00:00:00Z"}`, 31, "metric agent_runs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusingGateway(t, tc.status, tc.headers, tc.body)
			for name, run := range map[string]func() error{
				"call":           func() error { _, err := runCommand(t, CallCmd(), "stripe", "create_refund"); return err },
				"approvals list": func() error { _, err := runCommand(t, ApprovalsCmd(), "list"); return err },
				"approvals show": func() error { _, err := runCommand(t, ApprovalsCmd(), "show", "apr_1"); return err },
				"services":       func() error { _, err := runCommand(t, ServicesCmd()); return err },
				"approvals wait": func() error {
					_, err := runCommand(t, ApprovalsCmd(), "wait", "apr_1", "--wait-timeout", "0")
					return err
				},
				"connectors list": func() error { _, err := runCommand(t, ConnectorsCmd()); return err },
			} {
				err := run()
				if got := exitCodeOf(err); got != tc.code {
					t.Errorf("%s: exit %d (%v), want %d", name, got, err, tc.code)
					continue
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s: message %q lacks %q", name, err, tc.want)
				}
			}
		})
	}
}

func TestEntitlementRefusals_AnAgentSessionGetsTheSameMapping(t *testing.T) {
	refusingGateway(t, 402, nil, `{"error":"plan_required","message":"needs Pro","upgrade_url":"https://gw.example/u"}`)
	t.Setenv("ZUANO_AGENT", "1")
	_, err := runCommand(t, CallCmd(), "stripe", "create_refund")
	if got := exitCodeOf(err); got != 30 {
		t.Fatalf("exit %d (%v), want 30", got, err)
	}
}

func TestRateLimitIsExit13AndNamesTheWait(t *testing.T) {
	refusingGateway(t, 429, map[string]string{"Retry-After": "3"}, `{"error":"rate_limited"}`)
	_, err := runCommand(t, ApprovalsCmd(), "list")
	if got := exitCodeOf(err); got != 13 || !strings.Contains(err.Error(), "retry after 3s") {
		t.Fatalf("exit %d (%v), want 13 naming the wait", got, err)
	}
}

func TestAnUnrecognisedForbiddenIsNotAnEntitlementError(t *testing.T) {
	refusingGateway(t, 403, nil, `{"error":"approval_required"}`)
	_, err := runCommand(t, ApprovalsCmd(), "list")
	if got := exitCodeOf(err); got != 1 {
		t.Fatalf("exit %d (%v), want 1", got, err)
	}
}
