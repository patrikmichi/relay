package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/webhooksign"
)

func fastStream(t *testing.T) {
	t.Helper()
	old := triggerStreamBackoff
	triggerStreamBackoff = time.Millisecond
	t.Cleanup(func() { triggerStreamBackoff = old })
}

const typeJSON = `{"slug":"github.issue_opened","service":"github","title":"Issue opened","description":"A new issue is opened in the repository","kind":"webhook","latency":"realtime","config":{"type":"object","required":["owner","repo"],"properties":{"owner":{"type":"string","description":"Account"},"repo":{"type":"string"}}},"output":{"type":"object","properties":{"issue":{"type":"object"},"repository":{"type":"string","description":"owner/name"}}},"sample":{"repository":"acme/web"},"requiresEndpointSetup":false,"perRecipientAuthorization":true,"minRole":"member","registration":{"method":"api"}}`

func TestTriggersTypesAndInfo(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"GET /api/v1/trigger-types":                     jsonReply(200, `{"types":[`+typeJSON+`,{"slug":"gmail.new_message","title":"New message","kind":"poll","latency":"polling","pollIntervalSec":300}]}`),
		"GET /api/v1/trigger-types/github.issue_opened": jsonReply(200, `{"type":`+typeJSON+`}`),
	})
	out, err := runCommand(t, TriggersCmd(), "types", "--service", "github", "--kind", "webhook", "--search", "issue")
	if err != nil || !strings.Contains(out, "github.issue_opened") || !strings.Contains(out, "realtime") || !strings.Contains(out, "polling, every 5m0s") {
		t.Fatalf("types: %v\n%s", err, out)
	}
	if out, err = runCommand(t, TriggersCmd(), "types", "--json"); err != nil || !strings.Contains(out, `"types"`) {
		t.Errorf("--json: %v", err)
	}
	if gw.count("GET /api/v1/trigger-types") != 2 {
		t.Errorf("requests = %d", gw.count("GET /api/v1/trigger-types"))
	}
	out, err = runCommand(t, TriggersCmd(), "info", "github.issue_opened")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"github.issue_opened: Issue opened", "realtime (registered by api)", "each event is checked against the recipient's own account", "owner", "required", "repository", "owner/name", `"repository": "acme/web"`} {
		if !strings.Contains(out, want) {
			t.Errorf("info lacks %q:\n%s", want, out)
		}
	}
	if _, err := runCommand(t, TriggersCmd(), "info", "a/b"); err == nil {
		t.Error("a path in the type")
	}
	if out, err := runCommand(t, TriggersCmd(), "info", "github.issue_opened", "--json"); err != nil || !strings.Contains(out, `"slug"`) {
		t.Errorf("info --json: %v", err)
	}
}

func TestTriggersTypesEmpty(t *testing.T) {
	a2aGateway(t, routeFns{"GET /api/v1/trigger-types": jsonReply(200, `{"types":[]}`)})
	if out, err := runCommand(t, TriggersCmd(), "types"); err != nil || !strings.Contains(out, "No trigger types match") {
		t.Errorf("empty: %v %q", err, out)
	}
}

func TestTriggersCreateEachDestination(t *testing.T) {
	gw := a2aGateway(t, routeFns{"POST /api/v1/triggers": jsonReply(201, `{"triggerId":"wtr_1","registration":{"status":"active","method":"api"}}`)})
	cfgFile := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(cfgFile, []byte(`{"owner":"acme","labels":["bug"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		want map[string]any
	}{
		{[]string{"--automation", "atm_k3j2h1"}, map[string]any{"kind": "automation", "automationId": "atm_k3j2h1"}},
		{[]string{"--webhook", "https://example.com/hook", "--description", "mine"}, map[string]any{"kind": "subscription", "url": "https://example.com/hook", "description": "mine"}},
		{[]string{"--agent-prompt", "Triage it", "--agent", "res_1"}, map[string]any{"kind": "agent_run", "prompt": "Triage it", "agentResourceId": "res_1"}},
		{[]string{"--deliver"}, map[string]any{"kind": "deliver"}},
	}
	for i, tc := range cases {
		args := append([]string{"create", "github.issue_opened", "--config", "repo=web", "--config-file", cfgFile, "--config", "owner=override", "--account", "ca_1", "--user", "user_9"}, tc.args...)
		out, err := runCommand(t, TriggersCmd(), args...)
		if err != nil || !strings.Contains(out, "Created trigger wtr_1 (registration: active via api)") {
			t.Fatalf("%v: %v\n%s", tc.args, err, out)
		}
		b := gw.body("POST /api/v1/triggers", i)
		bind, _ := b["binding"].(map[string]any)
		cfg, _ := b["config"].(map[string]any)
		if b["type"] != "github.issue_opened" || b["connectedAccountId"] != "ca_1" || b["userId"] != "user_9" || cfg["repo"] != "web" || cfg["owner"] != "override" || cfg["labels"] == nil {
			t.Errorf("body = %v", b)
		}
		for k, v := range tc.want {
			if bind[k] != v {
				t.Errorf("binding %v = %v, want %v", k, bind, tc.want)
			}
		}
	}
}

func TestTriggersCreateRegistrationOutcomes(t *testing.T) {
	reply := `{"triggerId":"wtr_2","registration":{"status":"manual","method":"manual","manualSteps":["Open the repository settings","Add the URL as a webhook"]}}`
	gw := a2aGateway(t, routeFns{
		"POST /api/v1/triggers":                          func(_ *packServer, w http.ResponseWriter, r *http.Request) { jsonReply(201, reply)(nil, w, r) },
		"POST /api/v1/triggers/wtr_2/retry-registration": jsonReply(200, `{"registration":{"status":"active"}}`),
		"DELETE /api/v1/triggers/wtr_2":                  jsonReply(200, `{}`),
		"GET /api/v1/triggers":                           jsonReply(200, `{"triggers":[{"id":"wtr_2","type":"github.push","enabled":true,"connectedAccountId":"ca_1","binding":{"kind":"deliver"},"registration":{"status":"active"}},{"id":"wtr_3","type":"stripe.invoice_paid","enabled":false,"binding":{"kind":"subscription"},"registration":{"status":"failed"}}],"nextCursor":"c2"}`),
	})
	out, err := runCommand(t, TriggersCmd(), "create", "github.push", "--deliver")
	if err != nil || !strings.Contains(out, "1. Open the repository settings") || !strings.Contains(out, "2. Add the URL as a webhook") {
		t.Errorf("manual: %v\n%s", err, out)
	}
	reply = `{"triggerId":"wtr_3","registration":{"status":"failed","method":"api"}}`
	if _, err := runCommand(t, TriggersCmd(), "create", "github.push", "--deliver"); err == nil || !strings.Contains(err.Error(), "triggers retry wtr_3") {
		t.Errorf("failed: %v", err)
	}
	if out, err := runCommand(t, TriggersCmd(), "retry", "wtr_2"); err != nil || !strings.Contains(out, "Registration of wtr_2: active") {
		t.Errorf("retry: %v %q", err, out)
	}
	out, err = runCommand(t, TriggersCmd(), "list", "--user", "u1,u2", "--account", "ca_1", "--type", "github.push", "--auth-config", "ac_1", "--trigger-id", "wtr_2", "--show-disabled", "--cursor", "c1")
	if err != nil || !strings.Contains(out, "wtr_3  stripe.invoice_paid  off") || !strings.Contains(out, "More: --cursor c2") || !strings.Contains(out, "ca_1") {
		t.Errorf("list: %v\n%s", err, out)
	}
	if _, err := runCommand(t, TriggersCmd(), "list", "--json"); err != nil {
		t.Error(err)
	}
	if _, err := runCommand(t, TriggersCmd(), "delete", "wtr_2"); err == nil {
		t.Error("delete without --yes or a terminal")
	}
	if out, err := runCommand(t, TriggersCmd(), "delete", "wtr_2", "--yes"); err != nil || !strings.Contains(out, "Deleted wtr_2") || gw.count("DELETE /api/v1/triggers/wtr_2") != 1 {
		t.Errorf("delete: %v %q", err, out)
	}
}

func TestTriggersCreateChecksItsInput(t *testing.T) {
	gw := a2aGateway(t, routeFns{"POST /api/v1/triggers": jsonReply(201, `{}`)})
	for name, args := range map[string][]string{
		"no destination":   {"create", "github.push"},
		"two destinations": {"create", "github.push", "--deliver", "--webhook", "https://example.com"},
		"bad automation":   {"create", "github.push", "--automation", "x"},
		"webhook scheme":   {"create", "github.push", "--webhook", "ftp://example.com"},
		"webhook creds":    {"create", "github.push", "--webhook", "https://u:p@example.com"},
		"bad config":       {"create", "github.push", "--deliver", "--config", "novalue"},
		"missing file":     {"create", "github.push", "--deliver", "--config-file", filepath.Join(t.TempDir(), "none")},
	} {
		if _, err := runCommand(t, TriggersCmd(), args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	bad := filepath.Join(t.TempDir(), "a.json")
	_ = os.WriteFile(bad, []byte(`[1]`), 0o644)
	if _, err := runCommand(t, TriggersCmd(), "create", "github.push", "--deliver", "--config-file", bad); err == nil {
		t.Error("a config file that is not an object")
	}
	for _, id := range []string{"x", "wtr_", "abc_123"} {
		if _, err := runCommand(t, TriggersCmd(), "delete", id, "--yes"); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
	if gw.count("POST /api/v1/triggers") != 0 {
		t.Error("a refused command still called the gateway")
	}
}

func TestTriggersCreateByAnAgentIsHeld(t *testing.T) {
	gw := a2aGateway(t, routeFns{})
	gw.replace(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		_, _ = io.WriteString(w, `{"status":"pending_approval","approval_id":"apr_7","service":"triggers","tool":"trigger_create","risk":"outward","expires_at":"2026-10-16T13:00:00Z"}`)
	})
	t.Setenv("ZUANO_AGENT", "1")
	out, err := runCommand(t, TriggersCmd(), "create", "github.push", "--webhook", "https://example.com/h")
	if exitCodeOf(err) != exitPending || !strings.Contains(out+err.Error(), "apr_7") {
		t.Errorf("held: exit %d (%v)\n%s", exitCodeOf(err), err, out)
	}
}

// eventStream answers the stream route: first is what the first connection
// sends, then every later connection gets the next entry; headers records Last-Event-ID.
type eventStream struct {
	mu      sync.Mutex
	replies []func(w http.ResponseWriter)
	lastIDs []string
	queries []string
	n       int
}

func (e *eventStream) handler(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.lastIDs = append(e.lastIDs, r.Header.Get("Last-Event-ID"))
	e.queries = append(e.queries, r.URL.RawQuery)
	i := e.n
	e.n++
	e.mu.Unlock()
	if i >= len(e.replies) {
		i = len(e.replies) - 1
	}
	e.replies[i](w)
}

func sseFrames(frames ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
	}
}

func failWith(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func evt(id, typ string) string {
	return "id: " + id + "\nevent: " + typ + "\ndata: {\"id\":\"" + id + "\",\"type\":\"" + typ + "\",\"version\":1,\n" +
		"data: \"metadata\":{\"trigger_id\":\"wtr_1\"},\"data\":{\"n\":1}}\n\n"
}

func TestTriggersStreamPrintsJSONLinesAndResumes(t *testing.T) {
	fastStream(t)
	es := &eventStream{replies: []func(http.ResponseWriter){
		sseFrames(": heartbeat\n\n", evt("evt_1", "zuano.trigger.event"), evt("evt_2", "connection.expired")),
		failWith(503, "down"),
		sseFrames(": heartbeat\n\n", evt("evt_3", "zuano.trigger.event")),
		failWith(401, `{"error":"unauthorized"}`),
	}}
	a2aGateway(t, routeFns{"GET /api/v1/triggers/stream": func(_ *packServer, w http.ResponseWriter, r *http.Request) { es.handler(w, r) }})
	file := filepath.Join(t.TempDir(), "events.jsonl")
	out, err := runCommand(t, TriggersCmd(), "stream", "--type", "github.*", "--service", "github", "--trigger", "wtr_1", "--account", "ca_1", "--user", "u1", "--out", file)
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("the stream should end on the 401: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "{") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 3 || !strings.HasPrefix(lines[0], `{"id":"evt_1"`) || !strings.Contains(lines[2], `"evt_3"`) {
		t.Fatalf("lines = %q\n%s", lines, out)
	}
	var env map[string]any
	if json.Unmarshal([]byte(lines[1]), &env) != nil || env["version"] != 1.0 {
		t.Errorf("an event is not compact JSON: %q", lines[1])
	}
	if want := []string{"", "evt_2", "evt_2", "evt_3"}; strings.Join(es.lastIDs, ",") != strings.Join(want, ",") {
		t.Errorf("Last-Event-ID per connection = %q, want %q", es.lastIDs, want)
	}
	for _, want := range []string{"trigger_id=wtr_1", "type=github.%2A", "service=github", "connected_account_id=ca_1", "user_id=u1"} {
		if !strings.Contains(es.queries[0], want) {
			t.Errorf("query %q lacks %q", es.queries[0], want)
		}
	}
	if b, _ := os.ReadFile(file); strings.Count(string(b), "\n") != 3 {
		t.Errorf("--out = %q", b)
	}
	if !strings.Contains(out, "stream interrupted") {
		t.Errorf("the interruption was not reported:\n%s", out)
	}
}

func TestStreamHonoursRateLimitsAndStopsOnRefusals(t *testing.T) {
	fastStream(t)
	var got []string
	es := &eventStream{replies: []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":"rate_limited"}`)
		},
		sseFrames(evt("evt_9", "zuano.trigger.event")),
	}}
	gw := a2aGateway(t, routeFns{"GET /api/v1/triggers/stream": func(_ *packServer, w http.ResponseWriter, r *http.Request) { es.handler(w, r) }})
	c, err := approvalsClient(gw.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = streamTriggerEvents(ctx, c, triggerFilters{}, "", func(ev sseEvent) error {
		got = append(got, ev.ID+"|"+ev.Name)
		cancel()
		return nil
	}, func(string) {})
	if err != nil || len(got) != 1 || got[0] != "evt_9|zuano.trigger.event" {
		t.Errorf("after the 429: %v %v", err, got)
	}

	for name, status := range map[string]int{"plan": 402, "forbidden": 403} {
		es.n = 0
		es.replies = []func(http.ResponseWriter){failWith(status, `{"error":"plan_required","message":"nope"}`)}
		if status == 403 {
			es.replies = []func(http.ResponseWriter){failWith(403, `{"error":"org_suspended","message":"suspended"}`)}
		}
		var ent *client.EntitlementError
		err := streamTriggerEvents(context.Background(), c, triggerFilters{}, "", func(sseEvent) error { return nil }, func(string) {})
		if !asEntitlement(err, &ent) {
			t.Errorf("%s: %v", name, err)
		}
	}

	es.n = 0
	es.replies = []func(http.ResponseWriter){sseFrames("data: not json at all\n\n")}
	err = streamTriggerEvents(context.Background(), c, triggerFilters{}, "", func(ev sseEvent) error {
		var v any
		return json.Unmarshal(ev.Data, &v)
	}, func(string) {})
	if err == nil {
		t.Error("a handler error must end the stream")
	}

	es.n = 0
	es.replies = []func(http.ResponseWriter){failWith(404, `{"error":"not_found"}`)}
	if err := streamTriggerEvents(context.Background(), c, triggerFilters{}, "", func(sseEvent) error { return nil }, func(string) {}); err == nil {
		t.Error("a 404 must end the stream")
	}
}

func asEntitlement(err error, target **client.EntitlementError) bool {
	for err != nil {
		if e, ok := err.(*client.EntitlementError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestDevTriggersListenForwardsSignedEvents(t *testing.T) {
	fastStream(t)
	secret, err := webhooksign.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(webhookSecretEnv, secret)
	var mu sync.Mutex
	var received []string
	var verifyErrs []error
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		received = append(received, string(body))
		verifyErrs = append(verifyErrs, webhooksign.Verify(secret, r.Header, body, webhooksign.DefaultTolerance, time.Now()))
		if len(received) == 2 {
			w.WriteHeader(500)
		}
	}))
	defer local.Close()
	es := &eventStream{replies: []func(http.ResponseWriter){
		sseFrames(evt("evt_1", "zuano.trigger.event"), evt("evt_2", "zuano.trigger.event")),
		failWith(401, `{}`),
	}}
	a2aGateway(t, routeFns{"GET /api/v1/triggers/stream": func(_ *packServer, w http.ResponseWriter, r *http.Request) { es.handler(w, r) }})
	file := filepath.Join(t.TempDir(), "tee.jsonl")
	out, err := runCommand(t, DevCmd(), "triggers", "listen", "--forward", local.URL+"/hook", "--out", file, "--service", "github")
	if err == nil {
		t.Fatal("the 401 should end the command")
	}
	if len(received) != 2 || verifyErrs[0] != nil || verifyErrs[1] != nil {
		t.Fatalf("received %d, verify %v", len(received), verifyErrs)
	}
	if !strings.Contains(received[0], `"id":"evt_1"`) || strings.Contains(received[0], "\n") {
		t.Errorf("body = %q", received[0])
	}
	for _, want := range []string{"zuano.trigger.event evt_1 -> 200", "zuano.trigger.event evt_2 -> 500"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, secret) {
		t.Error("a secret given by the environment was printed")
	}
	if b, _ := os.ReadFile(file); strings.Count(string(b), "\n") != 2 {
		t.Errorf("--out = %q", b)
	}

	// Replay re-sends the same ids without opening the stream.
	received, verifyErrs = nil, nil
	out, err = runCommand(t, DevCmd(), "triggers", "listen", "--forward", local.URL, "--replay", file)
	if err != nil || len(received) != 2 || verifyErrs[0] != nil || !strings.Contains(out, "evt_2") {
		t.Errorf("replay: %v received %d\n%s", err, len(received), out)
	}
}

func TestDevTriggersListenGeneratesAndChecksSecrets(t *testing.T) {
	t.Setenv(webhookSecretEnv, "")
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer local.Close()
	file := filepath.Join(t.TempDir(), "e.jsonl")
	_ = os.WriteFile(file, []byte(`{"id":"evt_1","type":"x"}`+"\n"), 0o644)
	out, err := runCommand(t, DevCmd(), "triggers", "listen", "--forward", local.URL, "--replay", file)
	if err != nil || !strings.Contains(out, "Signing secret (shown once): whsec_") {
		t.Errorf("generated: %v\n%s", err, out)
	}
	t.Setenv(webhookSecretEnv, "not-a-secret!")
	if _, err := runCommand(t, DevCmd(), "triggers", "listen", "--forward", local.URL, "--replay", file); err == nil || !strings.Contains(err.Error(), webhookSecretEnv) {
		t.Errorf("a bad secret: %v", err)
	}
	t.Setenv(webhookSecretEnv, "")
	for name, args := range map[string][]string{
		"no forward":   {"triggers", "listen"},
		"remote host":  {"triggers", "listen", "--forward", "https://example.com/hook", "--replay", file},
		"bad scheme":   {"triggers", "listen", "--forward", "ftp://localhost/x", "--replay", file},
		"credentials":  {"triggers", "listen", "--forward", "http://u:p@localhost/x", "--replay", file},
		"empty replay": {"triggers", "listen", "--forward", local.URL, "--replay", writeEmpty(t)},
		"no envelope":  {"triggers", "listen", "--forward", local.URL, "--replay", writeLine(t, `{"nope":1}`)},
		"missing file": {"triggers", "listen", "--forward", local.URL, "--replay", filepath.Join(t.TempDir(), "none")},
	} {
		if _, err := runCommand(t, DevCmd(), args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := runCommand(t, DevCmd(), "triggers", "listen", "--forward", "http://127.0.0.1:1/x", "--replay", file); err != nil {
		t.Errorf("an unreachable local server is reported per event, not fatal: %v", err)
	}
	if !isLoopbackHost("app.localhost") || isLoopbackHost("10.0.0.5") || !isLoopbackHost("::1") {
		t.Error("isLoopbackHost")
	}
	_ = atomic.Int32{}
}

func writeEmpty(t *testing.T) string { return writeLine(t, "\n") }

func writeLine(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "l.jsonl")
	if err := os.WriteFile(p, []byte(s+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGenerateWritesTypes(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"GET /api/v1/trigger-types": jsonReply(200, `{"types":[{"slug":"github.push","output":{"type":"object","required":["ref"],"properties":{"ref":{"type":"string"}}}},{"slug":"github.no_schema"}]}`),
		"GET /api/integrations":     jsonReply(200, `{"ok":true,"services":[{"id":"github","name":"GitHub","tools":[{"name":"create_issue","inputSchema":{"type":"object","required":["title"],"properties":{"title":{"type":"string"}}}},{"name":"bare"}]},{"id":"other","tools":[{"name":"x","inputSchema":{"type":"object"}}]}]}`),
	})
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("package.json", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCommand(t, GenerateCmd(), "--services", "github,missing")
	if err != nil || !strings.Contains(out, "Wrote 1 trigger payload(s) and 1 tool input(s) to zuano.gen.ts") || !strings.Contains(out, `No tools found for "missing"`) {
		t.Fatalf("generate: %v\n%s", err, out)
	}
	src, _ := os.ReadFile("zuano.gen.ts")
	for _, want := range []string{"Generated by zuano generate. Do not edit.", `"github.push": GithubPushPayload;`, `"github.create_issue": GithubCreateIssueInput;`, "ref: string;", "title: string;"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("zuano.gen.ts lacks %q:\n%s", want, src)
		}
	}
	if gw.count("GET /api/v1/trigger-types") != 2 {
		t.Errorf("one catalog request per service, got %d", gw.count("GET /api/v1/trigger-types"))
	}
	// Run again: our own file is replaced; someone else's is not.
	if _, err := runCommand(t, GenerateCmd(), "--services", "github", "--no-tools"); err != nil {
		t.Errorf("regenerate: %v", err)
	}
	if err := os.WriteFile("mine.ts", []byte("export const x = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, GenerateCmd(), "--services", "github", "--out", "mine.ts"); err == nil || !strings.Contains(err.Error(), "not written by zuano") {
		t.Errorf("overwrote a foreign file: %v", err)
	}
	if _, err := runCommand(t, GenerateCmd(), "--services", "github", "--out", "mine.ts", "--force"); err != nil {
		t.Errorf("--force: %v", err)
	}
	out, err = runCommand(t, GenerateCmd(), "--services", "github", "--lang", "py", "--out", "gen/zuano_gen.py", "--no-triggers")
	if err != nil || !strings.Contains(out, "Wrote 0 trigger payload(s) and 1 tool input(s)") {
		t.Errorf("python: %v\n%s", err, out)
	}
	if py, _ := os.ReadFile("gen/zuano_gen.py"); !strings.Contains(string(py), "from typing import") {
		t.Errorf("python file = %q", py)
	}
}

func TestGenerateDetectsTheLanguageAndChecksInput(t *testing.T) {
	a2aGateway(t, routeFns{
		"GET /api/v1/trigger-types": jsonReply(200, `{"types":[]}`),
		"GET /api/integrations":     jsonReply(200, `{"ok":true,"services":[]}`),
	})
	t.Chdir(t.TempDir())
	for name, args := range map[string][]string{
		"no services":   {},
		"bad service":   {"--services", "a/b"},
		"no project":    {"--services", "github"},
		"bad lang":      {"--services", "github", "--lang", "rust"},
		"nothing found": {"--services", "github", "--lang", "ts"},
	} {
		if _, err := runCommand(t, GenerateCmd(), args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	_ = os.WriteFile("package.json", []byte("{}"), 0o644)
	_ = os.WriteFile("pyproject.toml", []byte(""), 0o644)
	if _, err := runCommand(t, GenerateCmd(), "--services", "github"); err == nil || !strings.Contains(err.Error(), "--lang") {
		t.Errorf("both languages: %v", err)
	}
	_ = os.Remove("package.json")
	if lang, err := generateLanguage(""); err != nil || lang != "py" {
		t.Errorf("python project: %q %v", lang, err)
	}
	if lang, _ := generateLanguage("typescript"); lang != "ts" {
		t.Error("typescript alias")
	}
}
