package evals

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrikmichi/zuano/internal/brand"
)

// fake is a Provider answered by a function; it records the requests.
type fake struct {
	mu   sync.Mutex
	fn   func(ChatRequest) (ChatResponse, error)
	reqs []ChatRequest
}

func (f *fake) ID() string { return "fake" }
func (f *fake) Complete(_ context.Context, r ChatRequest) (ChatResponse, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.mu.Unlock()
	return f.fn(r)
}

func text(s string) func(ChatRequest) (ChatResponse, error) {
	return func(ChatRequest) (ChatResponse, error) {
		return ChatResponse{Text: s, Usage: Usage{InputTokens: 10, OutputTokens: 5}}, nil
	}
}

func testCell(f *fake) Cell {
	return Cell{Model: ModelRef{"anthropic", "m"}, Provider: f}
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const skillFile = "---\nname: pdf\ndescription: Merge and split PDF files\n---\nUse the merge tool.\n"

func TestSkillHarnessTrigger(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "SKILL.md", skillFile)
	writeFile(t, dir, "catalog.json", `[{"name":"docx","description":"Edit Word documents"},{"name":"pdf","description":"stale copy"}]`)
	cfg := &Config{Dir: dir}
	h, err := NewHarness(cfg, &Suite{Target: Target{Kind: "skill", Path: "SKILL.md", Catalog: "catalog.json"}}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if h.Subject() != "pdf" || h.Digest() == "" {
		t.Errorf("subject %q digest %q", h.Subject(), h.Digest())
	}
	f := &fake{fn: text(`{"choice": "pdf"}`)}
	yes := true
	out := h.Run(context.Background(), Case{Kind: KindTrigger, Name: "n", Input: Input{Text: "merge these"}, ShouldTrigger: &yes}, testCell(f))
	if out.Choice == nil || *out.Choice != "pdf" || out.Usage.InputTokens != 10 {
		t.Fatalf("out = %+v", out)
	}
	sys := f.reqs[0].System
	if !strings.Contains(sys, "Merge and split PDF files") || strings.Contains(sys, "stale copy") || strings.Index(sys, "docx") > strings.Index(sys, "pdf") {
		t.Errorf("router prompt:\n%s", sys)
	}
	f.fn = text(`{"choice": null}`)
	if o := h.Run(context.Background(), Case{Kind: KindTrigger, Name: "n", Input: Input{Text: "x"}, ShouldTrigger: &yes}, testCell(f)); o.Choice != nil {
		t.Errorf("a null choice = %v", *o.Choice)
	}
	f.fn = func(ChatRequest) (ChatResponse, error) {
		return ChatResponse{}, &CaseError{Kind: ErrRateLimited, Message: "slow down"}
	}
	if o := h.Run(context.Background(), Case{Kind: KindTrigger, Name: "n", Input: Input{Text: "x"}, ShouldTrigger: &yes}, testCell(f)); o.Error == nil || o.Error.Kind != ErrRateLimited {
		t.Errorf("error = %+v", o.Error)
	}
}

func TestSkillHarnessTaskLoopAnswersFromFixtures(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "SKILL.md", skillFile)
	h, err := NewHarness(&Config{Dir: dir}, &Suite{Target: Target{Kind: "skill", Path: "SKILL.md"}}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	step := 0
	f := &fake{fn: func(r ChatRequest) (ChatResponse, error) {
		step++
		if step == 1 {
			return ChatResponse{ToolCalls: []ToolCall{{ID: "1", Name: "invoice", Args: map[string]any{"id": "42"}}, {ID: "2", Name: "invoice", Args: map[string]any{"id": "99"}}, {ID: "3", Name: "failing"}}}, nil
		}
		return ChatResponse{Text: "total 1 250", Usage: Usage{InputTokens: 1, OutputTokens: 1}}, nil
	}}
	c := Case{Kind: KindTask, Name: "t", Input: Input{Text: "total of invoice 42?"}, Fixtures: []Fixture{
		{Tool: "invoice", Match: map[string]any{"id": "42"}, Response: map[string]any{"total": 1250}},
		{Tool: "failing", Error: "boom"},
	}}
	out := h.Run(context.Background(), c, testCell(f))
	if out.Error != nil || out.Text != "total 1 250" || len(out.ToolCalls) != 3 {
		t.Fatalf("out = %+v", out)
	}
	last := f.reqs[1].Turns
	answers := []string{last[2].Text, last[3].Text, last[4].Text}
	if !strings.Contains(answers[0], "1250") || answers[1] != fixtureMissing || answers[2] != "error: boom" {
		t.Errorf("tool answers = %q", answers)
	}
	if !strings.Contains(f.reqs[0].System, "Use the merge tool") || len(f.reqs[0].Tools) != 2 {
		t.Errorf("first request = %+v", f.reqs[0])
	}

	// A model that never stops calling tools ends in a model error.
	f2 := &fake{fn: func(ChatRequest) (ChatResponse, error) {
		return ChatResponse{ToolCalls: []ToolCall{{ID: "x", Name: "invoice"}}}, nil
	}}
	if o := h.Run(context.Background(), c, testCell(f2)); o.Error == nil || o.Error.Kind != ErrModel {
		t.Errorf("runaway loop: %+v", o.Error)
	}
}

func TestPromptHarnessRendersTemplates(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "system.md", "You are a {{role}} assistant.")
	h, err := NewHarness(&Config{Dir: dir}, &Suite{Target: Target{Kind: "prompt", Path: "system.md", Template: "Question: {{q}}"}}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{fn: text("fine")}
	c := Case{Kind: KindTask, Name: "n", Input: Input{Text: "q", Variables: map[string]string{"role": "pirate", "q": "why?"}}}
	if out := h.Run(context.Background(), c, testCell(f)); out.Error != nil || f.reqs[0].System != "You are a pirate assistant." || f.reqs[0].Turns[0].Text != "Question: why?" {
		t.Fatalf("out %+v req %+v", out, f.reqs[0])
	}
	c.Input.Variables = map[string]string{"q": "x"}
	if out := h.Run(context.Background(), c, testCell(f)); out.Error == nil || out.Error.Kind != ErrSetup || !strings.Contains(out.Error.Message, "role") {
		t.Errorf("a missing variable: %+v", out.Error)
	}
	if _, err := NewHarness(&Config{Dir: dir}, &Suite{Target: Target{Kind: "prompt"}}, env(nil)); err == nil {
		t.Error("an empty prompt target")
	}
	multi := Case{Kind: KindTask, Name: "m", Input: Input{Messages: []Message{{"user", "hi"}, {"assistant", "hello"}, {"user", "and?"}}}}
	hp, _ := NewHarness(&Config{Dir: dir}, &Suite{Target: Target{Kind: "prompt", System: "plain"}}, env(nil))
	hp.Run(context.Background(), multi, testCell(f))
	if got := f.reqs[len(f.reqs)-1].Turns; len(got) != 3 || got[1].Role != "assistant" {
		t.Errorf("multi-turn = %+v", got)
	}
}

func TestAgentHarnessLimitsToolsToTheFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "reviewer.md", "---\nname: reviewer\ndescription: Reviews pull requests\ntools: Read, Grep\n---\nReview carefully.\n")
	writeFile(t, dir, "none.md", "---\nname: planner\ndescription: Plans\ntools: []\n---\nPlan.\n")
	writeFile(t, dir, "all.md", "---\nname: free\ndescription: Anything\n---\nDo.\n")
	cfg := &Config{Dir: dir}
	fixtures := []Fixture{{Tool: "Read", Response: "file"}, {Tool: "Bash", Response: "ran"}}
	run := func(file string) (Output, *fake) {
		h, err := NewHarness(cfg, &Suite{Target: Target{Kind: "agent_config", Path: file}}, env(nil))
		if err != nil {
			t.Fatal(err)
		}
		step := 0
		f := &fake{fn: func(ChatRequest) (ChatResponse, error) {
			step++
			if step == 1 {
				return ChatResponse{ToolCalls: []ToolCall{{ID: "1", Name: "Bash"}}}, nil
			}
			return ChatResponse{Text: "done"}, nil
		}}
		return h.Run(context.Background(), Case{Kind: KindTask, Name: "n", Input: Input{Text: "go"}, Fixtures: fixtures}, testCell(f)), f
	}
	_, f := run("reviewer.md")
	if len(f.reqs[0].Tools) != 1 || f.reqs[0].Tools[0].Name != "Read" || f.reqs[1].Turns[2].Text != "error: this tool is not available" {
		t.Errorf("restricted: tools %v answer %q", f.reqs[0].Tools, f.reqs[1].Turns[2].Text)
	}
	_, f = run("none.md")
	if len(f.reqs[0].Tools) != 0 {
		t.Errorf("an empty list still offered tools: %v", f.reqs[0].Tools)
	}
	_, f = run("all.md")
	if len(f.reqs[0].Tools) != 2 || f.reqs[1].Turns[2].Text != "ran" {
		t.Errorf("unrestricted: tools %v answer %q", f.reqs[0].Tools, f.reqs[1].Turns[2].Text)
	}

	writeFile(t, dir, "cat.json", `[{"name":"tester","description":"Writes tests"}]`)
	h, err := NewHarness(cfg, &Suite{Target: Target{Kind: "agent_config", Path: "reviewer.md", Catalog: "cat.json"}}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	no := false
	fr := &fake{fn: text(`{"choice":"tester"}`)}
	out := h.Run(context.Background(), Case{Kind: KindTrigger, Name: "n", Input: Input{Text: "write tests"}, ShouldTrigger: &no}, testCell(fr))
	if CheckTrigger(Case{ShouldTrigger: &no}, h.Subject(), out).Pass != true || !strings.Contains(fr.reqs[0].System, "Reviews pull requests") {
		t.Errorf("delegation = %+v", out)
	}
}

func TestMCPHarnessChoosesATool(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "tools.json", `{"result":{"tools":[{"name":"create_invoice","description":"Create","inputSchema":{"type":"object"}},{"name":"list_invoices","description":"List"}]}}`)
	h, err := NewHarness(&Config{Dir: dir}, &Suite{Target: Target{Kind: "mcp_server", Tools: "tools.json"}}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{fn: func(ChatRequest) (ChatResponse, error) {
		return ChatResponse{ToolCalls: []ToolCall{{Name: "list_invoices", Args: map[string]any{"limit": 5.0}}}}, nil
	}}
	out := h.Run(context.Background(), Case{Kind: KindTrigger, Name: "n", Input: Input{Text: "show invoices"}}, testCell(f))
	if out.Choice == nil || *out.Choice != "list_invoices" || len(f.reqs[0].Tools) != 2 || f.reqs[0].Tools[0].Schema == nil {
		t.Fatalf("out = %+v", out)
	}
	f.fn = text("no tool fits")
	if out := h.Run(context.Background(), Case{Kind: KindTrigger, Name: "n", Input: Input{Text: "x"}}, testCell(f)); out.Choice != nil {
		t.Errorf("no tool called but choice = %v", *out.Choice)
	}
	if out := h.Run(context.Background(), Case{Kind: KindTask, Name: "n", Input: Input{Text: "x"}}, testCell(f)); out.Text != "no tool fits" {
		t.Errorf("task = %+v", out)
	}
	f.fn = func(ChatRequest) (ChatResponse, error) { return ChatResponse{}, errors.New("plain failure") }
	if out := h.Run(context.Background(), Case{Kind: KindTrigger, Name: "n", Input: Input{Text: "x"}}, testCell(f)); out.Error == nil || out.Error.Kind != ErrTarget {
		t.Errorf("failure = %+v", out.Error)
	}

	for name, body := range map[string]string{"empty": `[]`, "nameless": `[{"description":"x"}]`, "junk": `{"tools": 3}`} {
		if _, err := ParseToolList([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if tools, err := ParseToolList([]byte(`[{"name":"a"}]`)); err != nil || len(tools) != 1 {
		t.Errorf("bare list: %v %v", tools, err)
	}
}

func TestAppHarnessSignsAndRetries(t *testing.T) {
	secret := "shared-" + strings.Repeat("x", 4)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		hdr := r.Header.Get(brand.SignatureHeader)
		var ts string
		var sig string
		for _, part := range strings.Split(hdr, ",") {
			k, v, _ := strings.Cut(part, "=")
			switch k {
			case "t":
				ts = v
			case "v1":
				sig = v
			}
		}
		mac := hmac.New(sha256.New, []byte(secret))
		fmt.Fprintf(mac, "%s.", ts)
		mac.Write(body)
		if sig != hex.EncodeToString(mac.Sum(nil)) {
			w.WriteHeader(401)
			return
		}
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		_ = json.NewEncoder(w).Encode(map[string]any{"output": "echo " + in["input"].(string), "tool_calls": []any{map[string]any{"name": "t"}}, "usage": map[string]int{"input_tokens": 4, "output_tokens": 2}})
	}))
	defer srv.Close()
	h, err := NewHarness(&Config{}, &Suite{Target: Target{Kind: "app", URL: srv.URL, SecretEnv: "HOOK_SECRET"}}, env(map[string]string{"HOOK_SECRET": secret}))
	if err != nil {
		t.Fatal(err)
	}
	out := h.Run(context.Background(), Case{Kind: KindTask, Name: "n", Input: Input{Text: "hi"}}, testCell(&fake{}))
	if out.Error != nil || out.Text != "echo hi" || len(out.ToolCalls) != 1 || out.Usage.OutputTokens != 2 || calls != 2 {
		t.Fatalf("out = %+v after %d calls", out, calls)
	}
	if _, err := NewHarness(&Config{}, &Suite{Target: Target{Kind: "app", URL: srv.URL, SecretEnv: "MISSING"}}, env(nil)); err == nil {
		t.Error("an empty signing secret")
	}
	if _, err := NewHarness(&Config{}, &Suite{Target: Target{Kind: "app"}}, env(nil)); err == nil {
		t.Error("no url")
	}
	if !strings.HasPrefix(Sign([]byte("k"), time.Unix(5, 0), []byte("b")), "t=5,v1=") {
		t.Error("Sign format")
	}
}

func TestAppHarnessReportsHookFailures(t *testing.T) {
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "not json")
	}))
	defer srv.Close()
	h, _ := NewHarness(&Config{}, &Suite{Target: Target{Kind: "app", URL: srv.URL}}, env(nil))
	run := func() *CaseError {
		return h.Run(context.Background(), Case{Kind: KindTask, Name: "n", Input: Input{Text: "hi"}}, testCell(&fake{})).Error
	}
	if e := run(); e == nil || e.Kind != ErrTarget {
		t.Errorf("non-JSON: %v", e)
	}
	status = 404
	if e := run(); e == nil || !strings.Contains(e.Message, "404") {
		t.Errorf("404: %v", e)
	}
	status = 500
	if e := run(); e == nil || !strings.Contains(e.Message, "500") {
		t.Errorf("500: %v", e)
	}
	srv.Close()
	if e := run(); e == nil {
		t.Error("a closed hook")
	}
}

func TestHarnessSetupErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "noname.md", "---\ndescription: x\n---\nbody")
	writeFile(t, dir, "badcat.json", "{}")
	writeFile(t, dir, "ok.md", skillFile)
	cfg := &Config{Dir: dir}
	for name, tgt := range map[string]Target{
		"cloud only":   {Kind: "agent_endpoint"},
		"no path":      {Kind: "skill"},
		"missing file": {Kind: "skill", Path: "zzz.md"},
		"no name":      {Kind: "skill", Path: "noname.md"},
		"bad catalog":  {Kind: "skill", Path: "ok.md", Catalog: "badcat.json"},
		"agent noname": {Kind: "agent_config", Path: "noname.md"},
		"mcp no tools": {Kind: "mcp_server"},
		"mcp bad":      {Kind: "mcp_server", Tools: "badcat.json"},
	} {
		if _, err := NewHarness(cfg, &Suite{Target: tgt}, env(nil)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if body, err := ParseFrontmatter([]byte("---\nname: [\n---\nbody"), &struct{}{}); err == nil {
		t.Errorf("broken front matter: %q", body)
	}
	if body, _ := ParseFrontmatter([]byte("no header"), &struct{}{}); body != "no header" {
		t.Errorf("headerless = %q", body)
	}
}
