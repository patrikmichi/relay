package cli

// White-box tests for service_cmd.go — the discovery repoint (fetchIntegrations,
// off the kill-switched aggregate /api/mcp endpoint) and the execution repoint
// (callTool, off REST /api/<service>/call onto MCP JSON-RPC tools/call).
//
// These live in `package cli` (not cli_test) so they can call the unexported
// fetchIntegrations/callTool functions directly with an httptest.Server URL —
// no real gateway or keychain involved.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/client"
)

// ---- fetchIntegrations (discovery repoint) ----

func TestFetchIntegrations_ParsesServicesAndTools(t *testing.T) {
	var capturedAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/integrations" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(IntegrationsResponse{
			OK:           true,
			ServiceCount: 1,
			ToolCount:    2,
			Services: []DiscoveryService{
				{
					ID:         "clockify",
					Name:       "Clockify",
					Accessible: true,
					ToolCount:  2,
					Tools: []DiscoveryTool{
						{Name: "list_workspaces", Description: "List workspaces"},
						{Name: "list_projects", Description: "List projects"},
					},
				},
			},
		})
	}))
	defer srv.Close()

	info, err := fetchIntegrations(client.New(srv.URL, "bearer-tok"))
	if err != nil {
		t.Fatalf("fetchIntegrations: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil info")
	}
	if capturedAuth != "Bearer bearer-tok" {
		t.Errorf("Authorization: got %q, want %q", capturedAuth, "Bearer bearer-tok")
	}
	if info.ToolCount != 2 {
		t.Errorf("ToolCount: got %d, want 2", info.ToolCount)
	}
	if len(info.Services) != 1 || info.Services[0].ID != "clockify" {
		t.Fatalf("Services: got %+v", info.Services)
	}
	if len(info.Services[0].Tools) != 2 {
		t.Errorf("Tools: got %d, want 2", len(info.Services[0].Tools))
	}
}

func TestFetchIntegrations_UnauthorizedDegradesGracefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	info, err := fetchIntegrations(client.New(srv.URL, "expired-tok"))
	if err != nil {
		t.Fatalf("expected no error on 401 (graceful degrade), got %v", err)
	}
	if info != nil {
		t.Fatalf("expected nil info on 401, got %+v", info)
	}
}

func TestFetchIntegrations_ServerErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := fetchIntegrations(client.New(srv.URL, "tok"))
	if err == nil {
		t.Fatal("expected an error on 500, got nil")
	}
}

// ---- callTool (execution repoint: REST /call -> MCP tools/call) ----

func TestCallTool_PostsJSONRPCToolsCall(t *testing.T) {
	var capturedAuth string
	var capturedBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/clockify/mcp" {
			t.Errorf("unexpected path: %s (want /api/clockify/mcp — MCP transport, not /call)", r.URL.Path)
		}
		capturedAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &capturedBody)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"content": []map[string]interface{}{
					{"type": "text", "text": `{"id":"ws1","name":"Workspace One"}`},
				},
			},
		})
	}))
	defer srv.Close()

	err := callTool(client.New(srv.URL, "bearer-tok"), "clockify", "list_workspaces", []string{"workspaceId=ws1"})
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if capturedAuth != "Bearer bearer-tok" {
		t.Errorf("Authorization: got %q, want %q", capturedAuth, "Bearer bearer-tok")
	}
	if capturedBody["method"] != "tools/call" {
		t.Errorf("method: got %v, want tools/call", capturedBody["method"])
	}
	params, ok := capturedBody["params"].(map[string]interface{})
	if !ok {
		t.Fatalf("params: got %T, want map", capturedBody["params"])
	}
	if params["name"] != "list_workspaces" {
		t.Errorf("params.name: got %v, want list_workspaces", params["name"])
	}
	arguments, ok := params["arguments"].(map[string]interface{})
	if !ok {
		t.Fatalf("params.arguments: got %T, want map", params["arguments"])
	}
	if arguments["workspaceId"] != "ws1" {
		t.Errorf("arguments.workspaceId: got %v, want ws1", arguments["workspaceId"])
	}
}

// TestCallTool_AccountLabelPassthrough verifies that an --arg account=<label>
// (used by multi-account services like google-workspace/intervals-icu) reaches
// the tool as a regular argument inside params.arguments — the account/athlete
// label passthrough the REST /call surface historically special-cased at the
// top level of the request body is unnecessary here, because the CLI has
// always sent labels as --arg account=<label>, which lands inside `arguments`
// on both transports.
func TestCallTool_AccountLabelPassthrough(t *testing.T) {
	var capturedBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "{}"}}},
		})
	}))
	defer srv.Close()

	err := callTool(client.New(srv.URL, "tok"), "google-workspace", "list_files", []string{"account=work", "folderId=f1"})
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	params := capturedBody["params"].(map[string]interface{})
	arguments := params["arguments"].(map[string]interface{})
	if arguments["account"] != "work" {
		t.Errorf("arguments.account: got %v, want work", arguments["account"])
	}
	if arguments["folderId"] != "f1" {
		t.Errorf("arguments.folderId: got %v, want f1", arguments["folderId"])
	}
}

func TestCallTool_MapsJSONRPCErrorToGoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error":   map[string]interface{}{"code": -32601, "message": "Tool clockify.delete_project is not accessible"},
		})
	}))
	defer srv.Close()

	err := callTool(client.New(srv.URL, "tok"), "clockify", "delete_project", nil)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("not accessible")) {
		t.Errorf("error message: got %q, want it to contain %q", err.Error(), "not accessible")
	}
}

func TestCallTool_MapsRateLimitedWithRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error":   map[string]interface{}{"code": -32000, "message": "rate limit exceeded"},
		})
	}))
	defer srv.Close()

	err := callTool(client.New(srv.URL, "tok"), "clockify", "list_workspaces", nil)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("42")) {
		t.Errorf("error message should mention the Retry-After value: %q", err.Error())
	}
}

func TestCallTool_PrintsResultText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"content": []map[string]interface{}{{"type": "text", "text": `[{"id":"p1"}]`}},
			},
		})
	}))
	defer srv.Close()

	stdout := captureStdout(t, func() {
		if err := callTool(client.New(srv.URL, "tok"), "clockify", "list_projects", nil); err != nil {
			t.Fatalf("callTool: %v", err)
		}
	})

	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("expected pretty-printed JSON on stdout, got %q (%v)", stdout, err)
	}
	if len(parsed) != 1 || parsed[0]["id"] != "p1" {
		t.Errorf("unexpected printed result: %v", parsed)
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(out)
}

// ---- BuildServiceCommands (dynamic sub-command registration) ----

func TestBuildServiceCommands_RegistersOnlyAccessibleServicesWithTools(t *testing.T) {
	// BuildServiceCommands resolves a client.Client (GATEWAY_API_KEY /
	// RELAY_EMAIL+keychain / persisted login email+keychain) before it does
	// anything network-related; without any of those it must no-op.
	t.Setenv("HOME", t.TempDir()) // isolate from any real ~/.config/relay/config.json
	t.Setenv("RELAY_EMAIL", "")
	t.Setenv("GATEWAY_API_KEY", "")

	root := &cobra.Command{Use: "relay"}
	BuildServiceCommands(root, "http://example.invalid")

	if len(root.Commands()) != 0 {
		t.Errorf("expected no dynamic commands registered without a session, got %d", len(root.Commands()))
	}
}

// ---- buildToolCmd offline fail-closed (Fix 1) ----

// TestBuildToolCmd_OfflineFlag_FailsClosedWithoutNetworkCall confirms a
// dynamic tool leaf command built by BuildServiceCommands (e.g. `relay
// clockify list_time_entries`) refuses to dial the gateway when --offline
// is set — even though it already has a concrete, non-empty gatewayURL
// closed over from discovery time. Points gatewayURL at a server that would
// fail the test if actually dialed, so this also proves zero network calls
// happen under --offline, not just that SOME error is returned.
func TestBuildToolCmd_OfflineFlag_FailsClosedWithoutNetworkCall(t *testing.T) {
	dialed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	SetOffline(true)
	t.Cleanup(func() { SetOffline(false) })

	cmd := buildToolCmd("clockify", "list_time_entries", "List time entries", srv.URL)
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offlineGuidance error, got: %v", err)
	}
	if dialed {
		t.Fatalf("expected zero network calls under --offline, but the gateway was dialed")
	}
}
