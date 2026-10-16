package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type routeFns = map[string]func(*packServer, http.ResponseWriter, *http.Request)

func a2aGateway(t *testing.T, extra routeFns) *packServer {
	t.Helper()
	syncHome(t)
	gw := packGateway(t, extra)
	gw.lenient = true
	return gw
}

// Characters an outside party can plant in a card to hide or reorder text.
var (
	rlo        = string(rune(0x202e))
	zeroWidth  = string(rune(0x200b))
	hidingText = strings.NewReplacer("{RLO}", rlo, "{ZW}", zeroWidth)
)

var agentsDoc = hidingText.Replace(`{"agents":[
 {"id":"agt_1","slug":"acme-billing","name":"Acme Billing","kind":"external","status":"active","card_state":"pinned","signature":"verified","pending_changes":1,"skills":[{"id":"issue_credit_note","risk":"outward","classification_source":"admin","description":"Issues a credit note.{RLO} hidden"}]},
 {"id":"agt_2","slug":"invoice-checker","name":"Invoice Checker","kind":"hosted","status":"active","card_state":"pinned","skills":[]}]}`)

func TestA2AAgentsListAndShow(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"GET /api/a2a/agents":                         jsonReply(200, agentsDoc),
		"GET /api/a2a/agents/acme-billing":            jsonReply(200, hidingText.Replace(`{"agent":{"id":"agt_1","slug":"acme-billing","name":"Acme Billing","kind":"external","status":"active","card_state":"pinned","signature":"verified","on_card_change":"block","exposure":["internal"],"endpoint":"https://gw.example/a2a/acme/acme-billing","residency":["eu"],"dlp_categories":["finance"],"pending_changes":1,"skills":[{"id":"issue_credit_note","risk":"outward","classification_source":"admin","description":"Issues a credit note.{RLO} hidden"}]}}`)),
		"GET /api/a2a/agents/acme-billing/changes":    jsonReply(200, `{"changes":[{"id":"chg_1","severity":"high","status":"pending","at":"t","summary":"New skill issue_refund"}]}`),
		"GET /api/a2a/agents/invoice-checker/changes": jsonReply(200, `{"changes":[]}`),
	})
	out, err := runCommand(t, A2ACmd(), "agents", "list")
	if err != nil || !strings.Contains(out, "acme-billing") || !strings.Contains(out, "pinned (verified)") || !strings.Contains(out, "external") {
		t.Fatalf("list: %q %v", out, err)
	}
	if out, err = runCommand(t, A2ACmd(), "agents", "list", "--kind", "external", "--external", "--json"); err != nil || !strings.Contains(out, `"agents"`) {
		t.Errorf("list filters: %q %v", out, err)
	}
	if q := gw.count("GET /api/a2a/agents"); q != 2 {
		t.Errorf("requests = %d", q)
	}
	out, err = runCommand(t, A2ACmd(), "agents", "show", "acme-billing")
	if err != nil || !strings.Contains(out, "Acme Billing (acme-billing)") || !strings.Contains(out, "issue_credit_note") || !strings.Contains(out, "outward") || strings.Contains(out, rlo) {
		t.Errorf("show: %q %v", out, err)
	}
	if out, err = runCommand(t, A2ACmd(), "agents", "show", "acme-billing", "--json"); err != nil || !strings.Contains(out, `"agent"`) {
		t.Errorf("show --json: %q %v", out, err)
	}
	if out, err = runCommand(t, A2ACmd(), "agents", "show", "a/b"); err == nil {
		t.Errorf("a slug with a slash: %q", out)
	}
	if out, err = runCommand(t, A2ACmd(), "agents", "changes", "acme-billing"); err != nil || !strings.Contains(out, "chg_1") || !strings.Contains(out, "New skill") {
		t.Errorf("changes: %q %v", out, err)
	}
	if out, err = runCommand(t, A2ACmd(), "agents", "changes", "invoice-checker"); err != nil || !strings.Contains(out, "No card changes") {
		t.Errorf("no changes: %q %v", out, err)
	}
}

func TestA2AAgentsListEmpty(t *testing.T) {
	a2aGateway(t, routeFns{"GET /api/a2a/agents": jsonReply(200, `{"agents":[]}`)})
	if out, err := runCommand(t, A2ACmd(), "agents", "list"); err != nil || !strings.Contains(out, "No agents") {
		t.Errorf("empty: %q %v", out, err)
	}
}

func TestA2ARegisterPrintsTheReviewLinkAndIsRefusedForAgents(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"POST /api/a2a/agents": jsonReply(201, `{"agent":{"slug":"acme-billing"},"review_url":"/agents/a2a/acme-billing?review=1"}`),
	})
	out, err := runCommand(t, A2ACmd(), "agents", "register", "--card-url", "https://agent.example.com/.well-known/agent-card.json", "--auth", "per-user")
	if err != nil || !strings.Contains(out, "Registered acme-billing") || !strings.Contains(out, gw.URL+"/agents/a2a/acme-billing?review=1") {
		t.Fatalf("register: %q %v", out, err)
	}
	if b := gw.body("POST /api/a2a/agents", 0); b["kind"] != "external" || b["auth"] != "per_user" || b["cardUrl"] == "" {
		t.Errorf("body = %v", b)
	}
	if _, err := runCommand(t, A2ACmd(), "agents", "register", "--card-url", "https://agent.example.com/card", "--internal"); err != nil {
		t.Fatal(err)
	}
	if b := gw.body("POST /api/a2a/agents", 1); b["kind"] != "org_service" || b["auth"] != "none" {
		t.Errorf("internal body = %v", b)
	}
	if out, err = runCommand(t, A2ACmd(), "agents", "register", "--card-url", "https://agent.example.com/card", "--json"); err != nil || !strings.Contains(out, `"review_url"`) {
		t.Errorf("--json: %q %v", out, err)
	}
	for _, args := range [][]string{
		{"agents", "register"},
		{"agents", "register", "--card-url", "not a url"},
		{"agents", "register", "--card-url", "ftp://x.example/card"},
		{"agents", "register", "--card-url", "https://x.example/card", "--auth", "magic"},
	} {
		if _, err := runCommand(t, A2ACmd(), args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, A2ACmd(), "agents", "register", "--card-url", "https://x.example/card"); exitCodeOf(err) != exitDenied {
		t.Errorf("an agent registered an agent: %v", err)
	}
	if _, err := runCommand(t, A2ACmd(), "agents", "review", "acme-billing"); exitCodeOf(err) != 14 {
		t.Errorf("an agent opened the review page: %v", err)
	}
}

func TestA2AReviewOpensThePage(t *testing.T) {
	a2aGateway(t, nil)
	out, err := runCommand(t, A2ACmd(), "agents", "review", "acme-billing", "--no-browser")
	if err != nil || !strings.Contains(out, "/agents/a2a/acme-billing") {
		t.Errorf("review: %q %v", out, err)
	}
}

func TestA2ALinkStaysOnTheGateway(t *testing.T) {
	for given, want := range map[string]string{
		"":                                  "https://gw.example/agents/a2a/x",
		"/agents/a2a/y":                     "https://gw.example/agents/a2a/y",
		"https://gw.example/agents/a2a/z":   "https://gw.example/agents/a2a/z",
		"https://evil.example/agents/a2a/z": "https://gw.example/agents/a2a/x",
		"http://gw.example/agents/a2a/z":    "https://gw.example/agents/a2a/x",
	} {
		got, err := a2aLink("https://gw.example", given, "/agents/a2a/x")
		if err != nil || got != want {
			t.Errorf("a2aLink(%q) = %q, %v; want %q", given, got, err, want)
		}
	}
}

func TestA2ACardPrintsCleanedJSON(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"GET /api/a2a/agents/acme-billing/card": jsonReply(200, hidingText.Replace(`{"card":{"name":"Acme{RLO}","skills":[{"description":"line\u0007bell{ZW}zero"}]}}`)),
	})
	out, err := runCommand(t, A2ACmd(), "card", "acme-billing")
	if err != nil || !strings.Contains(out, `"name": "Acme"`) || !strings.Contains(out, "linebellzero") {
		t.Errorf("pinned: %q %v", out, err)
	}
	if strings.ContainsAny(out, rlo+zeroWidth+"\a") {
		t.Errorf("hidden characters reached the screen: %q", out)
	}
	if _, err := runCommand(t, A2ACmd(), "card", "acme-billing", "--live"); err != nil {
		t.Fatal(err)
	}
	if gw.count("GET /api/a2a/agents/acme-billing/card") != 2 {
		t.Error("requests")
	}
}

func completed(extra string) string {
	return `{"task":{"id":"tsk_1","context_id":"ctx_1","state":"COMPLETED","message":"Credit note created.","artifacts":[{"name":"a","parts":[{"text":"CN-42"},{"file_id":"fil_abc123","name":"cn.pdf"}]}]` + extra + `}}`
}

func TestA2ACallCompletes(t *testing.T) {
	gw := a2aGateway(t, routeFns{"POST /api/a2a/agents/acme-billing/messages": jsonReply(200, completed(""))})
	msgFile := filepath.Join(t.TempDir(), "m.txt")
	_ = os.WriteFile(msgFile, []byte("please issue a credit note"), 0o600)
	out, err := runCommand(t, A2ACmd(), "call", "acme-billing", "--skill", "issue_credit_note", "--message", "@"+msgFile, "--context", "ctx_0", "--wait", "30")
	if err != nil || !strings.Contains(out, "task tsk_1  COMPLETED  context ctx_1") || !strings.Contains(out, "CN-42") || !strings.Contains(out, "file fil_abc123 (cn.pdf)") {
		t.Fatalf("call: %q %v", out, err)
	}
	b := gw.body("POST /api/a2a/agents/acme-billing/messages", 0)
	if b["skill"] != "issue_credit_note" || b["message"] != "please issue a credit note" || b["context_id"] != "ctx_0" || b["wait_seconds"] != float64(30) {
		t.Errorf("body = %v", b)
	}
	if out, err = runCommand(t, A2ACmd(), "call", "acme-billing", "--skill", "s", "--message", "hi", "--json"); err != nil || !strings.Contains(out, `"state":"COMPLETED"`) || strings.Contains(out, "task tsk_1") {
		t.Errorf("--json: %q %v", out, err)
	}
	for _, args := range [][]string{
		{"call", "acme-billing"},
		{"call", "acme-billing", "--skill", "s"},
		{"call", "acme-billing", "--skill", "s", "--message", "m", "--wait", "999"},
		{"call", "acme-billing", "--skill", "s", "--message", "@" + filepath.Join(t.TempDir(), "none")},
		{"call", "a/b", "--skill", "s", "--message", "m"},
		{"call", "acme-billing", "--skill", "s", "--message", "m", "--file", "nonsense"},
	} {
		if _, err := runCommand(t, A2ACmd(), args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	big := filepath.Join(t.TempDir(), "big.txt")
	_ = os.WriteFile(big, []byte(strings.Repeat("x", maxA2AMessage+1)), 0o600)
	if _, err := runCommand(t, A2ACmd(), "call", "acme-billing", "--skill", "s", "--message", "@"+big); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Errorf("a message over 1 MiB: %v", err)
	}
}

func TestA2ACallExitStatuses(t *testing.T) {
	state := ""
	gw := a2aGateway(t, nil)
	gw.replace(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch state {
		case "held":
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"status":"pending_approval","approval_id":"apr_1","service":"a2a:acme-billing","tool":"issue_credit_note","risk":"outward","expires_at":"2026-10-17T00:00:00Z"}`))
		case "input":
			_, _ = w.Write([]byte(`{"task":{"id":"tsk_2","state":"INPUT_REQUIRED","approval":{"approval_id":"apr_2","approval_url":"https://gw.example/approvals/apr_2","effect":{"service":"stripe","tool":"create_refund","risk":"outward"}}}}`))
		case "input-nothing":
			_, _ = w.Write([]byte(`{"task":{"id":"tsk_2","state":"INPUT_REQUIRED"}}`))
		case "auth":
			_, _ = w.Write([]byte(`{"task":{"id":"tsk_3","state":"AUTH_REQUIRED","connect_url":"https://gw.example/connect/acme"}}`))
		case "working":
			_, _ = w.Write([]byte(`{"task":{"id":"tsk_4","state":"WORKING"}}`))
		case "http-budget":
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"chain_budget_exhausted","message":"the chain used up its budget"}`))
		case "http-quota":
			w.WriteHeader(402)
			_, _ = w.Write([]byte(`{"error":"quota_exceeded","message":"a2a messages used up"}`))
		case "http-rate":
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":"rate_limited","message":"slow down"}`))
		case "http-other":
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		default:
			code := strings.TrimPrefix(state, "failed:")
			_, _ = w.Write([]byte(`{"task":{"id":"tsk_5","state":"FAILED","reason":"` + code + `","message":"refused\nbecause"}}`))
		}
	})
	for name, want := range map[string]int{
		"held": 12, "input": 12, "input-nothing": 12, "auth": 1, "working": 0,
		"http-budget": 52, "http-quota": 31, "http-rate": 13, "http-other": 1,
		"failed:a2a_loop": 50, "failed:a2a_depth_exceeded": 50, "failed:a2a_chain_limit": 50, "failed:chain_killed": 51,
		"failed:chain_budget_exhausted": 52, "failed:agent_card_changed": 53, "failed:a2a_dlp_blocked": 10, "failed:approval_denied": 10,
		"failed:approval_expired": 11, "failed:identity_suspended": 33, "failed:something_else": 1,
	} {
		state = name
		_, err := runCommand(t, A2ACmd(), "call", "acme-billing", "--skill", "s", "--message", "hi")
		if got := exitCodeOf(err); got != want {
			t.Errorf("%s: exit %d (%v), want %d", name, got, err, want)
		}
	}
}
