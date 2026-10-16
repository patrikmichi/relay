package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func fakeKey() string { return strings.Join([]string{"sk", "-", strings.Repeat("a", 24)}, "") }

func sessionFile(t *testing.T) string {
	t.Helper()
	content := `{"type":"user","timestamp":"2026-10-01T10:00:00Z","cwd":"/home/ann/shop","message":{"role":"user","content":"deploy with key ` + fakeKey() + ` and mail ann@example.com"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"},{"type":"tool_use","id":"t1","name":"Bash","input":{"password":"hunter2hunter2"}}]}}
{"type":"user","message":{"role":"user","content":"thanks"}}
`
	p := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sessionRoutes(extStatuses ...string) routeMap {
	n := 0
	return routeMap{
		"GET /api/skills/from-session/consent":                 jsonReply(200, `{"statementVersion":"v1","text":"I may share this."}`),
		"POST /api/skills/from-session/captures":               jsonReply(201, `{"captureId":"cap_1","turns":3,"toolCalls":1,"expiresAt":"2026-10-23","redaction":{"counts":{"secret":2},"placeholders":3}}`),
		"POST /api/skills/from-session/captures/cap_1/extract": jsonReply(202, `{"extractionId":"ext_1","jobId":"job_1"}`),
		"GET /api/skills/from-session/extractions/ext_1": func(_ *packServer, w http.ResponseWriter, _ *http.Request) {
			st := "ready"
			if n < len(extStatuses) {
				st = extStatuses[n]
			}
			n++
			_, _ = io.WriteString(w, `{"extraction":{"id":"ext_1","status":"`+st+`","error":"model failed","injectionFlag":true,"proposal":{"name":"deploy-check","description":"Checks a deploy","connectors":["github"],"cases":[{"k":1},{"k":2}],"warnings":["thin transcript"]}}}`)
		},
		"POST /api/skills/from-session/extractions/ext_1/accept": jsonReply(200, `{"resourceId":"res_1","versionId":"rv_1","setId":"set_1","catalogUrl":"/skills/res_1"}`),
	}
}

func sessionGateway(t *testing.T, routes routeMap) *packServer {
	t.Helper()
	ps := packGateway(t, routes)
	old := sessionPollDelay
	sessionPollDelay = time.Millisecond
	t.Cleanup(func() { sessionPollDelay = old })
	return ps
}

func TestFromSession_ShowRedactedUploadsNothing(t *testing.T) {
	ps := sessionGateway(t, routeMap{})
	out, err := runCommand(t, SkillCmd(), "from-session", sessionFile(t), "--show-redacted", "--redact-term", "deploy")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, leak := range []string{fakeKey(), "ann@example.com", "hunter2hunter2"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q printed:\n%s", leak, out)
		}
	}
	for _, want := range []string{"claude_code session, 3 turns, 1 tool calls: removed", "⟦SECRET_", "⟦EMAIL_1⟧", "⟦TERM_1⟧", "Nothing was uploaded."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(ps.hits) != 0 {
		t.Errorf("the gateway was contacted: %v", ps.hits)
	}
	if out, err := runCommand(t, SkillCmd(), "from-session", sessionFile(t), "--show-redacted", "--json"); err != nil || !strings.HasPrefix(strings.TrimSpace(out), "{") || strings.Contains(out, "removed") {
		t.Errorf("json: %q %v", out, err)
	}
}

func TestFromSession_UploadsOnlyTheRedactedSession(t *testing.T) {
	ps := sessionGateway(t, sessionRoutes())
	out, err := runCommand(t, SkillCmd(), "from-session", sessionFile(t), "--consent", "--from-turn", "1", "--to-turn", "2")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	raw, _ := json.Marshal(ps.body("POST /api/skills/from-session/captures", 0))
	for _, leak := range []string{fakeKey(), "ann@example.com", "hunter2hunter2", "thanks"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("%q in the upload: %s", leak, raw)
		}
	}
	b := ps.body("POST /api/skills/from-session/captures", 0)
	sess := b["session"].(map[string]any)
	if b["source"] != "claude_code" || b["clientRedacted"] != true || b["consent"].(map[string]any)["statementVersion"] != "v1" ||
		b["turnRange"].(map[string]any)["to"] != float64(2) || sess["redaction"].(map[string]any)["clientRedacted"] != true || len(sess["turns"].([]any)) != 2 {
		t.Errorf("body = %s", raw)
	}
	if e := ps.body("POST /api/skills/from-session/captures/cap_1/extract", 0); e["mode"] != "new_skill" {
		t.Errorf("extract = %v", e)
	}
	for _, want := range []string{"Uploaded cap_1: 3 turns", "Drafting started. Review it at " + ps.URL + "/skills/from-session/cap_1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestFromSession_WaitAcceptAndThen(t *testing.T) {
	ps := sessionGateway(t, sessionRoutes("queued", "running", "ready"))
	out, err := runCommand(t, SkillCmd(), "from-session", sessionFile(t), "--consent", "--wait", "--accept", "--then", "evals-optimize", "--into", "pr-triage", "--set", "set_9")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Proposal deploy-check", "connectors: github", "2 eval cases", "warning: thin transcript", "looks like instructions", "Accepted. The draft is private to you and not published."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if e := ps.body("POST /api/skills/from-session/captures/cap_1/extract", 0); e["mode"] != "eval_cases" || e["targetResourceId"] != "pr-triage" || e["setId"] != "set_9" {
		t.Errorf("extract = %v", e)
	}
	a := ps.body("POST /api/skills/from-session/extractions/ext_1/accept", 0)
	if a["then"] != "evals_optimize" || a["mode"] != "eval_cases" || a["targetResourceId"] != "pr-triage" || a["proposal"].(map[string]any)["name"] != "deploy-check" {
		t.Errorf("accept = %v", a)
	}
	if ps.count("GET /api/skills/from-session/extractions/ext_1") != 3 {
		t.Errorf("polled %d times", ps.count("GET /api/skills/from-session/extractions/ext_1"))
	}
	out, err = runCommand(t, SkillCmd(), "from-session", sessionFile(t), "--consent", "--wait", "--json")
	if err != nil || !strings.Contains(out, `"extractionId":"ext_1"`) || !strings.Contains(out, `"proposal"`) {
		t.Errorf("json: %q %v", out, err)
	}
}

func TestFromSession_ExtractionFailureAndTimeout(t *testing.T) {
	sessionGateway(t, sessionRoutes("failed"))
	if _, err := runCommand(t, SkillCmd(), "from-session", sessionFile(t), "--consent", "--wait"); err == nil || !strings.Contains(err.Error(), "model failed") {
		t.Errorf("failed: %v", err)
	}
	sessionGateway(t, sessionRoutes(strings.Split(strings.Repeat("running,", 2000), ",")...))
	old := extractionWait
	extractionWait = 5 * time.Millisecond
	t.Cleanup(func() { extractionWait = old })
	_, err := runCommand(t, SkillCmd(), "from-session", sessionFile(t), "--consent", "--wait")
	if exitCodeOf(err) != 12 {
		t.Errorf("timeout: exit %d, %v", exitCodeOf(err), err)
	}
}

func TestFromSession_RunsAndMCPSessionsAreReadByTheGateway(t *testing.T) {
	ps := sessionGateway(t, sessionRoutes())
	if _, err := runCommand(t, SkillCmd(), "from-session", "crn_k3j2h1", "--consent"); err != nil {
		t.Fatal(err)
	}
	if b := ps.body("POST /api/skills/from-session/captures", 0); b["source"] != "agent_run" || b["runId"] != "crn_k3j2h1" || b["session"] != nil {
		t.Errorf("run body = %v", b)
	}
	if _, err := runCommand(t, SkillCmd(), "from-session", "mcp-session:abc"); exitCodeOf(err) != 2 || !strings.Contains(err.Error(), "--goal") {
		t.Errorf("no goal: %v", err)
	}
	if _, err := runCommand(t, SkillCmd(), "from-session", "mcp-session:abc", "--consent", "--goal", "close the books", "--redact-term", "acme"); err != nil {
		t.Fatal(err)
	}
	if b := ps.body("POST /api/skills/from-session/captures", 1); b["source"] != "mcp_session" || b["sessionRef"] != "abc" || b["goal"] != "close the books" || b["redactTerms"].([]any)[0] != "acme" {
		t.Errorf("mcp body = %v", b)
	}
	if _, err := runCommand(t, SkillCmd(), "from-session", "crn_k3j2h1", "--show-redacted"); exitCodeOf(err) != 2 {
		t.Errorf("show-redacted of a run: %v", err)
	}
}

func TestFromSession_RefusalsAndExitCodes(t *testing.T) {
	routes := sessionRoutes()
	routes["POST /api/skills/from-session/captures"] = jsonReply(413, `{"error":"too_large"}`)
	sessionGateway(t, routes)
	_, err := runCommand(t, SkillCmd(), "from-session", sessionFile(t), "--consent")
	if exitCodeOf(err) != 22 || !strings.Contains(err.Error(), "--from-turn") {
		t.Errorf("413: exit %d, %v", exitCodeOf(err), err)
	}
	routes["POST /api/skills/from-session/captures"] = jsonReply(422, `{"error":{"message":"cannot parse line 4"}}`)
	sessionGateway(t, routes)
	_, err = runCommand(t, SkillCmd(), "from-session", sessionFile(t), "--consent")
	if exitCodeOf(err) != 2 || !strings.Contains(err.Error(), "cannot parse line 4") {
		t.Errorf("422: exit %d, %v", exitCodeOf(err), err)
	}
	routes["POST /api/skills/from-session/captures"] = jsonReply(402, `{"error":"plan_required","message":"needs Pro"}`)
	sessionGateway(t, routes)
	if _, err = runCommand(t, SkillCmd(), "from-session", sessionFile(t), "--consent"); exitCodeOf(err) != 30 {
		t.Errorf("402: exit %d, %v", exitCodeOf(err), err)
	}
	routes["POST /api/skills/from-session/captures"] = jsonReply(500, `{"error":"boom"}`)
	sessionGateway(t, routes)
	if _, err = runCommand(t, SkillCmd(), "from-session", sessionFile(t), "--consent"); err == nil {
		t.Error("500 accepted")
	}
	routes["POST /api/skills/from-session/captures"] = jsonReply(201, `{}`)
	sessionGateway(t, routes)
	if _, err = runCommand(t, SkillCmd(), "from-session", sessionFile(t), "--consent"); err == nil || !strings.Contains(err.Error(), "capture id") {
		t.Errorf("no id: %v", err)
	}

	ps := sessionGateway(t, sessionRoutes())
	if _, err := runCommand(t, SkillCmd(), "from-session", sessionFile(t)); exitCodeOf(err) != 2 || !strings.Contains(err.Error(), "--consent") {
		t.Errorf("no consent: exit %d, %v", exitCodeOf(err), err)
	}
	if ps.count("POST /api/skills/from-session/captures") != 0 {
		t.Error("uploaded without consent")
	}
	t.Setenv("ZUANO_AGENT", "1")
	for _, args := range [][]string{{"from-session", sessionFile(t), "--consent"}, {"from-session", "show", "cap_1"}} {
		if _, err := runCommand(t, SkillCmd(), args...); exitCodeOf(err) != 10 {
			t.Errorf("%v under an agent: exit %d, %v", args, exitCodeOf(err), err)
		}
	}
}

func TestFromSession_FlagAndInputErrors(t *testing.T) {
	sessionGateway(t, sessionRoutes())
	f := sessionFile(t)
	for name, args := range map[string][]string{
		"accept without wait": {"from-session", f, "--accept"},
		"then without accept": {"from-session", f, "--wait", "--then", "evals"},
		"bad then":            {"from-session", f, "--wait", "--accept", "--then", "x"},
		"set without into":    {"from-session", f, "--set", "s"},
		"unknown agent":       {"from-session", f, "--agent", "vim"},
		"missing file":        {"from-session", filepath.Join(t.TempDir(), "none")},
		"missing redact file": {"from-session", f, "--redact-file", filepath.Join(t.TempDir(), "none")},
		"bad range":           {"from-session", f, "--from-turn", "9", "--show-redacted"},
		"reversed range":      {"from-session", f, "--from-turn", "3", "--to-turn", "1", "--show-redacted"},
		"no source no tty":    {"from-session", "--consent"},
	} {
		_, err := runCommand(t, SkillCmd(), args...)
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	empty := filepath.Join(t.TempDir(), "e.json")
	_ = os.WriteFile(empty, []byte("   "), 0o600)
	if _, err := runCommand(t, SkillCmd(), "from-session", empty); exitCodeOf(err) != 2 {
		t.Errorf("empty file: %v", err)
	}
	wrong := filepath.Join(t.TempDir(), "w.json")
	_ = os.WriteFile(wrong, []byte(`{"messages":[]}`), 0o600)
	if _, err := runCommand(t, SkillCmd(), "from-session", wrong, "--agent", "cline"); exitCodeOf(err) != 2 {
		t.Errorf("wrong format: %v", err)
	}
}

func TestFromSession_ConsentPromptOnATerminal(t *testing.T) {
	ps := sessionGateway(t, sessionRoutes())
	cmd := SkillCmd()
	cmd.SetIn(iotest.OneByteReader(strings.NewReader("n\n")))
	out, err := runCommand(t, cmd, "from-session", sessionFile(t))
	if err == nil || !strings.Contains(out, "I may share this.") || ps.count("POST /api/skills/from-session/captures") != 0 {
		t.Errorf("declined: %v\n%s", err, out)
	}
	cmd = SkillCmd()
	cmd.SetIn(iotest.OneByteReader(strings.NewReader("y\n")))
	if _, err := runCommand(t, cmd, "from-session", sessionFile(t)); err != nil {
		t.Errorf("accepted: %v", err)
	}
}

func TestFromSession_ReadsStandardInputAndExports(t *testing.T) {
	sessionGateway(t, sessionRoutes())
	cmd := SkillCmd()
	cmd.SetIn(strings.NewReader("User: hello there\nAssistant: hi"))
	out, err := runCommand(t, cmd, "from-session", "-", "--show-redacted")
	if err != nil || !strings.Contains(out, "markdown session, 2 turns") {
		t.Errorf("stdin: %q %v", out, err)
	}
	export := `[{"uuid":"u1","name":"One","chat_messages":[{"sender":"human","text":"first"}]},{"uuid":"u2","name":"Two","chat_messages":[{"sender":"human","text":"second"}]}]`
	p := filepath.Join(t.TempDir(), "conversations.json")
	_ = os.WriteFile(p, []byte(export), 0o600)
	if out, err := runCommand(t, SkillCmd(), "from-session", p, "--conversation", "u2", "--show-redacted", "--agent", "claude-ai"); err != nil || !strings.Contains(out, "second") {
		t.Errorf("conversation: %q %v", out, err)
	}
	cmd = SkillCmd()
	cmd.SetIn(iotest.OneByteReader(strings.NewReader("1\n")))
	if out, err := runCommand(t, cmd, "from-session", p, "--show-redacted"); err != nil || !strings.Contains(out, "first") || !strings.Contains(out, "Conversation number") {
		t.Errorf("chosen: %q %v", out, err)
	}
	cmd = SkillCmd()
	cmd.SetIn(iotest.OneByteReader(strings.NewReader("x\n")))
	if _, err := runCommand(t, cmd, "from-session", p, "--show-redacted"); exitCodeOf(err) != 2 {
		t.Errorf("no choice: %v", err)
	}
	if _, err := runCommand(t, SkillCmd(), "from-session", p, "--conversation", "zzz", "--show-redacted"); exitCodeOf(err) != 2 {
		t.Errorf("unknown conversation: %v", err)
	}
}

func writeSession(t *testing.T, rel, content string, mod time.Time) {
	t.Helper()
	p := filepath.Join(os.Getenv("HOME"), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

const localSession = `{"type":"user","cwd":"/home/ann/%s","message":{"role":"user","content":"%s"}}` + "\n"

func TestFromSession_LastAndPickUseLocalSessions(t *testing.T) {
	sessionGateway(t, routeMap{})
	if _, err := runCommand(t, SkillCmd(), "from-session", "--last", "--show-redacted"); exitCodeOf(err) != 2 {
		t.Errorf("no sessions: %v", err)
	}
	now := time.Now()
	writeSession(t, ".claude/projects/-a/old.jsonl", strings.NewReplacer("%s", "x").Replace(localSession), now.Add(-3*time.Hour))
	writeSession(t, ".claude/projects/-b/new.jsonl", `{"type":"user","cwd":"/home/ann/other","message":{"role":"user","content":"newest question"}}`+"\n", now.Add(-time.Minute))
	writeSession(t, ".codex/sessions/2026/10/01/rollout-1.jsonl", `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"codex question"}]}}`+"\n", now.Add(-time.Hour))
	out, err := runCommand(t, SkillCmd(), "from-session", "--last", "--show-redacted")
	if err != nil || !strings.Contains(out, "newest question") || !strings.Contains(out, "Using the newest session") {
		t.Errorf("last: %q %v", out, err)
	}
	if out, err := runCommand(t, SkillCmd(), "from-session", "--last", "--agent", "codex", "--show-redacted"); err != nil || !strings.Contains(out, "codex question") {
		t.Errorf("last codex: %q %v", out, err)
	}
	cmd := SkillCmd()
	cmd.SetIn(iotest.OneByteReader(strings.NewReader("2\n")))
	out, err = runCommand(t, cmd, "from-session", "--show-redacted")
	if err != nil || !strings.Contains(out, "Session number (1-3)") || !strings.Contains(out, "codex question") || !strings.Contains(out, "newest question") {
		t.Errorf("pick: %q %v", out, err)
	}
	cmd = SkillCmd()
	cmd.SetIn(iotest.OneByteReader(strings.NewReader("9\n")))
	if _, err := runCommand(t, cmd, "from-session", "--pick", "--show-redacted"); exitCodeOf(err) != 2 {
		t.Errorf("bad pick: %v", err)
	}
	cwd, _ := os.Getwd()
	_ = cwd
}

func TestFromSession_OpenCodeSessionsAreExported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script")
	}
	sessionGateway(t, routeMap{})
	dir := t.TempDir()
	script := "#!/bin/sh\necho '{\"messages\":[{\"info\":{\"role\":\"user\"},\"parts\":[{\"type\":\"text\",\"text\":\"exported question\"}]}]}'\n"
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	out, err := runCommand(t, SkillCmd(), "from-session", "ses_1", "--agent", "opencode", "--show-redacted")
	if err != nil || !strings.Contains(out, "exported question") {
		t.Errorf("%q %v", out, err)
	}
}

func TestFromSession_ListShowDiscard(t *testing.T) {
	ps := sessionGateway(t, routeMap{
		"GET /api/skills/from-session/captures":          jsonReply(200, `{"captures":[{"id":"cap_1","source":"claude_code","status":"extracted","expiresAt":"2026-10-23","skill":{"id":"res_1","name":"deploy-check"}},{"id":"cap_2","source":"markdown","status":"received","expiresAt":"2026-10-24"}]}`),
		"GET /api/skills/from-session/captures/cap_1":    jsonReply(200, `{"capture":{"id":"cap_1","source":"claude_code","status":"extracted","expiresAt":"2026-10-23","turns":12,"toolCalls":4,"redaction":{"counts":{"secret":2,"email":0,"ip":1}}}}`),
		"DELETE /api/skills/from-session/captures/cap_1": jsonReply(200, `{"ok":true}`),
	})
	out, err := runCommand(t, SkillCmd(), "from-session", "list")
	if err != nil || !strings.Contains(out, "cap_1") || !strings.Contains(out, "deploy-check") || !strings.Contains(out, "cap_2") {
		t.Errorf("list: %q %v", out, err)
	}
	out, err = runCommand(t, SkillCmd(), "from-session", "show", "cap_1")
	if err != nil || !strings.Contains(out, "12 turns, 4 tool calls") || !strings.Contains(out, "removed: ip 1, secret 2") || !strings.Contains(out, ps.URL+"/skills/from-session/cap_1") {
		t.Errorf("show: %q %v", out, err)
	}
	for _, args := range [][]string{{"list", "--json"}, {"show", "cap_1", "--json"}} {
		if out, err := runCommand(t, SkillCmd(), append([]string{"from-session"}, args...)...); err != nil || !strings.Contains(out, "cap_1") {
			t.Errorf("%v: %q %v", args, out, err)
		}
	}
	if _, err := runCommand(t, SkillCmd(), "from-session", "discard", "cap_1"); err == nil {
		t.Error("discarded without confirmation")
	}
	if out, err := runCommand(t, SkillCmd(), "from-session", "discard", "cap_1", "--yes"); err != nil || !strings.Contains(out, "Deleted cap_1.") {
		t.Errorf("discard: %q %v", out, err)
	}
	if out, err := runCommand(t, SkillCmd(), "from-session", "discard", "cap_1", "--yes", "--json"); err != nil || !strings.Contains(out, `"ok":true`) {
		t.Errorf("discard json: %q %v", out, err)
	}
}

func TestFromSession_ListWhenEmpty(t *testing.T) {
	sessionGateway(t, routeMap{"GET /api/skills/from-session/captures": jsonReply(200, `{"captures":[]}`)})
	if out, err := runCommand(t, SkillCmd(), "from-session", "list"); err != nil || !strings.Contains(out, "No uploaded sessions.") {
		t.Errorf("%q %v", out, err)
	}
}

func TestDoctorTranscripts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var got string
	doctorTranscripts(func(name string, ok bool, d string) { got = d })
	if got != "no agent session directories found" {
		t.Errorf("none: %q", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	doctorTranscripts(func(name string, ok bool, d string) { got = d })
	if !strings.Contains(got, "claude_code") {
		t.Errorf("readable: %q", got)
	}
}
