package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestHelperMCP is a small stdio MCP server the supervisor tests run: the test
// binary re-executed with TUNNEL_HELPER=mcp.
func TestHelperMCP(t *testing.T) {
	if os.Getenv("TUNNEL_HELPER") != "mcp" {
		return
	}
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		raw, _ := json.Marshal(v)
		_, _ = out.Write(append(raw, '\n'))
		_ = out.Flush()
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var fromServer []string
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method == "" && len(m.Error) > 0 {
			fromServer = append(fromServer, string(m.Error))
			continue
		}
		if len(m.ID) == 0 {
			continue
		}
		switch m.Method {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "1"}}})
		case "tools/list":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"tools": []any{map[string]any{"name": "echo"}}}})
		case "tools/call":
			switch m.Params.Name {
			case "crash":
				os.Exit(7)
			case "slow":
				time.Sleep(2 * time.Second)
			case "ask":
				send(map[string]any{"jsonrpc": "2.0", "id": "srv-1", "method": "sampling/createMessage"})
				time.Sleep(100 * time.Millisecond)
			case "notify":
				send(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
			case "big":
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 5000)}}}})
				continue
			case "env":
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"token": os.Getenv("HR_TOKEN"), "home": os.Getenv("HOME")}})
				continue
			}
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"echo": m.Params.Arguments, "server_errors": fromServer}})
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{}})
		}
	}
	os.Exit(0)
}

func stdioTarget(mod func(*Target)) Target {
	t := Target{Name: "hr", Kind: KindMCPStdio, Command: os.Args[0], Args: []string{"-test.run=^TestHelperMCP$"},
		Env: map[string]string{"TUNNEL_HELPER": "mcp"}, Timeout: Duration(5 * time.Second), IdleTimeout: Duration(time.Hour)}
	t.MCP.Methods = DefaultMCPMethods
	if mod != nil {
		mod(&t)
	}
	return t
}

func newSupervisor(t *testing.T, mod func(*Target)) *StdioSupervisor {
	t.Helper()
	s := NewStdioSupervisor(stdioTarget(mod), mustLogger(t))
	s.MinBackoff, s.MaxBackoff, s.Grace = 50*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(s.Close)
	return s
}

func mustLogger(t *testing.T) *Logger {
	t.Helper()
	l, err := NewLogger(nil, "json", "", 1, 1, "off")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func call(t *testing.T, s *StdioSupervisor, id any, method string, params any) (map[string]json.RawMessage, *ExecError) {
	t.Helper()
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	res, xerr := s.Call(context.Background(), req, 0)
	if xerr != nil || res == nil {
		return nil, xerr
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(res, &m); err != nil {
		t.Fatalf("response %s: %v", res, err)
	}
	return m, nil
}

func TestStdioSupervisorServesOneRequestPerExchange(t *testing.T) {
	s := newSupervisor(t, nil)
	res, xerr := call(t, s, "gw-1", "initialize", map[string]any{"protocolVersion": "x"})
	if xerr != nil || string(res["id"]) != `"gw-1"` || !strings.Contains(string(res["result"]), `"fake"`) {
		t.Fatalf("initialize: %v %v", res, xerr)
	}
	res, xerr = call(t, s, 42, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"a": 1}})
	if xerr != nil || string(res["id"]) != "42" || !strings.Contains(string(res["result"]), `"a":1`) {
		t.Fatalf("tools/call: %v %v", res, xerr)
	}
	// Two gateway requests with the same id do not collide: ids are local.
	r1, _ := call(t, s, 7, "tools/list", nil)
	r2, _ := call(t, s, 7, "tools/list", nil)
	if string(r1["id"]) != "7" || string(r2["id"]) != "7" {
		t.Errorf("ids = %s %s", r1["id"], r2["id"])
	}
	// A notification is dropped without an answer.
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if res, xerr := s.Call(context.Background(), raw, 0); res != nil || xerr != nil {
		t.Errorf("notification = %s %v", res, xerr)
	}
	if _, xerr := s.Call(context.Background(), []byte("nope"), 0); xerr == nil || xerr.Code != "mcp_method_not_allowed" {
		t.Errorf("garbage: %v", xerr)
	}
}

func TestStdioSupervisorAnswersServerRequestsWithMethodNotFound(t *testing.T) {
	s := newSupervisor(t, nil)
	if _, xerr := call(t, s, 1, "tools/call", map[string]any{"name": "ask"}); xerr != nil {
		t.Fatal(xerr)
	}
	res, _ := call(t, s, 2, "tools/call", map[string]any{"name": "echo"})
	if !strings.Contains(string(res["result"]), "-32601") {
		t.Errorf("the server was not told its request is unavailable: %s", res["result"])
	}
}

func TestStdioSupervisorReportsListChanged(t *testing.T) {
	s := newSupervisor(t, nil)
	if s.ListChanged() {
		t.Fatal("list_changed before any notification")
	}
	_, _ = call(t, s, 1, "tools/call", map[string]any{"name": "notify"})
	time.Sleep(50 * time.Millisecond)
	if !s.ListChanged() || s.ListChanged() {
		t.Error("list_changed must be reported once")
	}
}

func TestStdioSupervisorRestartsAfterACrashWithBackoff(t *testing.T) {
	s := newSupervisor(t, nil)
	if _, xerr := call(t, s, 1, "tools/call", map[string]any{"name": "crash"}); xerr == nil || xerr.Code != "upstream_reset" || !xerr.Dispatched {
		t.Fatalf("crash: %+v", xerr)
	}
	// Right away the process is not started again.
	if _, xerr := call(t, s, 2, "tools/list", nil); xerr == nil || xerr.Code != "upstream_connect_failed" || xerr.Dispatched || !xerr.Retryable {
		t.Errorf("inside the backoff: %+v", xerr)
	}
	time.Sleep(250 * time.Millisecond)
	if res, xerr := call(t, s, 3, "tools/list", nil); xerr != nil || !strings.Contains(string(res["result"]), "echo") {
		t.Errorf("after the backoff: %v %v", res, xerr)
	}
}

func TestStdioSupervisorStopsWhenIdle(t *testing.T) {
	s := newSupervisor(t, func(tg *Target) { tg.IdleTimeout = Duration(150 * time.Millisecond) })
	if _, xerr := call(t, s, 1, "tools/list", nil); xerr != nil {
		t.Fatal(xerr)
	}
	s.mu.Lock()
	first := s.proc
	s.mu.Unlock()
	time.Sleep(500 * time.Millisecond)
	s.mu.Lock()
	idle := s.proc
	s.mu.Unlock()
	if first == nil || idle != nil {
		t.Fatalf("process before %v, after idle %v", first != nil, idle != nil)
	}
	if _, xerr := call(t, s, 2, "tools/list", nil); xerr != nil {
		t.Errorf("a stopped process must start again without a backoff: %+v", xerr)
	}
}

func TestStdioSupervisorLimitsAndFailures(t *testing.T) {
	s := newSupervisor(t, func(tg *Target) { tg.Timeout = Duration(300 * time.Millisecond) })
	if _, xerr := call(t, s, 1, "tools/call", map[string]any{"name": "slow"}); xerr == nil || xerr.Code != "upstream_timeout" || !xerr.Dispatched {
		t.Errorf("timeout: %+v", xerr)
	}
	big := newSupervisor(t, nil)
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "big"}})
	if _, xerr := big.Call(context.Background(), req, 1000); xerr == nil || xerr.Code != "response_too_large" {
		t.Errorf("response size: %+v", xerr)
	}
	missing := NewStdioSupervisor(stdioTarget(func(tg *Target) { tg.Command = "/nonexistent/server" }), mustLogger(t))
	if _, xerr := call(t, missing, 1, "tools/list", nil); xerr == nil || xerr.Code != "upstream_connect_failed" || xerr.Dispatched {
		t.Errorf("start failure: %+v", xerr)
	}
	badEnv := NewStdioSupervisor(stdioTarget(func(tg *Target) { tg.Env["HR_TOKEN"] = "${file:/nonexistent/token}" }), mustLogger(t))
	if _, xerr := call(t, badEnv, 1, "tools/list", nil); xerr == nil || xerr.Code != "credentials_missing" {
		t.Errorf("unreadable secret: %+v", xerr)
	}
	runAs := NewStdioSupervisor(stdioTarget(func(tg *Target) { tg.RunAsUser = "nobody" }), mustLogger(t))
	if os.Geteuid() != 0 {
		if _, xerr := call(t, runAs, 1, "tools/list", nil); xerr == nil || xerr.Code != "upstream_connect_failed" {
			t.Errorf("run_as_user without root: %+v", xerr)
		}
	}
	// A server that never answers initialize is not used.
	deaf := NewStdioSupervisor(stdioTarget(func(tg *Target) { tg.Command, tg.Args = "sleep", []string{"30"} }), mustLogger(t))
	deaf.Grace = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if _, xerr := deaf.Call(ctx, raw, 0); xerr == nil {
		t.Error("a server that never answered was used")
	}
	deaf.Close()
}

func TestStdioSupervisorEnvironment(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	secret := t.TempDir() + "/tok"
	_ = os.WriteFile(secret, []byte("hr-secret\n"), 0o600)
	s := newSupervisor(t, func(tg *Target) { tg.Env["HR_TOKEN"] = "${file:" + secret + "}" })
	res, xerr := call(t, s, 1, "tools/call", map[string]any{"name": "env"})
	if xerr != nil || !strings.Contains(string(res["result"]), `"token":"hr-secret"`) || strings.Contains(string(res["result"]), "someone") {
		t.Errorf("not inherited, secret resolved: %v %v", res, xerr)
	}
	inherit := newSupervisor(t, func(tg *Target) { tg.EnvInherit = true })
	res, _ = call(t, inherit, 1, "tools/call", map[string]any{"name": "env"})
	if !strings.Contains(string(res["result"]), "someone") {
		t.Errorf("env_inherit: %s", res["result"])
	}
}

func TestStdioSupervisorSurvivesConcurrentCalls(t *testing.T) {
	s := newSupervisor(t, nil)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func(i int) {
			res, xerr := call(t, s, i, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"i": i}})
			if xerr != nil {
				errs <- xerr
				return
			}
			want, _ := json.Marshal(i)
			if string(res["id"]) != string(want) {
				errs <- errors.New("answer for another request")
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < 20; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func TestDefaultStartCommandRefusesUnavailableIdentity(t *testing.T) {
	if _, err := defaultStartCommand(Target{Command: "x", RunAsUser: "definitely-not-a-user-zz"}, nil); err == nil {
		t.Error("an unknown run_as_user was accepted")
	}
	cmd, err := defaultStartCommand(Target{Command: "x", Args: []string{"a"}, Cwd: "/tmp"}, []string{"A=1"})
	if err != nil || cmd.Dir != "/tmp" || len(cmd.Env) != 1 {
		t.Errorf("cmd = %+v, %v", cmd, err)
	}
	_ = exec.Command
}
