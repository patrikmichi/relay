package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func sse(events ...string) func(*packServer, http.ResponseWriter, *http.Request) {
	return func(_ *packServer, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = io.WriteString(w, "data: "+e+"\n\n")
			w.(http.Flusher).Flush()
		}
	}
}

func TestA2ACallStreamsProgress(t *testing.T) {
	gw := a2aGateway(t, routeFns{"POST /api/a2a/agents/acme-billing/messages": sse(
		`{"task":{"id":"tsk_1","state":"WORKING","message":"looking up the invoice"}}`,
		`{"task":{"id":"tsk_1","state":"WORKING","message":"drafting","artifacts":[{"parts":[{"text":"part one"}]}]}}`,
		`{"task":{"id":"tsk_1","state":"COMPLETED","message":"done","artifacts":[{"parts":[{"text":"part one"},{"text":"part two"}]}]},"final":true}`,
	)})
	out, err := runCommand(t, A2ACmd(), "call", "acme-billing", "--skill", "s", "--message", "hi", "--stream")
	if err != nil {
		t.Fatalf("stream: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[WORKING] looking up the invoice") || !strings.Contains(out, "[COMPLETED] done") || strings.Count(out, "part one") != 1 || !strings.Contains(out, "part two") {
		t.Errorf("stream output:\n%s", out)
	}
	_ = gw
	out, err = runCommand(t, A2ACmd(), "call", "acme-billing", "--skill", "s", "--message", "hi", "--stream", "--json")
	if err != nil || !strings.Contains(out, `"state":"COMPLETED"`) || strings.Contains(out, "[COMPLETED]") {
		t.Errorf("stream --json: %q %v", out, err)
	}
}

func TestA2ACallStreamEndsLikeACall(t *testing.T) {
	state := ""
	gw := a2aGateway(t, nil)
	gw.replace(func(w http.ResponseWriter, r *http.Request) {
		switch state {
		case "held":
			w.WriteHeader(202)
			_, _ = io.WriteString(w, `{"status":"pending_approval","approval_id":"apr_1","service":"a2a:x","tool":"s","risk":"outward","expires_at":"t"}`)
		case "refused":
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"error":"chain_killed"}`)
		case "plain":
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		case "garbage":
			_, _ = io.WriteString(w, "data: not json\n\n")
		case "empty":
		case "input":
			_, _ = io.WriteString(w, "data: "+`{"task":{"id":"t","state":"INPUT_REQUIRED","approval":{"approval_id":"apr_2","approval_url":"https://gw/a","effect":{"service":"s","tool":"t","risk":"outward"}}}}`+"\n\n")
		case "killed":
			_, _ = io.WriteString(w, "data: "+`{"task":{"id":"t","state":"FAILED","reason":"chain_killed"},"final":true}`+"\n\n")
		case "working":
			_, _ = io.WriteString(w, "data: "+`{"task":{"id":"t","state":"WORKING"}}`+"\n\n")
		}
	})
	for name, want := range map[string]int{"held": 12, "refused": 51, "plain": 1, "garbage": 1, "empty": 1, "input": 12, "killed": 51, "working": 17} {
		state = name
		_, err := runCommand(t, A2ACmd(), "call", "x", "--skill", "s", "--message", "hi", "--stream", "--wait", "5")
		if got := exitCodeOf(err); got != want {
			t.Errorf("%s: exit %d (%v), want %d", name, got, err, want)
		}
	}
}

const chainDoc = `{"chain":{"id":"chn_1","status":"running","origin":"patrik","depth":2,"audit":{"verified":true},
 "budget":{"limits":{"tokens":1000,"cost_eur":5,"tool_calls":10},"used":{"tokens":10,"cost_eur":0.5,"tool_calls":1}},
 "hops":[{"id":"h1","depth":1,"caller":"claude","callee":"checker","skill":"check","state":"completed"},
         {"id":"h2","parent":"h1","depth":2,"caller":"checker","callee":"acme","skill":"credit","external":true,"state":"input_required","effects":[{"service":"stripe","tool":"refund","risk":"outward","approval_id":"apr_1","outcome":"held"}]}]}}`

func TestA2AChains(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"GET /api/a2a/chains":             jsonReply(200, `{"chains":[{"id":"chn_1","status":"running","origin":"patrik","depth":2,"started_at":"t","hops":[{"id":"h1"},{"id":"h2"}]}]}`),
		"GET /api/a2a/chains/chn_1":       jsonReply(200, chainDoc),
		"POST /api/a2a/chains/chn_1/kill": jsonReply(200, `{}`),
		"POST /api/a2a/chains/chn_2/kill": jsonReply(403, `{"error":"not_yours","message":"only the origin or an administrator can stop this chain"}`),
	})
	out, err := runCommand(t, A2ACmd(), "chains", "list", "--mine", "--status", "running", "--agent", "acme")
	if err != nil || !strings.Contains(out, "chn_1") || !strings.Contains(out, "running") {
		t.Fatalf("list: %q %v", out, err)
	}
	if out, err = runCommand(t, A2ACmd(), "chains", "list", "--json"); err != nil || !strings.Contains(out, `"chains"`) {
		t.Errorf("list --json: %q %v", out, err)
	}
	out, err = runCommand(t, A2ACmd(), "chains", "show", "chn_1")
	if err != nil || !strings.Contains(out, "checker → acme.credit  EXTERNAL  input_required") || !strings.Contains(out, "effect stripe.refund  outward  held  approval apr_1") || !strings.Contains(out, "audit verified") {
		t.Errorf("show: %q %v", out, err)
	}
	if out, err = runCommand(t, A2ACmd(), "chains", "show", "chn_1", "--json"); err != nil || !strings.Contains(out, `"hops"`) {
		t.Errorf("show --json: %q %v", out, err)
	}
	if out, err = runCommand(t, A2ACmd(), "chains", "kill", "chn_1", "--reason", "wrong customer"); err != nil || !strings.Contains(out, "killed") {
		t.Errorf("kill: %q %v", out, err)
	}
	if b := gw.body("POST /api/a2a/chains/chn_1/kill", 0); b["reason"] != "wrong customer" {
		t.Errorf("kill body = %v", b)
	}
	if _, err := runCommand(t, A2ACmd(), "chains", "kill", "chn_1"); err == nil {
		t.Error("kill without a reason")
	}
	if _, err := runCommand(t, A2ACmd(), "chains", "kill", "chn_2", "--reason", "x"); err == nil || !strings.Contains(err.Error(), "origin or an administrator") {
		t.Errorf("kill refused: %v", err)
	}
	if _, err := runCommand(t, A2ACmd(), "chains", "kill", "chn_1", "--reason", "x", "--json"); err != nil {
		t.Errorf("kill --json: %v", err)
	}
	// An agent session may stop a chain.
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, A2ACmd(), "chains", "kill", "chn_1", "--reason", "runaway"); err != nil {
		t.Errorf("an agent stopping a chain: %v", err)
	}
}

func TestA2AChainsEmpty(t *testing.T) {
	a2aGateway(t, routeFns{"GET /api/a2a/chains": jsonReply(200, `{"chains":[]}`)})
	if out, err := runCommand(t, A2ACmd(), "chains", "list"); err != nil || !strings.Contains(out, "No chains match") {
		t.Errorf("empty: %q %v", out, err)
	}
}

func TestA2AGraphAndExplain(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"GET /api/a2a/graph":   jsonReply(200, `{"nodes":[{"id":"claude","kind":"mcp_client"},{"id":"acme","kind":"external","external":true}],"edges":[{"from":"claude","to":"acme","allowed":true,"rule":"clients-to-billing","calls":4,"refusals":1}]}`),
		"GET /api/a2a/explain": jsonReply(200, `{"decision":{"result":"approval","reasons":["edge allows issue_credit_note","hop is outward"],"approvals":["first use of this edge"],"ceilings":["max_risk write"],"edge":{"id":"finance-to-billing","file":"policy/a2a.yaml","line":14}}}`),
	})
	out, err := runCommand(t, A2ACmd(), "graph", "--from", "claude", "--to", "acme", "--window", "7d")
	if err != nil || !strings.Contains(out, "digraph agents") || !strings.Contains(out, `"acme" [label="acme (external)" shape=box]`) || !strings.Contains(out, "color=red") {
		t.Fatalf("graph: %q %v", out, err)
	}
	if out, err = runCommand(t, A2ACmd(), "graph", "--format", "json"); err != nil || !strings.Contains(out, `"nodes"`) {
		t.Errorf("graph json: %q %v", out, err)
	}
	if out, err = runCommand(t, A2ACmd(), "graph", "--json"); err != nil || !strings.Contains(out, `"edges"`) {
		t.Errorf("graph --json: %q %v", out, err)
	}
	if _, err := runCommand(t, A2ACmd(), "graph", "--format", "svg"); err == nil {
		t.Error("an unknown format")
	}
	if gw.count("GET /api/a2a/graph") != 3 {
		t.Errorf("graph requests = %d", gw.count("GET /api/a2a/graph"))
	}

	out, err = runCommand(t, A2ACmd(), "explain", "--from", "claude", "--to", "acme", "--skill", "issue_credit_note")
	if err != nil || !strings.Contains(out, "claude → acme.issue_credit_note: APPROVAL") || !strings.Contains(out, "decided by edge finance-to-billing (policy/a2a.yaml:14)") ||
		!strings.Contains(out, "edge allows issue_credit_note") || !strings.Contains(out, "approval needed: first use of this edge") || !strings.Contains(out, "ceiling: max_risk write") {
		t.Errorf("explain: %q %v", out, err)
	}
	if out, err = runCommand(t, A2ACmd(), "explain", "--from", "a", "--to", "b", "--skill", "c", "--json"); err != nil || !strings.Contains(out, `"decision"`) {
		t.Errorf("explain --json: %q %v", out, err)
	}
	if _, err := runCommand(t, A2ACmd(), "explain", "--from", "a"); err == nil {
		t.Error("explain without its flags")
	}
}
