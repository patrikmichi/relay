package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// elicitationGateway answers tools/call with the -32042 fixture until the
// approval is decided, and records what the client sent.
type elicitationGateway struct {
	mu        sync.Mutex
	srv       *httptest.Server
	methods   []string
	sessions  []string
	decision  string
	polls     int
	toolCalls int
}

func newElicitationGateway(t *testing.T, decision string, pageHost string) *elicitationGateway {
	t.Helper()
	g := &elicitationGateway{decision: decision}
	fixture := contractFixture(t, "mcp-url-elicitation.json")
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case r.URL.Path == "/api/stripe/mcp":
			raw, _ := io.ReadAll(r.Body)
			var rpc struct {
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			_ = json.Unmarshal(raw, &rpc)
			g.methods = append(g.methods, rpc.Method)
			g.sessions = append(g.sessions, r.Header.Get("Mcp-Session-Id"))
			switch rpc.Method {
			case "initialize":
				caps, _ := json.Marshal(rpc.Params["capabilities"])
				if !strings.Contains(string(caps), `"elicitation":{"url":{}}`) {
					t.Errorf("initialize capabilities = %s", caps)
				}
				w.Header().Set("Mcp-Session-Id", "sid_7")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{}}`))
			case "tools/call":
				g.toolCalls++
				if g.polls == 0 || g.decision != "approved" {
					host := pageHost
					if host == "" {
						host = g.srv.URL
					}
					_, _ = w.Write([]byte(strings.ReplaceAll(fixture, "https://gateway.example", host)))
					return
				}
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"refund done"}]}}`))
			default:
				w.WriteHeader(http.StatusAccepted)
			}
		case r.URL.Path == "/api/approvals/apr_1":
			g.polls++
			status := "pending"
			if g.polls >= 2 {
				status = g.decision
			}
			_, _ = w.Write([]byte(`{"approval":{"approval_id":"apr_1","status":"` + status + `","service":"stripe","tool":"create_refund","risk":"destructive"}}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(g.srv.Close)
	withAPIKeyGateway(t, g.srv)
	t.Setenv("ZUANO_AGENT", "")
	approvalPollInterval = time.Millisecond
	t.Cleanup(func() { approvalPollInterval = 2 * time.Second })
	return g
}

func TestCall_URLElicitationOpensThePageWaitsAndRetriesOnce(t *testing.T) {
	g := newElicitationGateway(t, "approved", "")
	opened := stubBrowserWith(t, nil)
	out, err := runWithInput(t, CallCmd(), "y\n", "stripe", "create_refund", "--arg", "charge=ch_1")
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	for _, want := range []string{
		"Approval required: Approve create_refund on stripe (destructive)",
		"/approvals/apr_1?e=eid_1",
		"host: " + strings.TrimPrefix(g.srv.URL, "http://"),
		"Approved. Running the call again.",
		"refund done",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(*opened) != 1 || *opened == nil || !strings.HasSuffix((*opened)[0], "/approvals/apr_1?e=eid_1") {
		t.Errorf("opened = %v", *opened)
	}
	if g.toolCalls != 2 {
		t.Errorf("tools/call sent %d times, want the call and one retry", g.toolCalls)
	}
	want := []string{"initialize", "notifications/initialized", "tools/call", "tools/call"}
	if strings.Join(g.methods, ",") != strings.Join(want, ",") {
		t.Errorf("methods = %v", g.methods)
	}
	for i, s := range g.sessions {
		if (i == 0) != (s == "") || (i > 0 && s != "sid_7") {
			t.Errorf("request %d (%s) carried session %q", i, g.methods[i], s)
		}
	}
}

func TestCall_URLElicitationWithNoBrowserOnlyPrintsTheLink(t *testing.T) {
	newElicitationGateway(t, "approved", "")
	opened := stubBrowserWith(t, nil)
	out, err := runWithInput(t, CallCmd(), "", "stripe", "create_refund", "--no-browser")
	if err != nil || len(*opened) != 0 || !strings.Contains(out, "refund done") {
		t.Fatalf("opened %v, err %v\n%s", *opened, err, out)
	}
}

func TestCall_URLElicitationDeclinedIsExit10AndNotRetried(t *testing.T) {
	g := newElicitationGateway(t, "denied", "")
	stubBrowserWith(t, nil)
	_, err := runWithInput(t, CallCmd(), "n\n", "stripe", "create_refund")
	if got := exitCodeOf(err); got != 10 {
		t.Fatalf("exit %d (%v), want 10", got, err)
	}
	if g.toolCalls != 1 {
		t.Errorf("tools/call sent %d times, want 1", g.toolCalls)
	}
}

func TestCall_URLElicitationWithoutAPersonDoesNotOpenOrWaitUnlessAsked(t *testing.T) {
	g := newElicitationGateway(t, "approved", "")
	t.Setenv("ZUANO_AGENT", "1")
	opened := stubBrowserWith(t, nil)
	out, err := runWithInput(t, CallCmd(), "", "stripe", "create_refund")
	if got := exitCodeOf(err); got != 12 || len(*opened) != 0 || g.polls != 0 {
		t.Fatalf("exit %d, opened %v, polls %d\n%s", got, *opened, g.polls, out)
	}
	if !strings.Contains(out, "zuano approvals wait apr_1") {
		t.Errorf("output = %s", out)
	}

	out, err = runWithInput(t, CallCmd(), "", "stripe", "create_refund", "--wait=30", "--json")
	if err != nil || g.toolCalls != 3 || strings.TrimSpace(out) != `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"refund done"}]}}` {
		t.Fatalf("err %v, toolCalls %d\n%s", err, g.toolCalls, out)
	}
}

func TestCall_URLElicitationOnAnotherHostIsNotOpened(t *testing.T) {
	newElicitationGateway(t, "approved", "https://phish.example")
	opened := stubBrowserWith(t, nil)
	out, err := runWithInput(t, CallCmd(), "y\n", "stripe", "create_refund")
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if len(*opened) != 0 || !strings.Contains(out, "host: phish.example") || !strings.Contains(out, "not on your gateway") {
		t.Errorf("opened %v\n%s", *opened, out)
	}
}

func TestElicitationFromError(t *testing.T) {
	e := &mcpRPCError{Code: -32042, Data: json.RawMessage(`{"elicitations":[{"mode":"form","url":"https://x/y"},{"mode":"url","elicitationId":"e","url":"https://gw/approvals/apr_9?e=e"}]}`)}
	got := elicitationFromError(e)
	if got == nil || got.ID != "e" || got.ApprovalID != "apr_9" {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []*mcpRPCError{nil, {Data: json.RawMessage(`{}`)}, {Data: json.RawMessage(`not json`)}} {
		if elicitationFromError(bad) != nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if _, _, err := elicitationPage("https://gw", "javascript:alert(1)"); err == nil {
		t.Error("a javascript link was accepted")
	}
}

func TestHighlightHost(t *testing.T) {
	u, _ := url.Parse("https://gw.example.com/approvals/apr_1?e=1")
	if got := highlightHost(u, true); got != "https://\x1b[1;4mgw.example.com\x1b[0m/approvals/apr_1?e=1" {
		t.Errorf("colored = %q", got)
	}
	if got := highlightHost(u, false); got != u.String() {
		t.Errorf("plain = %q", got)
	}
}
