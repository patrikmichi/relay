package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func refusalServer(t *testing.T, status int, headers map[string]string, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok")
}

func TestEntitlementRefusalsMapToTheDocumentedExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   int
	}{
		{"plan required", 402, `{"error":"plan_required","message":"run_code needs the Pro plan","feature":"sandbox.run_code","current_plan":"free","required_plan":"pro","upgrade_url":"https://gw.example/settings/billing/plans?highlight=sandbox.run_code"}`, ExitPlanRequired},
		{"limit reached", 402, `{"error":"limit_reached","message":"seat limit","limit":"seats","current":3,"max":3,"upgrade_url":"https://gw.example/u"}`, ExitPlanRequired},
		{"verification", 402, `{"error":"verification_required","message":"verify the organisation"}`, ExitPlanRequired},
		{"quota", 402, `{"error":"quota_exceeded","message":"tool calls used up","meter":"tool_calls","resets_at":"2026-11-01T00:00:00Z"}`, ExitUsageExhausted},
		{"spend cap", 402, `{"error":"spend_cap_reached","message":"cap reached"}`, ExitUsageExhausted},
		{"billing unavailable", 503, `{"error":"billing_unavailable","message":"try again"}`, ExitBillingDown},
		{"org suspended", 403, `{"error":"org_suspended","message":"suspended"}`, ExitSuspended},
		{"seat suspended", 403, `{"error":"seat_suspended","message":"no seat"}`, ExitSuspended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := refusalServer(t, tc.status, map[string]string{"Retry-After": "30"}, tc.body)
			resp, err := c.Get("/x")
			var ent *EntitlementError
			if !errors.As(err, &ent) || resp != nil {
				t.Fatalf("resp = %v, err = %v, want an *EntitlementError", resp, err)
			}
			if ent.ExitCode() != tc.code {
				t.Errorf("exit code = %d, want %d", ent.ExitCode(), tc.code)
			}
			if ent.Status != tc.status {
				t.Errorf("status = %d", ent.Status)
			}
		})
	}
}

func TestEntitlementErrorMessageNamesThePlanAndTheUpgradeLink(t *testing.T) {
	c := refusalServer(t, 402, nil, `{"error":"plan_required","message":"run_code needs the Pro plan","feature":"sandbox.run_code","current_plan":"free","required_plan":"pro","upgrade_url":"https://gw.example/settings/billing/plans?highlight=sandbox.run_code"}`)
	_, err := c.Get("/x")
	for _, want := range []string{"run_code needs the Pro plan", "plan free", "needs pro", "Upgrade: https://gw.example/settings/billing/plans?highlight=sandbox.run_code"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v lacks %q", err, want)
		}
	}
}

func TestEntitlementErrorCleansControlCharacters(t *testing.T) {
	c := refusalServer(t, 402, nil, `{"error":"plan_required","message":"pay\u001b[2J now","upgrade_url":"https://gw.example/u\u0007"}`)
	_, err := c.Get("/x")
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Fatalf("control characters reached the message: %q", err)
	}
}

func TestBillingUnavailableCarriesRetryAfter(t *testing.T) {
	c := refusalServer(t, 503, map[string]string{"Retry-After": "12"}, `{"error":"billing_unavailable","message":"try again"}`)
	_, err := c.Get("/x")
	if err == nil || !strings.Contains(err.Error(), "retry after 12s") {
		t.Fatalf("err = %v", err)
	}
}

func TestQuotaExceededOn429IsExit13(t *testing.T) {
	c := refusalServer(t, 429, nil, `{"error":"quota_exceeded","message":"agent runs used up","metric":"agent_runs","limit":100,"resets_at":"2026-10-17T00:00:00Z"}`)
	_, err := c.Get("/x")
	var q *QuotaExceededError
	if !errors.As(err, &q) {
		t.Fatalf("err = %v, want *QuotaExceededError", err)
	}
	if q.ExitCode() != ExitQuotaExceeded || q.Metric != "agent_runs" || q.Limit != "100" || q.ResetsAt == "" {
		t.Errorf("quota = %+v", q)
	}
	if !strings.Contains(err.Error(), "metric agent_runs") || !strings.Contains(err.Error(), "limit 100") {
		t.Errorf("message = %q", err)
	}
}

func TestOtherResponsesPassThroughWithTheirBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unknown 429 code", 429, `{"error":"something_else"}`},
		{"unknown 402 code", 402, `{"error":"something_else"}`},
		{"403 approval", 403, `{"error":"approval_required"}`},
		{"non-JSON 503", 503, `upstream down`},
		{"plain 401", 401, `{"error":"Unauthorized"}`},
		{"200", 200, `{"error":"plan_required"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := refusalServer(t, tc.status, nil, tc.body)
			resp, err := c.Get("/x")
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status || string(got) != tc.body {
				t.Errorf("status %d body %q, want %d %q", resp.StatusCode, got, tc.status, tc.body)
			}
		})
	}
}

func TestStepUpRequiredWithoutAHandlerIsExit14(t *testing.T) {
	c := refusalServer(t, 401, nil, `{"error":"step_up_required","step_up_url":"https://gw.example/stepup/su_1","step_up_id":"su_1"}`)
	_, err := c.Get("/x")
	var su *StepUpRequiredError
	if !errors.As(err, &su) || su.ExitCode() != ExitStepUp || su.ID != "su_1" || su.URL != "https://gw.example/stepup/su_1" {
		t.Fatalf("err = %v", err)
	}
}

func TestStepUpHandlerCompletesTheChallengeAndTheRequestIsRetriedOnce(t *testing.T) {
	var calls atomic.Int32
	var retryHeader, retryBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"step_up_required","step_up_url":"https://gw.example/stepup/su_1","step_up_id":"su_1"}`)
			return
		}
		retryHeader = r.Header.Get("X-Step-Up")
		b, _ := io.ReadAll(r.Body)
		retryBody = string(b)
		_, _ = io.WriteString(w, "done")
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok")
	var seen StepUpChallenge
	c.SetStepUpHandler(func(_ context.Context, ch StepUpChallenge, _ *Client) (string, error) {
		seen = ch
		return "su_1", nil
	})

	resp, err := c.Post("/act", "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "done" || retryHeader != "su_1" || retryBody != `{"a":1}` || calls.Load() != 2 {
		t.Errorf("body %q header %q retry body %q calls %d", body, retryHeader, retryBody, calls.Load())
	}
	if seen.ID != "su_1" || seen.URL != "https://gw.example/stepup/su_1" || seen.Gateway != srv.URL {
		t.Errorf("challenge = %+v", seen)
	}
}

func TestStepUpHandlerFailureStopsWithoutARetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"step_up_required","step_up_id":"su_1"}`)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok")
	boom := errors.New("declined")
	c.SetStepUpHandler(func(context.Context, StepUpChallenge, *Client) (string, error) { return "", boom })
	if _, err := c.Get("/x"); !errors.Is(err, boom) || calls.Load() != 1 {
		t.Fatalf("err = %v after %d calls", err, calls.Load())
	}
}

func TestStepUpIsRetriedOnlyOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"step_up_required","step_up_id":"su_1"}`)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok")
	c.SetStepUpHandler(func(context.Context, StepUpChallenge, *Client) (string, error) { return "su_1", nil })
	_, err := c.Get("/x")
	var su *StepUpRequiredError
	if !errors.As(err, &su) || calls.Load() != 2 {
		t.Fatalf("err = %v after %d calls, want the second refusal returned", err, calls.Load())
	}
}

type fakeSigner struct{ args []string }

func (f *fakeSigner) Proof(method, rawURL, token string) (string, error) {
	f.args = []string{method, rawURL, token}
	return "proof.jwt", nil
}

func TestProofSignerSwitchesTheSchemeToDPoP(t *testing.T) {
	var auth, dpop string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		auth, dpop = r.Header.Get("Authorization"), r.Header.Get("DPoP")
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok")
	signer := &fakeSigner{}
	c.SetProofSigner(signer)
	resp, err := c.Get("/api/x?y=1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if auth != "DPoP tok" || dpop != "proof.jwt" {
		t.Errorf("Authorization %q, DPoP %q", auth, dpop)
	}
	if len(signer.args) != 3 || signer.args[0] != "GET" || signer.args[1] != srv.URL+"/api/x?y=1" || signer.args[2] != "tok" {
		t.Errorf("signer args = %v", signer.args)
	}
}
