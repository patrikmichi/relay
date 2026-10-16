package cli

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type handlers = map[string]func(http.ResponseWriter, map[string]any)

func allowSecrets(t *testing.T) {
	t.Helper()
	old := secretsMayPrint
	secretsMayPrint = func(io.Writer) bool { return true }
	t.Cleanup(func() { secretsMayPrint = old })
}

func TestWebhooksList_ShowsAllThreeKinds(t *testing.T) {
	approvalsGateway(t, handlers{
		"GET /api/webhooks/endpoints":     reply(200, contractFixture(t, "webhooks-endpoints.json")),
		"GET /api/webhooks/triggers":      reply(200, contractFixture(t, "webhooks-triggers.json")),
		"GET /api/webhooks/subscriptions": reply(200, contractFixture(t, "webhooks-subscriptions.json")),
	})
	out, err := runCommand(t, WebhooksCmd(), "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Endpoints (1)", "whe_k3j2h1g0f9  active  github", "Triggers (1)", "wtr_a1b2c3d4e5  enabled  agent_run", "whs_p9o8i7u6y5  active  approval.*,run.finished  https://example.com/hooks"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	out, err = runCommand(t, WebhooksCmd(), "list", "--json")
	if err != nil || !strings.Contains(out, `"subscriptions":[`) {
		t.Errorf("json: %q, %v", out, err)
	}
}

func TestWebhooksCreateEndpoint_PrintsTheSecretOnlyToATerminalOrWithJSON(t *testing.T) {
	_, bodies := approvalsGateway(t, handlers{
		"POST /api/webhooks/endpoints": reply(201, contractFixture(t, "webhooks-endpoint-create.json")),
	})
	_, err := runCommand(t, WebhooksCmd(), "create", "endpoint", "--service", "github")
	if err == nil || !strings.Contains(err.Error(), "standard output is not a terminal") {
		t.Fatalf("piped err = %v", err)
	}
	if len(*bodies) != 0 {
		t.Fatal("a request was sent although the secret could not be shown")
	}

	out, err := runCommand(t, WebhooksCmd(), "create", "endpoint", "--service", "github", "--description", "repo events", "--json")
	if err != nil || !strings.Contains(out, "whsec_c2VjcmV0") {
		t.Fatalf("--json: %q, %v", out, err)
	}
	if b := (*bodies)[0]; b["source_kind"] != "connector" || b["service"] != "github" || b["description"] != "repo events" {
		t.Errorf("body = %v", b)
	}

	allowSecrets(t)
	out, err = runCommand(t, WebhooksCmd(), "create", "endpoint", "--generic", "--header", "X-Sig", "--timestamp-header", "X-Ts")
	if err != nil || !strings.Contains(out, "Secret:   whsec_c2VjcmV0") || !strings.Contains(out, "URL:      https://gateway.example/api/webhooks/in/whe_k3j2h1g0f9") {
		t.Fatalf("terminal: %q, %v", out, err)
	}
	sig := (*bodies)[1]["signature"].(map[string]any)
	if sig["scheme"] != "hmac" || sig["header"] != "X-Sig" || sig["algorithm"] != "sha256" || sig["encoding"] != "hex" || sig["timestampHeader"] != "X-Ts" {
		t.Errorf("signature = %v", sig)
	}
}

func TestWebhooksCreateEndpoint_RejectsBadFlags(t *testing.T) {
	approvalsGateway(t, handlers{})
	allowSecrets(t)
	for name, args := range map[string][]string{
		"neither":         {"create", "endpoint"},
		"both":            {"create", "endpoint", "--service", "x", "--generic", "--header", "h"},
		"generic header":  {"create", "endpoint", "--generic"},
		"sub http":        {"create", "subscription", "--url", "http://example.com", "--events", "a.*"},
		"sub no events":   {"create", "subscription", "--url", "https://example.com"},
		"trigger no file": {"create", "trigger"},
	} {
		if _, err := runCommand(t, WebhooksCmd(), args...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestWebhooksCreateSubscriptionAndTrigger(t *testing.T) {
	_, bodies := approvalsGateway(t, handlers{
		"POST /api/webhooks/subscriptions": reply(201, `{"subscription":{"id":"whs_p9o8i7u6y5"},"secret":"whsec_abc"}`),
		"POST /api/webhooks/triggers":      reply(201, `{"trigger":{"id":"wtr_a1b2c3d4e5","enabled":true}}`),
	})
	allowSecrets(t)
	out, err := runCommand(t, WebhooksCmd(), "create", "subscription", "--url", "https://example.com/hooks", "--events", "approval.*,run.finished")
	if err != nil || !strings.Contains(out, "Secret:       whsec_abc") {
		t.Fatalf("subscription: %q, %v", out, err)
	}
	evs := (*bodies)[0]["event_types"].([]any)
	if len(evs) != 2 || evs[0] != "approval.*" {
		t.Errorf("event_types = %v", evs)
	}

	file := filepath.Join(t.TempDir(), "trigger.json")
	if err := os.WriteFile(file, []byte(`{"name":"t","source":{"kind":"endpoint","endpointId":"whe_k3j2h1g0f9"},"action":{"kind":"tool_call","service":"slack","tool":"post"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runCommand(t, WebhooksCmd(), "create", "trigger", "--from-file", file)
	if err != nil || !strings.Contains(out, "Trigger: wtr_a1b2c3d4e5  enabled") {
		t.Fatalf("trigger: %q, %v", out, err)
	}
	if (*bodies)[1]["name"] != "t" {
		t.Errorf("trigger body = %v", (*bodies)[1])
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`[1]`), 0o600)
	if _, err := runCommand(t, WebhooksCmd(), "create", "trigger", "--from-file", bad); err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Errorf("bad file err = %v", err)
	}
}

func TestWebhooks_TestRotateDeliveriesRedeliverEvents(t *testing.T) {
	_, bodies := approvalsGateway(t, handlers{
		"POST /api/webhooks/endpoints/whe_k3j2h1g0f9/test":                      reply(200, `{"ok":true}`),
		"POST /api/webhooks/subscriptions/whs_p9o8i7u6y5/test":                  reply(200, `{"ok":true}`),
		"POST /api/webhooks/subscriptions/whs_p9o8i7u6y5/rotate-secret":         reply(200, `{"secret":"whsec_new"}`),
		"GET /api/webhooks/subscriptions/whs_p9o8i7u6y5/deliveries?status=dead": reply(200, contractFixture(t, "webhooks-deliveries.json")),
		"POST /api/webhooks/deliveries/whd_q1w2e3r4t5/redeliver":                reply(202, `{"queued":true}`),
		"GET /api/webhooks/endpoints/whe_k3j2h1g0f9/events":                     reply(200, contractFixture(t, "webhooks-events.json")),
	})
	for _, id := range []string{"whe_k3j2h1g0f9", "whs_p9o8i7u6y5"} {
		if out, err := runCommand(t, WebhooksCmd(), "test", id); err != nil || !strings.Contains(out, "Sent a test event to "+id) {
			t.Errorf("test %s: %q, %v", id, out, err)
		}
	}
	if _, err := runCommand(t, WebhooksCmd(), "rotate-secret", "whs_p9o8i7u6y5"); err == nil {
		t.Error("a rotated secret was printed to a pipe")
	}
	allowSecrets(t)
	if out, err := runCommand(t, WebhooksCmd(), "rotate-secret", "whs_p9o8i7u6y5"); err != nil || !strings.Contains(out, "whsec_new") {
		t.Errorf("rotate: %q, %v", out, err)
	}
	out, err := runCommand(t, WebhooksCmd(), "deliveries", "whs_p9o8i7u6y5", "--status", "dead")
	if err != nil || !strings.Contains(out, "whd_q1w2e3r4t5  evt_01  run.finished        3        failed     503   HTTP 503") || !strings.Contains(out, "whd_z9x8c7v6b5") {
		t.Errorf("deliveries: %q, %v", out, err)
	}
	if out, err := runCommand(t, WebhooksCmd(), "redeliver", "whd_q1w2e3r4t5"); err != nil || !strings.Contains(out, "Queued a new attempt") {
		t.Errorf("redeliver: %q, %v", out, err)
	}
	out, err = runCommand(t, WebhooksCmd(), "events", "whe_k3j2h1g0f9")
	if err != nil || !strings.Contains(out, "wev_m1n2b3v4c5  push") || !strings.Contains(out, "fail") {
		t.Errorf("events: %q, %v", out, err)
	}
	for _, args := range [][]string{{"test", "wtr_a1b2c3d4e5"}, {"deliveries", "whe_k3j2h1g0f9"}, {"redeliver", "bogus"}} {
		if _, err := runCommand(t, WebhooksCmd(), args...); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
	_ = bodies
}

func TestWebhooks_AnAgentSessionsWriteIsHeldAndExits12(t *testing.T) {
	approvalsGateway(t, handlers{
		"POST /api/webhooks/deliveries/whd_q1w2e3r4t5/redeliver": reply(202, pendingApprovalJSON2),
	})
	out, err := runCommand(t, WebhooksCmd(), "redeliver", "whd_q1w2e3r4t5")
	if got := exitCodeOf(err); got != 12 || !strings.Contains(out, "apr_9") {
		t.Fatalf("exit %d (%v)\n%s", got, err, out)
	}
}

const pendingApprovalJSON2 = `{"status":"pending_approval","approval_id":"apr_9","service":"gateway","tool":"webhook_redeliver","risk":"write","args_digest":"d","expires_at":"2026-10-16T12:15:00.000Z"}`
