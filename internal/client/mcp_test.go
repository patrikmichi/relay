package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type mcpRecord struct {
	method  string
	session string
	params  map[string]any
}

func mcpGateway(t *testing.T, issue func(n int) (sid string, status int)) (*httptest.Server, func() []mcpRecord) {
	t.Helper()
	var mu sync.Mutex
	var seen []mcpRecord
	inits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal(raw, &rpc)
		mu.Lock()
		seen = append(seen, mcpRecord{rpc.Method, r.Header.Get(MCPSessionHeader), rpc.Params})
		if rpc.Method == "initialize" {
			inits++
			sid, status := issue(inits)
			if sid != "" {
				w.Header().Set(MCPSessionHeader, sid)
			}
			mu.Unlock()
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{}}`))
			return
		}
		mu.Unlock()
		if rpc.Method == "tools/call" && r.Header.Get(MCPSessionHeader) == "expired" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []mcpRecord {
		mu.Lock()
		defer mu.Unlock()
		return append([]mcpRecord(nil), seen...)
	}
}

func TestMCPPost_InitializesOnceDeclaringURLElicitationAndEchoesTheSession(t *testing.T) {
	srv, seen := mcpGateway(t, func(int) (string, int) { return "sid_1", 200 })
	c := New(srv.URL, "tok")
	for range 2 {
		resp, err := c.MCPPostContext(t.Context(), "/api/stripe/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	got := seen()
	var methods []string
	for _, r := range got {
		methods = append(methods, r.method)
	}
	want := []string{"initialize", "notifications/initialized", "tools/call", "tools/call"}
	if len(methods) != len(want) {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
	for i := range want {
		if methods[i] != want[i] {
			t.Fatalf("methods = %v, want %v", methods, want)
		}
	}
	caps := got[0].params["capabilities"].(map[string]any)
	if _, ok := caps["elicitation"].(map[string]any)["url"]; !ok {
		t.Errorf("capabilities = %v, want elicitation.url", caps)
	}
	if got[0].session != "" || got[1].session != "sid_1" || got[2].session != "sid_1" || got[3].session != "sid_1" {
		t.Errorf("sessions = %q %q %q %q", got[0].session, got[1].session, got[2].session, got[3].session)
	}
}

func TestMCPPost_WorksWithAGatewayThatIssuesNoSession(t *testing.T) {
	srv, seen := mcpGateway(t, func(int) (string, int) { return "", 200 })
	c := New(srv.URL, "tok")
	resp, err := c.MCPPostContext(t.Context(), "/api/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if last := seen()[len(seen())-1]; last.method != "tools/call" || last.session != "" {
		t.Errorf("last request = %+v", last)
	}
}

func TestMCPPost_AFailedInitializeDoesNotStopTheCall(t *testing.T) {
	srv, seen := mcpGateway(t, func(int) (string, int) { return "", 500 })
	c := New(srv.URL, "tok")
	resp, err := c.MCPPostContext(t.Context(), "/api/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
	resp.Body.Close()
	if n := len(seen()); n != 2 {
		t.Errorf("requests = %d, want initialize and the call", n)
	}
}

func TestMCPPost_AForgottenSessionIsRenewedOnce(t *testing.T) {
	srv, seen := mcpGateway(t, func(n int) (string, int) {
		if n == 1 {
			return "expired", 200
		}
		return "sid_2", 200
	})
	c := New(srv.URL, "tok")
	resp, err := c.MCPPostContext(t.Context(), "/api/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
	resp.Body.Close()
	calls := 0
	var lastSession string
	for _, r := range seen() {
		if r.method == "tools/call" {
			calls++
			lastSession = r.session
		}
	}
	if calls != 2 || lastSession != "sid_2" {
		t.Errorf("tools/call sent %d times, last with session %q", calls, lastSession)
	}
}
