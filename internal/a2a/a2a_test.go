package a2a

import (
	"bytes"
	"strings"
	"testing"
)

func sampleChain() Chain {
	c := Chain{ID: "chn_1", Status: "running", Origin: "patrik@example.com", Depth: 2}
	c.Budget = Budget{Limits: Counters{Tokens: 400000, CostEUR: 5, ToolCalls: 200}, Used: Counters{Tokens: 1200, CostEUR: 0.12, ToolCalls: 3}}
	c.Audit.Verified = true
	c.Hops = []Hop{
		{ID: "h2", Parent: "h1", Depth: 2, Caller: "invoice-checker", Callee: "acme-billing", Skill: "issue_credit_note", External: true, State: "input_required", DurationMs: 1500, Tokens: 300, CostEUR: 0.02, DLPFindings: 2, InjectionFlags: 1,
			Effects: []Effect{{Service: "stripe", Tool: "create_refund", Risk: "outward", Approval: "apr_9", Outcome: "held"}}},
		{ID: "h1", Parent: "", Depth: 1, Caller: "claude", Callee: "invoice-checker", Skill: "check", State: "completed", DurationMs: 900, Tokens: 900, CostEUR: 0.1},
		{ID: "h3", Parent: "missing", Depth: 1, Caller: "claude", Callee: "orphan", Skill: "x", State: "completed"},
	}
	return c
}

func TestChainRendersAsATree(t *testing.T) {
	var b bytes.Buffer
	sampleChain().Render(&b)
	out := b.String()
	for _, want := range []string{
		"chain chn_1  running  origin patrik@example.com  depth 2  audit verified",
		"budget: tokens 1200 of 400000, cost 0.12 of 5.00 EUR, tool calls 3 of 200",
		"claude → invoice-checker.check  internal  completed",
		"│  └─ invoice-checker → acme-billing.issue_credit_note  EXTERNAL  input_required  1.5s  tokens 300  0.02 EUR  dlp 2  injection 1",
		"effect stripe.create_refund  outward  held  approval apr_9",
		"claude → orphan.x",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendering lacks %q:\n%s", want, out)
		}
	}
	// A cycle in the data does not loop forever.
	loop := Chain{ID: "c", Hops: []Hop{{ID: "a", Parent: "b"}, {ID: "b", Parent: "a"}}}
	var l bytes.Buffer
	loop.Render(&l)
	// Hops whose parent is known but never reached from a root are not shown twice.
	if strings.Count(l.String(), "→") > 2 {
		t.Errorf("cycle output: %s", l.String())
	}
	var bare bytes.Buffer
	Chain{ID: "x", Status: "killed"}.Render(&bare)
	if !strings.Contains(bare.String(), "audit not verified") || strings.Contains(bare.String(), "budget:") {
		t.Errorf("bare chain: %s", bare.String())
	}
}

func TestChainRenderingCleansWhatAgentsWrote(t *testing.T) {
	c := Chain{ID: "c", Hops: []Hop{{ID: "a", Caller: "evil\x1b[2J\u202e", Callee: "x\r\ny", Skill: "s", State: "ok"}}}
	var b bytes.Buffer
	c.Render(&b)
	if strings.ContainsAny(b.String(), "\x1b\r\u202e") {
		t.Errorf("control characters reached the screen: %q", b.String())
	}
}

func TestGraphDOT(t *testing.T) {
	g := Graph{
		Nodes: []Node{{ID: "claude", Name: "Claude", Kind: "mcp_client"}, {ID: "acme", Kind: "external", External: true}},
		Edges: []Edge{{From: "claude", To: "acme", Allowed: true, Rule: "clients-to-billing"}, {From: "claude", To: "acme2", Calls: 12, Refusals: 2, Rule: "r"}},
	}
	dot := g.DOT()
	for _, want := range []string{`digraph agents`, `"acme" [label="acme (external)" shape=box]`, `"claude" [label="Claude (mcp_client)" shape=ellipse]`, `label="clients-to-billing" style=dashed`, `label="r 12 calls" color=red`} {
		if !strings.Contains(dot, want) {
			t.Errorf("DOT lacks %q:\n%s", want, dot)
		}
	}
	if !strings.Contains(Graph{Nodes: []Node{{ID: `a"b`, Kind: "x"}}}.DOT(), `"a\"b"`) {
		t.Error("a quote in an id was not escaped")
	}
}

func TestTaskHelpers(t *testing.T) {
	task := Task{State: "COMPLETED", Message: "done", Artifacts: []Artifact{{Parts: []Part{{Text: "a"}, {FileID: "fil_1"}, {Text: "b"}}}}}
	if !task.Terminal() || strings.Join(task.Texts(), ",") != "done,a,b" {
		t.Errorf("task = %+v texts %v", task, task.Texts())
	}
	for state, want := range map[string]bool{"COMPLETED": true, "FAILED": true, "CANCELED": true, "REJECTED": true, "WORKING": false, "INPUT_REQUIRED": false, "": false} {
		if (Task{State: state}).Terminal() != want {
			t.Errorf("Terminal(%s) = %v", state, !want)
		}
	}
	if !(Agent{Kind: "external"}).External() || (Agent{Kind: "hosted"}).External() {
		t.Error("External")
	}
	if dur(0) != "-" || dur(2500) != "2.5s" {
		t.Errorf("dur = %s %s", dur(0), dur(2500))
	}
}
