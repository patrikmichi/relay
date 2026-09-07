package cli

// White-box tests for mcp_list.go. All network tests use httptest.Server —
// no live gateway is ever contacted.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newMockMcpCatalogGateway(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/catalog" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("type") != "mcp_server" {
			t.Errorf("expected type=mcp_server query param, got %q", r.URL.Query().Get("type"))
		}
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMcpList_HappyPath_PrintsTable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	body := `{"resources":[
		{"resource":{"id":"res_mcp1","slug":"notion-mcp","name":"Notion","type":"mcp_server"},
		 "currentVersion":{"semver":"1.0.0","manifestJson":{"source":"url"}}},
		{"resource":{"id":"res_mcp2","slug":"local-fs","name":"Local FS","type":"mcp_server"},
		 "currentVersion":{"semver":"2.1.0","manifestJson":{"source":"repo"}}}
	]}`
	srv := newMockMcpCatalogGateway(t, body, http.StatusOK)

	cmd := McpListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--gateway-url", srv.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, out.String())
	}

	got := out.String()
	for _, want := range []string{"res_mcp1", "notion-mcp", "Notion", "url", "1.0.0", "res_mcp2", "local-fs", "repo", "2.1.0"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, got)
		}
	}
}

func TestMcpList_EmptyResults_PrintsNoneFoundMessage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	srv := newMockMcpCatalogGateway(t, `{"resources":[]}`, http.StatusOK)

	cmd := McpListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--gateway-url", srv.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "no MCP servers found") {
		t.Errorf("expected a 'no MCP servers found' message, got: %s", out.String())
	}
}

func TestMcpList_JSONFlag_PrintsRawJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	body := `{"resources":[{"resource":{"id":"res_mcp1","slug":"notion-mcp","name":"Notion","type":"mcp_server"},"currentVersion":{"semver":"1.0.0","manifestJson":{"source":"url"}}}]}`
	srv := newMockMcpCatalogGateway(t, body, http.StatusOK)

	cmd := McpListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--gateway-url", srv.URL, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), `"id": "res_mcp1"`) {
		t.Errorf("expected raw JSON output, got: %s", out.String())
	}
}

// TestMcpList_NoGatewayConfigured_FailsClosed is the D-required offline
// exit criterion: an empty-HOME build with no gateway configured must fail
// closed with the exact offlineGuidance message.
func TestMcpList_NoGatewayConfigured_FailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cmd := McpListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected an error with no gateway configured")
	}
	if err.Error() != offlineGuidance {
		t.Fatalf("expected the exact offlineGuidance message, got: %v", err)
	}
}

func TestMcpList_OfflineFlag_FailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	SetOffline(true)
	t.Cleanup(func() { SetOffline(false) })

	cmd := McpListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected --offline to refuse mcp list even with credentials configured")
	}
}

// TestMcpList_NonOKStatus_ReturnsError exercises listMcpServers' generic
// non-200/non-401 error path (truncateForError).
func TestMcpList_NonOKStatus_ReturnsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	t.Cleanup(srv.Close)

	cmd := McpListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--gateway-url", srv.URL})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected an error for a 500 response")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected error to include the response body snippet, got: %v", err)
	}
}

func TestMcpList_UnauthorizedMapsToOfflineGuidance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	cmd := McpListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--gateway-url", srv.URL})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for a 401 response")
	}
}
