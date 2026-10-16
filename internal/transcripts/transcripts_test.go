package transcripts

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const claudeCode = `{"type":"summary","summary":"old"}
{"type":"user","timestamp":"2026-10-01T10:00:00Z","cwd":"/home/ann/code/shop","version":"2.1.0","message":{"role":"user","content":"Check the open invoices"}}
{"type":"assistant","message":{"role":"assistant","model":"claude-x","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Looking."}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__zuano__fakturoid.list_invoices","input":{"status":"open"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"3 invoices"}]}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"mcp__other__ping","input":{}},{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","content":"boom","is_error":true},{"type":"image","source":{}}]}}
{"type":"assistant","isSidechain":true,"timestamp":"2026-10-01T10:05:00Z","message":{"role":"assistant","content":"sub agent says hi"}}
not json
`

func TestReadClaudeCode(t *testing.T) {
	s, err := ReadClaudeCode([]byte(claudeCode))
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != SourceClaudeCode || s.Title != "Check the open invoices" || s.CwdHint != "shop" || s.Agent.Version != "2.1.0" || s.Agent.Model != "claude-x" ||
		s.StartedAt != "2026-10-01T10:00:00Z" || s.EndedAt != "2026-10-01T10:05:00Z" {
		t.Errorf("header = %+v agent %+v", s, s.Agent)
	}
	if len(s.Turns) != 4 {
		t.Fatalf("turns = %d: %+v", len(s.Turns), s.Turns)
	}
	a := s.Turns[1]
	if a.Text != "Looking." || len(a.ToolCalls) != 3 {
		t.Fatalf("assistant turn = %+v", a)
	}
	gw := a.ToolCalls[0]
	if gw.Kind != KindGateway || gw.Service != "fakturoid" || gw.Tool != "list_invoices" || gw.ResultPreview != "3 invoices" || gw.Outcome != "ok" || gw.ArgsPreview != `{"status":"open"}` {
		t.Errorf("gateway call = %+v", gw)
	}
	if a.ToolCalls[1].Kind != KindMCPOther || a.ToolCalls[2].Kind != KindBuiltin || a.ToolCalls[2].Outcome != "error" {
		t.Errorf("calls = %+v", a.ToolCalls)
	}
	if !s.Turns[3].Sidechain || s.Stats.ToolCalls != 3 || s.Stats.DroppedParts < 3 {
		t.Errorf("sidechain/stats = %+v %+v", s.Turns[3], s.Stats)
	}
}

const codex = `{"type":"session_meta","timestamp":"2026-10-02T09:00:00Z","payload":{"cwd":"C:\\work\\api","cli_version":"0.9.0"}}
{"type":"turn_context","payload":{"model":"gpt-x"}}
{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"system"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"list prs"}]}}
{"type":"response_item","payload":{"type":"reasoning","summary":[]}}
{"type":"response_item","payload":{"type":"function_call","name":"mcp__zuano__github.list_prs","arguments":"{\"repo\":\"a/b\"}","call_id":"c1"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"2 open"}}
{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"not json","call_id":"c2"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"c2","output":{"content":"done"}}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Two are open."}]}}
{"type":"event_msg","payload":{}}
`

func TestReadCodex(t *testing.T) {
	s, err := ReadCodex([]byte(codex))
	if err != nil {
		t.Fatal(err)
	}
	if s.CwdHint != "api" || s.Agent.Version != "0.9.0" || s.Agent.Model != "gpt-x" || len(s.Turns) != 2 || s.Stats.DroppedParts != 2 {
		t.Fatalf("session = %+v", s)
	}
	calls := s.Turns[1].ToolCalls
	if len(calls) != 2 || calls[0].Kind != KindGateway || calls[0].Service != "github" || calls[0].ResultPreview != "2 open" || calls[1].ArgsPreview != "not json" || calls[1].ResultPreview != "done" {
		t.Errorf("calls = %+v", calls)
	}
}

func TestReadGemini(t *testing.T) {
	content := `[{"role":"user","parts":[{"text":"hi"},{"inlineData":{}}]},
	 {"role":"model","parts":[{"text":"checking"},{"functionCall":{"name":"zuano__slack.post","args":{"c":"x"}}}]},
	 {"role":"user","parts":[{"functionResponse":{"name":"zuano__slack.post","response":{"ok":true}}}]},
	 {"role":"system","parts":[]}]`
	s, err := ReadGemini([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Turns) != 2 || s.Turns[0].Text != "hi\n"+attachmentText {
		t.Fatalf("turns = %+v", s.Turns)
	}
	c := s.Turns[1].ToolCalls[0]
	if c.Kind != KindGateway || c.Service != "slack" || c.Tool != "post" || c.ResultPreview != `{"ok":true}` {
		t.Errorf("call = %+v", c)
	}
	checkpoint := `{"messages":[{"type":"user","content":"go"},{"type":"gemini","content":"done","toolCalls":[{"id":"x","name":"read_file","args":{"p":1},"result":"data","status":"error"}]}]}`
	s, err = ReadGemini([]byte(checkpoint))
	if err != nil || len(s.Turns) != 2 || s.Turns[1].ToolCalls[0].Outcome != "error" || s.Turns[1].ToolCalls[0].Kind != KindBuiltin {
		t.Errorf("checkpoint = %+v, %v", s, err)
	}
	for _, bad := range []string{`nope`, `{"x":1}`} {
		if _, err := ReadGemini([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestReadOpenCode(t *testing.T) {
	content := `{"info":{"directory":"/home/ann/proj"},"messages":[
	 {"info":{"role":"user","time":{"created":1790000000000}},"parts":[{"type":"text","text":"deploy notes"},{"type":"file"}]},
	 {"info":{"role":"assistant"},"parts":[{"type":"reasoning"},{"type":"text","text":"on it"},{"type":"tool","tool":"zuano_notion.append","callID":"k1","state":{"status":"error","input":{"a":1},"output":"nope"}}]},
	 {"info":{"role":"system"},"parts":[]}]}`
	s, err := ReadOpenCode([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	c := s.Turns[1].ToolCalls[0]
	if s.CwdHint != "proj" || s.StartedAt == "" || len(s.Turns) != 2 || c.Kind != KindGateway || c.Service != "notion" || c.Outcome != "error" {
		t.Errorf("session = %+v call %+v", s, c)
	}
	if _, err := ReadOpenCode([]byte(`[1]`)); err == nil {
		t.Error("an array was accepted")
	}
}

func TestReadCline(t *testing.T) {
	content := `[{"role":"user","content":[{"type":"text","text":"<task>fix it</task>"}]},
	 {"role":"assistant","content":[{"type":"tool_use","id":"u1","name":"use_mcp_tool","input":{"server_name":"zuano","tool_name":"github.get_issue","arguments":{"n":4}}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"u1","content":"issue text"}]},
	 {"role":"other","content":"x"}]`
	s, err := ReadCline([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	c := s.Turns[1].ToolCalls[0]
	if c.Kind != KindGateway || c.Service != "github" || c.ArgsPreview != `{"n":4}` || c.ResultPreview != "issue text" {
		t.Errorf("call = %+v", c)
	}
	if _, err := ReadCline([]byte(`{}`)); err == nil {
		t.Error("an object was accepted")
	}
}

const claudeAI = `[{"uuid":"u1","name":"Plan","chat_messages":[
 {"sender":"human","text":"hello","created_at":"2026-10-03T08:00:00Z"},
 {"sender":"assistant","text":"","content":[{"type":"text","text":"hi there"},{"type":"tool_use","id":"a","name":"mcp__zuano__x.y","input":{}}],"created_at":"2026-10-03T08:01:00Z"}]},
 {"uuid":"u2","name":"Other","chat_messages":[]}]`

func TestReadClaudeAIAndChoosingAConversation(t *testing.T) {
	if _, err := ReadClaudeAI([]byte(claudeAI), ""); err == nil || !strings.Contains(err.Error(), "--conversation") {
		t.Errorf("two conversations without a choice: %v", err)
	}
	s, err := ReadClaudeAI([]byte(claudeAI), "u1")
	if err != nil || s.Title != "Plan" || len(s.Turns) != 2 || s.Turns[1].ToolCalls[0].Service != "x" || s.EndedAt != "2026-10-03T08:01:00Z" {
		t.Errorf("session = %+v, %v", s, err)
	}
	if _, err := ReadClaudeAI([]byte(claudeAI), "nope"); err == nil {
		t.Error("an unknown conversation was accepted")
	}
	if _, err := ReadClaudeAI([]byte(`{}`), ""); err == nil {
		t.Error("an object was accepted")
	}
	if _, err := ReadClaudeAI([]byte(`[]`), ""); err == nil {
		t.Error("an empty export was accepted")
	}
	convs, err := Conversations(SourceClaudeAI, []byte(claudeAI))
	if err != nil || len(convs) != 2 || convs[0].ID != "u1" || convs[1].Title != "Other" {
		t.Errorf("conversations = %+v, %v", convs, err)
	}
	if _, err := Conversations(SourceClaudeAI, []byte(`nope`)); err == nil {
		t.Error("garbage listed")
	}
}

const chatGPT = `[{"id":"c1","title":"Taxes","current_node":"n4","mapping":{
 "n1":{"parent":null,"message":{"author":{"role":"system"},"content":{"parts":[""]}}},
 "n2":{"parent":"n1","message":{"author":{"role":"user"},"content":{"parts":["how do I file?"]}}},
 "n3":{"parent":"n2","message":{"author":{"role":"tool","name":"zuano"},"content":{"parts":["result text"]}}},
 "n4":{"parent":"n3","message":{"author":{"role":"assistant"},"content":{"parts":["Like this.",{"asset":1}]}}},
 "side":{"parent":"n1","message":{"author":{"role":"user"},"content":{"parts":["abandoned branch"]}}}}}]`

func TestReadChatGPTWalksTheCurrentBranch(t *testing.T) {
	s, err := ReadChatGPT([]byte(chatGPT), "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "Taxes" || len(s.Turns) != 2 || s.Turns[0].Text != "how do I file?" || strings.Contains(s.Turns[1].Text, "abandoned") {
		t.Fatalf("session = %+v", s)
	}
	if s.Turns[1].Text != "Like this.\n"+attachmentText {
		t.Errorf("assistant = %q", s.Turns[1].Text)
	}
	c := s.Turns[1].ToolCalls
	if len(c) != 1 || c[0].Kind != KindGateway || c[0].ResultPreview != "result text" {
		t.Errorf("tool = %+v", c)
	}
	if s2, err := ReadChatGPT([]byte(strings.Replace(chatGPT, `"name":"zuano"`, `"name":"browser"`, 1)), "c1"); err != nil || s2.Turns[1].ToolCalls[0].Kind != KindMCPOther {
		t.Errorf("other tool: %+v, %v", s2, err)
	}
	if _, err := ReadChatGPT([]byte(chatGPT), "zzz"); err == nil {
		t.Error("an unknown conversation was accepted")
	}
	if _, err := ReadChatGPT([]byte(`nope`), ""); err == nil {
		t.Error("garbage accepted")
	}
	if convs, _ := Conversations(SourceChatGPT, []byte(chatGPT)); len(convs) != 1 || convs[0].ID != "c1" {
		t.Errorf("conversations = %+v", convs)
	}
}

func TestReadMarkdownRoleMarkers(t *testing.T) {
	text := "## User\nPlease summarise:\n```\nUser: this is code\n```\n**Claude**: Sure.\nMore.\nYou: thanks\nAI: welcome\nYou are great at this\n"
	s := ReadMarkdown(text)
	if len(s.Turns) != 4 || s.Turns[0].Role != "user" || !strings.Contains(s.Turns[0].Text, "User: this is code") || s.Turns[1].Role != "assistant" ||
		s.Turns[1].Text != "Sure.\nMore." || s.Turns[2].Text != "thanks" || s.Turns[3].Role != "assistant" || !strings.Contains(s.Turns[3].Text, "You are great") {
		t.Errorf("turns = %+v", s.Turns)
	}
	if p := ReadMarkdown("just some notes\nwith lines"); p.Source != SourcePlainText || len(p.Warnings) != 1 || len(p.Turns) != 1 {
		t.Errorf("plain = %+v", p)
	}
	if h := ReadMarkdown("Human:\r\nhello\r\nAssistant: hi"); len(h.Turns) != 2 || h.Turns[0].Text != "hello" {
		t.Errorf("crlf = %+v", h.Turns)
	}
}

func TestDetect(t *testing.T) {
	for name, c := range map[string]struct {
		file, content, want string
	}{
		"claude jsonl":   {"a.jsonl", claudeCode, SourceClaudeCode},
		"codex jsonl":    {"rollout-1.jsonl", codex, SourceCodex},
		"codex no ext":   {"stdin", codex, SourceCodex},
		"codex one line": {"x.json", `{"type":"session_meta","payload":{}}`, SourceCodex},
		"claude one":     {"x.json", `{"type":"user","message":{"role":"user","content":"x"}}`, SourceClaudeCode},
		"claude ai":      {"conversations.json", claudeAI, SourceClaudeAI},
		"chatgpt":        {"conversations.json", chatGPT, SourceChatGPT},
		"gemini array":   {"s.json", `[{"role":"user","parts":[{"text":"x"}]}]`, SourceGemini},
		"gemini object":  {"s.json", `{"history":[]}`, SourceGemini},
		"opencode":       {"o.json", `{"messages":[{"info":{"role":"user"},"parts":[]}]}`, SourceOpenCode},
		"cline":          {"api_conversation_history.json", `[{"role":"user","content":"x"}]`, SourceCline},
		"markdown ext":   {"chat.md", "x", SourceMarkdown},
		"text ext":       {"chat.txt", "x", SourcePlainText},
		"markers":        {"paste", "User: hi\nAssistant: yo", SourceMarkdown},
		"plain":          {"paste", "hi there", SourcePlainText},
	} {
		got, err := Detect(c.file, []byte(c.content))
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v (want %s)", name, got, err, c.want)
		}
	}
	for name, c := range map[string]struct{ file, content string }{
		"empty":         {"a.json", "  "},
		"bad jsonl":     {"a.jsonl", "{nope"},
		"empty array":   {"a.json", "[]"},
		"unknown array": {"a.json", `[{"x":1}]`},
		"unknown obj":   {"a.json", `{"x":1}`},
		"broken first":  {"a", "{broken\n{}"},
	} {
		if got, err := Detect(c.file, []byte(c.content)); err == nil {
			t.Errorf("%s: detected %q", name, got)
		}
	}
}

func TestReadDispatchesBySource(t *testing.T) {
	for _, src := range Sources {
		content := map[string]string{
			SourceClaudeCode: claudeCode, SourceCodex: codex, SourceGemini: `[{"role":"user","parts":[{"text":"x"}]}]`,
			SourceOpenCode: `{"messages":[]}`, SourceCline: `[]`, SourceClaudeAI: claudeAI, SourceChatGPT: chatGPT,
			SourceMarkdown: "User: x", SourcePlainText: "x",
		}[src]
		_, err := Read(src, []byte(content), "u1")
		if src == SourceChatGPT {
			_, err = Read(src, []byte(content), "c1")
		}
		if err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
	if _, err := Read("nope", nil, ""); err == nil {
		t.Error("an unknown source was read")
	}
}

func TestTrimCanonicalAndLimits(t *testing.T) {
	var lines []string
	for i := 0; i < 450; i++ {
		lines = append(lines, `{"type":"user","message":{"role":"user","content":"m`+strings.Repeat("x", i%3)+`"}}`)
	}
	s, err := ReadClaudeCode([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Trim(0, 0); err != nil || len(s.Turns) != MaxTurns || s.Turns[0].I != 50 || s.Stats.Turns != MaxTurns {
		t.Fatalf("default trim: %d turns, first %d, %v", len(s.Turns), s.Turns[0].I, err)
	}
	if err := s.Trim(60, 70); err != nil || len(s.Turns) != 11 || s.Turns[0].I != 59 {
		t.Errorf("range: %d turns, %v", len(s.Turns), err)
	}
	for _, r := range [][2]int{{-1, 0}, {5, 3}, {1000, 0}} {
		if err := s.Trim(r[0], r[1]); err == nil {
			t.Errorf("range %v accepted", r)
		}
	}
	all, _ := ReadClaudeCode([]byte(strings.Join(lines, "\n")))
	if err := all.Trim(1, 450); err == nil {
		t.Error("a range over the limit was accepted")
	}

	long := strings.Repeat("é", MaxTurnChars+10)
	b := newBuilder(SourceMarkdown)
	b.turn("user", long, false)
	b.call(0, ToolCall{Name: "x", Kind: KindBuiltin, ArgsPreview: b.args(strings.Repeat("a", 2000))})
	if !strings.HasSuffix(b.s.Turns[0].Text, truncMarker) || b.s.Stats.TruncatedParts != 2 {
		t.Errorf("truncation: %d", b.s.Stats.TruncatedParts)
	}
	b.call(-1, ToolCall{Name: "y", Kind: KindBuiltin})
	b.result("none", "x", false)
	if len(b.s.Turns) != 2 || b.s.Turns[1].ToolCalls[0].ArgsPreview != "{}" {
		t.Errorf("turns = %+v", b.s.Turns)
	}

	s1, _ := ReadClaudeCode([]byte(claudeCode))
	a, err := s1.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := ReadClaudeCode([]byte(claudeCode))
	b2, _ := s2.Canonical()
	if string(a) != string(b2) || !strings.HasPrefix(string(a), `{"agent":`) || strings.Contains(string(a), "\n") {
		t.Errorf("canonical = %.80s", a)
	}
	var doc map[string]any
	if json.Unmarshal(a, &doc) != nil || doc["format"] != Format {
		t.Error("canonical form is not the session")
	}
}

func TestClassifyAndHint(t *testing.T) {
	for in, want := range map[string]string{
		"Bash": KindBuiltin, "mcp__zuano__a.b": KindGateway, "mcp__zuano__plain": KindGateway, "mcp__other__a": KindMCPOther, "mcp__broken": KindMCPOther,
		"zuano__a.b": KindBuiltin,
	} {
		if got := classify(in).Kind; got != want {
			t.Errorf("classify(%q) = %s, want %s", in, got, want)
		}
	}
	if c := classify("mcp__zuano__plain"); c.Tool != "plain" || c.Service != "" {
		t.Errorf("no service: %+v", c)
	}
	for in, want := range map[string]string{"zuano__a.b": KindGateway, "zuano_a.b": KindGateway, "other_a": KindBuiltin} {
		if got := nameCall(in).Kind; got != want {
			t.Errorf("nameCall(%q) = %s", in, got)
		}
	}
	for in, want := range map[string]string{`C:\work\api\`: "api", "/a/b/": "b", "solo": "solo", "": ""} {
		if got := hint(in); got != want {
			t.Errorf("hint(%q) = %q", in, got)
		}
	}
}

func TestReadFileRefusesLinksAndBigFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jsonl")
	if err := os.WriteFile(p, []byte(claudeCode), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadFile(p, 1<<20); err != nil || len(data) == 0 {
		t.Fatalf("read: %v", err)
	}
	if _, err := ReadFile(p, 10); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("big: %v", err)
	}
	if _, err := ReadFile(dir, 10); err == nil {
		t.Error("a directory was read")
	}
	if _, err := ReadFile(filepath.Join(dir, "none"), 10); err == nil {
		t.Error("a missing file was read")
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(p, link); err == nil {
		if _, err := ReadFile(link, 1<<20); err == nil {
			t.Error("a symlink was followed")
		}
	}
}

func writeAt(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverListsNewestSessionsWithinTheDataDirectories(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	writeAt(t, filepath.Join(home, ".claude", "projects", "-home-ann-shop", "s1.jsonl"), claudeCode, now.Add(-time.Hour))
	writeAt(t, filepath.Join(home, ".claude", "projects", "-home-ann-shop", "deep", "x", "s2.jsonl"), claudeCode, now)
	writeAt(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "02", "rollout-1.jsonl"), codex, now.Add(-2*time.Hour))
	writeAt(t, filepath.Join(home, ".gemini", "tmp", "abc", "chats", "session-1.json"), `[{"role":"user","parts":[{"text":"gem question"}]}]`, now.Add(-3*time.Hour))
	writeAt(t, filepath.Join(home, ".config", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "tasks", "t1", "api_conversation_history.json"), `[{"role":"user","content":"cline question"}]`, now.Add(-4*time.Hour))
	outside := filepath.Join(t.TempDir(), "secret.jsonl")
	writeAt(t, outside, claudeCode, now)
	if err := os.Symlink(outside, filepath.Join(home, ".claude", "projects", "-home-ann-shop", "escape.jsonl")); err != nil {
		t.Log("no symlinks:", err)
	}

	l := Locations{Home: home, GOOS: "linux"}
	found := Discover(l, 10, 1<<20)
	var got []string
	for _, f := range found {
		got = append(got, f.Source)
	}
	if strings.Join(got, ",") != "claude_code,codex,gemini_cli,cline" && strings.Join(got, ",") != "claude_code,claude_code,codex,gemini_cli,cline" {
		t.Fatalf("sources = %v", got)
	}
	if found[0].Preview != "Check the open invoices" || found[0].Project != "shop" {
		t.Errorf("newest = %+v", found[0])
	}
	for _, f := range found {
		if f.Source == SourceGemini && (f.Preview != "gem question" || f.Project != "chats" && f.Project != "abc") {
			t.Errorf("gemini = %+v", f)
		}
		if strings.Contains(f.Path(), "secret.jsonl") {
			t.Error("a file outside the data directory was listed")
		}
	}
	if limited := Discover(l, 2, 1<<20); len(limited) != 2 {
		t.Errorf("limit: %d", len(limited))
	}
	data, err := found[0].Open(1 << 20)
	if err != nil || len(data) == 0 {
		t.Errorf("open: %v", err)
	}
	if _, err := found[0].Open(5); err == nil {
		t.Error("open ignored the size limit")
	}
	if readable := l.Readable(); strings.Join(readable, ",") != "claude_code,codex,gemini_cli,cline" {
		t.Errorf("readable = %v", readable)
	}
	if none := Discover(Locations{Home: t.TempDir(), GOOS: "linux"}, 5, 1<<20); len(none) != 0 {
		t.Errorf("empty home: %v", none)
	}
	if bad := (Found{Root: filepath.Join(home, "missing"), Rel: "x"}); func() bool { _, err := bad.Open(10); return err == nil }() {
		t.Error("opened a file under a missing directory")
	}
}

func TestLocationsPerPlatform(t *testing.T) {
	has := func(l Locations, suffix string) bool {
		for _, d := range l.Dirs() {
			if d.Source == SourceCline && strings.HasSuffix(filepath.ToSlash(d.Path), suffix) {
				return true
			}
		}
		return false
	}
	if !has(Locations{Home: "/h", GOOS: "darwin"}, "Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/tasks") {
		t.Error("macOS path")
	}
	if !has(Locations{Home: "/h", GOOS: "windows", AppData: "/ad"}, "ad/Code/User/globalStorage/saoudrizwan.claude-dev/tasks") {
		t.Error("windows path")
	}
	for _, d := range (Locations{Home: "/h", GOOS: "windows"}).Dirs() {
		if d.Source == SourceCline {
			t.Error("windows without APPDATA lists cline")
		}
	}
	_ = runtime.GOOS
}

func TestExportOpenCodePassesTheIDAsOneArgument(t *testing.T) {
	var got string
	old := runOpenCode
	runOpenCode = func(_ context.Context, id string) ([]byte, error) { got = id; return []byte("{}"), nil }
	t.Cleanup(func() { runOpenCode = old })
	if out, err := ExportOpenCode(context.Background(), "ses_123"); err != nil || string(out) != "{}" || got != "ses_123" {
		t.Errorf("export = %q %q %v", out, got, err)
	}
	for _, bad := range []string{"", "-h", "a b", "x\ny"} {
		if _, err := ExportOpenCode(context.Background(), bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRunOpenCodeReportsAMissingProgram(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := runOpenCode(context.Background(), "ses_1"); err == nil || !strings.Contains(err.Error(), "run opencode") {
		t.Errorf("err = %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\necho oops >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	err := errors.Unwrap(errors.New("x"))
	_ = err
	if _, err := runOpenCode(context.Background(), "ses_1"); err == nil || !strings.Contains(err.Error(), "oops") {
		t.Errorf("failing program: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\necho '{\"messages\":[]}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runOpenCode(context.Background(), "ses_1"); err != nil || !strings.Contains(string(out), "messages") {
		t.Errorf("program: %q %v", out, err)
	}
}
