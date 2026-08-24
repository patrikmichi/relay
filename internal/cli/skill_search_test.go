package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patrikmichi/relay/internal/client"
)

func TestSkillSearch_PrintsResultsTable(t *testing.T) {
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/mcp" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"jsonrpc": "2.0",
			"id": 1,
			"result": {
				"content": [{"type": "text", "text": "[{\"id\":\"res_abc\",\"slug\":\"pr-triage\",\"description\":\"triage PRs\",\"version\":\"1.4.2\",\"trustScore\":92}]"}]
			}
		}`))
	}))
	defer srv.Close()

	cmd := SkillSearchCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"triage", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "res_abc") || !strings.Contains(got, "pr-triage") || !strings.Contains(got, "92") {
		t.Errorf("expected result table with id/slug/trust score, got:\n%s", got)
	}
}

func TestSkillSearch_JSONOutput(t *testing.T) {
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"jsonrpc": "2.0", "id": 1,
			"result": {"content": [{"type": "text", "text": "[{\"id\":\"res_abc\",\"slug\":\"pr-triage\"}]"}]}
		}`))
	}))
	defer srv.Close()

	cmd := SkillSearchCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), `"slug": "pr-triage"`) {
		t.Errorf("expected raw JSON output, got:\n%s", out.String())
	}
}

func TestSkillSearch_NoResults(t *testing.T) {
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"[]"}]}}`))
	}))
	defer srv.Close()

	cmd := SkillSearchCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"nonexistent-thing", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "no matching skills") {
		t.Errorf("expected a no-results message, got:\n%s", out.String())
	}
}

func TestSkillSearch_AggregateDisabledDegradesWithGuidance(t *testing.T) {
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"ok":false,"error":"Aggregate MCP endpoint is disabled. Set AGGREGATE_MCP_ENABLED=true to enable."}`))
	}))
	defer srv.Close()

	cmd := SkillSearchCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"triage", "--gateway-url", srv.URL})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected an error when the aggregate MCP endpoint is disabled")
	}
	if !strings.Contains(err.Error(), "relay skill install") {
		t.Errorf("expected guidance pointing at `relay skill install`, got: %v", err)
	}
}

func TestSkillSearch_NoGatewayConfiguredDegrades(t *testing.T) {
	cmd := SkillSearchCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"triage"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error with no gateway configured")
	}
}

func TestSkillSearch_RPCErrorSurfaces(t *testing.T) {
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"bad args"}}`))
	}))
	defer srv.Close()

	cmd := SkillSearchCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"triage", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for a JSON-RPC error response")
	}
}

func TestSearchSkills_NonJSONRPCBodyWith200Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.Close()

	_, err := searchSkills(client.New(srv.URL, "tok"), "")
	if err == nil || !strings.Contains(err.Error(), "unexpected search response") {
		t.Fatalf("expected an 'unexpected search response' error, got: %v", err)
	}
}

func TestSearchSkills_NonJSONRPCBodyWithErrorStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream exploded"))
	}))
	defer srv.Close()

	_, err := searchSkills(client.New(srv.URL, "tok"), "")
	if err == nil || !strings.Contains(err.Error(), "search failed (502)") {
		t.Fatalf("expected a 'search failed (502)' error, got: %v", err)
	}
}

func TestSearchSkills_MalformedResultTextErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"not-a-json-array"}]}}`))
	}))
	defer srv.Close()

	_, err := searchSkills(client.New(srv.URL, "tok"), "")
	if err == nil || !strings.Contains(err.Error(), "decode search results") {
		t.Fatalf("expected a 'decode search results' error, got: %v", err)
	}
}

func TestSearchSkills_EmptyContentReturnsNilNoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
	}))
	defer srv.Close()

	results, err := searchSkills(client.New(srv.URL, "tok"), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil results for empty content, got: %+v", results)
	}
}

// TestSearchSkills_HangingServerBoundedByTimeout is the m3 regression: a
// gateway that never responds must not hang `relay skill search` forever —
// searchTimeout is shrunk for this test so it runs fast.
func TestSearchSkills_HangingServerBoundedByTimeout(t *testing.T) {
	orig := searchTimeout
	searchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { searchTimeout = orig })

	// unblock must be closed BEFORE srv.Close() is called, or Close() (which
	// waits for in-flight handlers to return) would itself hang forever —
	// hence one explicit defer in known order, not two t.Cleanup
	// registrations (whose LIFO order would run Close() first).
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	start := time.Now()
	_, err := searchSkills(client.New(srv.URL, "tok"), "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected an error for a hanging gateway response")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("expected searchSkills to abort promptly on timeout, took %s", elapsed)
	}
}

func TestSearchSkills_DirectDoer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"[{\"id\":\"res_x\",\"slug\":\"x\"}]"}]}}`))
	}))
	defer srv.Close()

	c := client.New(srv.URL, "tok")
	results, err := searchSkills(c, "")
	if err != nil {
		t.Fatalf("searchSkills: %v", err)
	}
	if len(results) != 1 || results[0].ID != "res_x" {
		t.Fatalf("unexpected results: %+v", results)
	}
}
