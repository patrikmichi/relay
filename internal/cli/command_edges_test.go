package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/client"
)

type postOnlyDoer struct{ calls int }

func (d *postOnlyDoer) Post(string, string, io.Reader) (*http.Response, error) {
	d.calls++
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("legacy"))}, nil
}

type getOnlyDoer struct{ path string }

func (d *getOnlyDoer) Get(path string) (*http.Response, error) {
	d.path = path
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("legacy"))}, nil
}

func TestCommandDoer_FallsBackToLegacyDoers(t *testing.T) {
	post := &postOnlyDoer{}
	resp, err := commandDoer{post, context.Background()}.Post("/p", "text/plain", nil)
	if err != nil || post.calls != 1 {
		t.Fatalf("legacy Post: %v (calls %d)", err, post.calls)
	}
	_ = resp.Body.Close()

	get := &getOnlyDoer{}
	resp, err = commandDoer{get, context.Background()}.Get("/g")
	if err != nil || get.path != "/g" {
		t.Fatalf("legacy Get: %v (path %q)", err, get.path)
	}
	_ = resp.Body.Close()

	if _, err := (commandDoer{post, context.Background()}).Get("/g"); err == nil || !strings.Contains(err.Error(), "does not support GET") {
		t.Fatalf("POST-only doer must refuse GET, got %v", err)
	}
}

func TestCommandDoer_PostThroughContextClientAndCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c := client.New(srv.URL, syntheticKey("bearer"))

	resp, err := commandDoer{c, nil}.Post("/echo", "text/plain", strings.NewReader("ping"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ping" {
		t.Errorf("body = %q", body)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (commandDoer{c, ctx}).Post("/echo", "text/plain", strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Post: got %v", err)
	}
}

func TestOfflineGuidanceIsTheSharedMessage(t *testing.T) {
	if OfflineGuidance() != offlineGuidance {
		t.Fatal("exported guidance diverged from the internal message")
	}
}

func mcpInspectServer(t *testing.T, status int, descriptor map[string]any) *httptest.Server {
	t.Helper()
	content, _ := json.Marshal(descriptor)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":       map[string]any{"id": "res_1", "type": "mcp_server", "name": "Files"},
			"currentVersion": map[string]any{"semver": "3.1.0", "manifestJson": map[string]any{"content": string(content)}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMcpInspect_TextOutputAppliesDefaults(t *testing.T) {
	srv := mcpInspectServer(t, http.StatusOK, map[string]any{"transport": "stdio-command", "command": "files"})
	withAPIKeyGateway(t, srv)

	out, err := runCommand(t, McpInspectCmd(), "res_1")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	for _, want := range []string{"Files (res_1)", "Version: 3.1.0", "Source: builtin", "Transport: stdio-command", "Authentication: none (org)", "Credential reference configured: false", "Declared tools: 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestInspectMcpServer_AuthReferenceImpliesAPIKey(t *testing.T) {
	srv := mcpInspectServer(t, http.StatusOK, map[string]any{"transport": "http", "endpoint": "https://m.example.com", "authRef": "files-key"})
	got, err := inspectMcpServer(context.Background(), client.New(srv.URL, syntheticKey("bearer")), "res_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthType != "api_key" || !got.HasAuthReference {
		t.Errorf("unexpected inspection %+v", got)
	}
}

func TestInspectMcpServer_Errors(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		status int
		desc   map[string]any
		want   string
	}{
		{"invalid id", "../x", 200, nil, "invalid catalog resource id"},
		{"unauthorized", "res_1", 401, nil, "authentication required"},
		{"server error", "res_1", 502, nil, "MCP inspection failed (HTTP 502)"},
		{"invalid descriptor", "res_1", 200, map[string]any{"transport": "smoke-signal"}, "MCP transport must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := mcpInspectServer(t, tc.status, tc.desc)
			_, err := inspectMcpServer(context.Background(), client.New(srv.URL, syntheticKey("bearer")), tc.id)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"resource":{"id":"res_1","type":"mcp_server"},"currentVersion":{"manifestJson":{"content":"not json"}}}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := inspectMcpServer(context.Background(), client.New(srv.URL, syntheticKey("bearer")), "res_1"); err == nil || !strings.Contains(err.Error(), "gateway upgrade may be required") {
		t.Fatalf("unparseable descriptor: got %v", err)
	}

	srv.Close()
	if _, err := inspectMcpServer(context.Background(), client.New(srv.URL, syntheticKey("bearer")), "res_1"); err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("unreachable gateway: got %v", err)
	}
}

func TestMcpInspect_FailsClosedAndRequiresSession(t *testing.T) {
	withNoGateway(t)
	if _, err := runCommand(t, McpInspectCmd(), "res_1"); err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Errorf("expected offline guidance, got %v", err)
	}
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")
	if _, err := runCommand(t, McpInspectCmd(), "res_1", "--gateway-url", "https://gateway.example.com"); !errors.Is(err, client.ErrNotLoggedIn) {
		t.Errorf("expected ErrNotLoggedIn, got %v", err)
	}
}

func TestSkillUninstall_FlagValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"x", "--from", "claude", "--scope", "everywhere"}, "invalid --scope"},
		{[]string{"x"}, "--from is required"},
		{[]string{"x", "--from", "notepad"}, `unknown --from provider "notepad"`},
	}
	for _, tc := range cases {
		if _, err := runCommand(t, SkillUninstallCmd(), tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("args %v: got %v, want %q", tc.args, err, tc.want)
		}
	}
	if plural(1) != "y" || plural(2) != "ies" {
		t.Error("plural suffix mismatch")
	}
}
