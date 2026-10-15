package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

// syntheticKey assembles a placeholder credential at runtime so no
// credential-shaped literal ever appears in source.
func syntheticKey(parts ...string) string {
	return strings.Join(append([]string{"relay", "test"}, parts...), "-")
}

// withAPIKeyGateway isolates HOME and authenticates every client.Resolve
// call through GATEWAY_API_KEY against srv.
func withAPIKeyGateway(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RELAY_EMAIL", "")
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("GATEWAY_API_KEY", syntheticKey("api"))
}

// runCommand executes cmd with args and returns stdout+stderr and the error.
func runCommand(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return out.String(), err
}

func jsonHandler(status int, body any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

func writeCorruptConfig(t *testing.T) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".config", "relay")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigSetGateway_NormalizesAndPersists(t *testing.T) {
	withNoGateway(t)

	out, err := runCommand(t, ConfigCmd(), "set-gateway", "https://Gateway.example.com/")
	if err != nil {
		t.Fatalf("set-gateway: %v", err)
	}
	if !strings.Contains(out, "Gateway URL set to: https://Gateway.example.com") {
		t.Errorf("unexpected output: %q", out)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayURL != "https://Gateway.example.com" {
		t.Errorf("persisted gateway = %q", cfg.GatewayURL)
	}

	out, err = runCommand(t, ConfigCmd(), "get-gateway")
	if err != nil {
		t.Fatalf("get-gateway: %v", err)
	}
	if strings.TrimSpace(out) != "https://Gateway.example.com" {
		t.Errorf("get-gateway printed %q", out)
	}
}

func TestConfigSetGateway_RejectsInsecureRemoteURLWithoutWriting(t *testing.T) {
	withNoGateway(t)

	_, err := runCommand(t, ConfigCmd(), "set-gateway", "http://gateway.example.com")
	if err == nil || !strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("expected plain-HTTP rejection, got %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayURL != "" {
		t.Errorf("rejected URL was persisted: %q", cfg.GatewayURL)
	}
}

func TestConfigSetGateway_RequiresExactlyOneArg(t *testing.T) {
	withNoGateway(t)
	if _, err := runCommand(t, ConfigCmd(), "set-gateway"); err == nil {
		t.Fatal("expected an argument-count error")
	}
}

func TestConfigCommands_CorruptConfigFileIsAnError(t *testing.T) {
	for _, args := range [][]string{
		{"set-gateway", "https://gateway.example.com"},
		{"get-gateway"},
		{"show"},
	} {
		t.Run(args[0], func(t *testing.T) {
			withNoGateway(t)
			writeCorruptConfig(t)
			_, err := runCommand(t, ConfigCmd(), args...)
			if err == nil || !strings.Contains(err.Error(), "parse config file") {
				t.Fatalf("expected a parse error, got %v", err)
			}
		})
	}
}

func TestConfigShow_ReportsConfiguredAndEffectiveValues(t *testing.T) {
	withNoGateway(t)
	if err := config.Save(config.Config{GatewayURL: "https://saved.example.com", Email: "saved@example.com"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_URL", "https://env.example.com")
	t.Setenv("RELAY_EMAIL", "env@example.com")

	out, err := runCommand(t, ConfigCmd(), "show")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("show output is not JSON: %v\n%s", err, out)
	}
	want := map[string]string{
		"configuredGatewayUrl": "https://saved.example.com",
		"effectiveGatewayUrl":  "https://env.example.com",
		"loggedInEmail":        "saved@example.com",
		"effectiveEmail":       "env@example.com",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestConfigShow_SucceedsWhenNothingIsConfigured(t *testing.T) {
	withNoGateway(t)
	t.Setenv("RELAY_EMAIL", "")

	out, err := runCommand(t, ConfigCmd(), "show")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if strings.Contains(out, "configuredGatewayUrl") || strings.Contains(out, "effectiveEmail") {
		t.Errorf("empty values must be omitted, got %s", out)
	}
	if !strings.Contains(out, `"effectiveGatewayUrl": ""`) {
		t.Errorf("effectiveGatewayUrl must always be present, got %s", out)
	}
}

func whoamiServer(t *testing.T, status int, body any, gotPath *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/cli/whoami" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if gotPath != nil {
			*gotPath = r.URL.RequestURI()
		}
		jsonHandler(status, body)(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWhoami_PrintsIdentityAndGroupsWithFull(t *testing.T) {
	var path string
	srv := whoamiServer(t, http.StatusOK, map[string]any{
		"email": "dev@example.com", "googleSub": "sub-1", "services": []string{"clockify", "github"},
		"issuedAt": "2026-01-01T00:00:00Z", "groups": []string{"eng", "ops"},
	}, &path)
	withAPIKeyGateway(t, srv)

	out, err := runCommand(t, WhoamiCmd(), "--full")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if path != "/api/cli/whoami?groups=true" {
		t.Errorf("--full must request groups, got %q", path)
	}
	for _, want := range []string{"Email:    dev@example.com", "Sub:      sub-1", "Services: clockify, github", "Issued:   2026-01-01T00:00:00Z", "Groups:   eng, ops"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWhoami_WithoutServicesShowsAllAndHidesGroups(t *testing.T) {
	srv := whoamiServer(t, http.StatusOK, map[string]any{
		"email": "dev@example.com", "issuedAt": "now", "groups": []string{"eng"},
	}, nil)
	withAPIKeyGateway(t, srv)

	out, err := runCommand(t, WhoamiCmd())
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if !strings.Contains(out, "Services: all") {
		t.Errorf("expected unrestricted services, got:\n%s", out)
	}
	if strings.Contains(out, "Sub:") || strings.Contains(out, "Groups:") {
		t.Errorf("empty sub and non --full groups must be hidden:\n%s", out)
	}
}

func TestWhoami_ServerErrorsAreReported(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "token expired or revoked"},
		{http.StatusInternalServerError, "whoami failed (500): boom"},
	}
	for _, tc := range cases {
		srv := whoamiServer(t, tc.status, map[string]string{"error": "boom"}, nil)
		withAPIKeyGateway(t, srv)
		_, err := runCommand(t, WhoamiCmd())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: got %v, want %q", tc.status, err, tc.want)
		}
	}
}

func TestWhoami_NotLoggedIn(t *testing.T) {
	withNoGateway(t)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")
	_, err := runCommand(t, WhoamiCmd(), "--gateway-url", "https://gateway.example.com")
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected not-logged-in error, got %v", err)
	}
}

func TestTokensList_PrintsSessionWithoutTheToken(t *testing.T) {
	srv := whoamiServer(t, http.StatusOK, map[string]any{
		"email": "dev@example.com", "services": []string{"clockify"},
		"issuedAt": "2026-01-01", "expiresAt": "2026-02-01",
	}, nil)
	withAPIKeyGateway(t, srv)

	out, err := runCommand(t, TokensCmd(), "list")
	if err != nil {
		t.Fatalf("tokens list: %v", err)
	}
	for _, want := range []string{"Email:     dev@example.com", "Services:  clockify", "Issued at: 2026-01-01", "Expires:   2026-02-01", "stored in system keychain"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, os.Getenv("GATEWAY_API_KEY")) {
		t.Fatal("tokens list must never print the bearer token")
	}
}

func TestTokensList_UnrestrictedSessionWithoutExpiry(t *testing.T) {
	srv := whoamiServer(t, http.StatusOK, map[string]any{"email": "dev@example.com", "issuedAt": "x"}, nil)
	withAPIKeyGateway(t, srv)

	out, err := runCommand(t, TokensCmd(), "list")
	if err != nil {
		t.Fatalf("tokens list: %v", err)
	}
	if !strings.Contains(out, "Services:  all") || strings.Contains(out, "Expires:") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestTokensList_ErrorResponses(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"unauthorized", jsonHandler(http.StatusUnauthorized, nil), "token expired or revoked"},
		{"server error", jsonHandler(http.StatusBadGateway, nil), "tokens list failed (HTTP 502)"},
		{"bad body", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }, "decode response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			withAPIKeyGateway(t, srv)
			_, err := runCommand(t, TokensCmd(), "list")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestTokensList_FailsClosedWithoutGateway(t *testing.T) {
	withNoGateway(t)
	_, err := runCommand(t, TokensCmd(), "list")
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offline guidance, got %v", err)
	}
}

// logoutServer counts POST /api/cli/logout calls and answers with status.
func logoutServer(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/cli/logout" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		calls.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func sessionCommands() map[string]func() (*cobra.Command, []string) {
	return map[string]func() (*cobra.Command, []string){
		"logout":        func() (*cobra.Command, []string) { return LogoutCmd(), nil },
		"tokens revoke": func() (*cobra.Command, []string) { return TokensCmd(), []string{"revoke"} },
	}
}

func TestSessionRemoval_RevokesRemotelyAndDeletesLocalSession(t *testing.T) {
	for name, build := range sessionCommands() {
		for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
			t.Run(name, func(t *testing.T) {
				srv, calls := logoutServer(t, status)
				withNoGateway(t)
				t.Setenv("GATEWAY_API_KEY", "")
				email, origin := withLoggedInSession(t, srv.URL)

				cmd, args := build()
				out, err := runCommand(t, cmd, append(args, "--gateway-url", srv.URL)...)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if calls.Load() != 1 {
					t.Errorf("expected one remote revoke, got %d", calls.Load())
				}
				if !strings.Contains(out, "Logged out "+email) && !strings.Contains(out, "Session revoked") {
					t.Errorf("unexpected output %q", out)
				}
				if _, err := keychain.ReadToken(origin, email); !errors.Is(err, keychain.ErrNotFound) {
					t.Errorf("local session must be removed even when the server fails, ReadToken err = %v", err)
				}
			})
		}
	}
}

func TestSessionRemoval_OfflineDeletesLocallyWithoutDialing(t *testing.T) {
	for name, build := range sessionCommands() {
		t.Run(name, func(t *testing.T) {
			srv, calls := logoutServer(t, http.StatusOK)
			withNoGateway(t)
			t.Setenv("GATEWAY_API_KEY", "")
			email, origin := withLoggedInSession(t, srv.URL)
			SetOffline(true)
			t.Cleanup(func() { SetOffline(false) })

			cmd, args := build()
			out, err := runCommand(t, cmd, append(args, "--gateway-url", srv.URL)...)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if calls.Load() != 0 {
				t.Errorf("offline removal dialed the gateway %d times", calls.Load())
			}
			if !strings.Contains(out, "Offline — skipped server-side session revoke") {
				t.Errorf("unexpected output %q", out)
			}
			if _, err := keychain.ReadToken(origin, email); !errors.Is(err, keychain.ErrNotFound) {
				t.Errorf("local session not removed: %v", err)
			}
		})
	}
}

func TestSessionRemoval_NoGatewayErrors(t *testing.T) {
	for name, build := range sessionCommands() {
		t.Run(name, func(t *testing.T) {
			withNoGateway(t)
			cmd, args := build()
			if _, err := runCommand(t, cmd, args...); err == nil || !strings.Contains(err.Error(), offlineGuidance) {
				t.Errorf("online: expected offline guidance, got %v", err)
			}

			SetOffline(true)
			t.Cleanup(func() { SetOffline(false) })
			cmd, args = build()
			if _, err := runCommand(t, cmd, args...); err == nil || !strings.Contains(err.Error(), "pass --gateway-url") {
				t.Errorf("offline: expected a --gateway-url hint, got %v", err)
			}
		})
	}
}

func TestSessionRemoval_RefusesAPIKeySessions(t *testing.T) {
	for name, build := range sessionCommands() {
		t.Run(name, func(t *testing.T) {
			srv, calls := logoutServer(t, http.StatusOK)
			withAPIKeyGateway(t, srv)
			cmd, args := build()
			_, err := runCommand(t, cmd, args...)
			if err == nil || !strings.Contains(err.Error(), "GATEWAY_API_KEY") {
				t.Fatalf("expected GATEWAY_API_KEY refusal, got %v", err)
			}
			if calls.Load() != 0 {
				t.Error("an API-key session must never be revoked remotely")
			}
		})
	}
}

func TestSessionRemoval_InvalidGatewayURL(t *testing.T) {
	for name, build := range sessionCommands() {
		t.Run(name, func(t *testing.T) {
			withNoGateway(t)
			cmd, args := build()
			_, err := runCommand(t, cmd, append(args, "--gateway-url", "ftp://gateway.example.com")...)
			if err == nil || !strings.Contains(err.Error(), "unsupported scheme") {
				t.Fatalf("expected scheme rejection, got %v", err)
			}
		})
	}
}

func TestSessionRemoval_NotLoggedIn(t *testing.T) {
	for name, build := range sessionCommands() {
		t.Run(name, func(t *testing.T) {
			withNoGateway(t)
			t.Setenv("GATEWAY_API_KEY", "")
			t.Setenv("RELAY_EMAIL", "")
			cmd, args := build()
			_, err := runCommand(t, cmd, append(args, "--gateway-url", "https://gateway.example.com")...)
			if err == nil || !strings.Contains(err.Error(), "not logged in") {
				t.Fatalf("expected not-logged-in error, got %v", err)
			}
		})
	}
}

func TestLegacySessionRemoval(t *testing.T) {
	for name, build := range sessionCommands() {
		t.Run(name, func(t *testing.T) {
			withNoGateway(t)
			email := "legacy-" + strings.ReplaceAll(name, " ", "-") + "@example.com"
			t.Setenv("RELAY_EMAIL", email)
			legacyAccount := "oauth-refresh-token:" + email
			raw, _ := json.Marshal(keychain.TokenData{AccessToken: syntheticKey("legacy")})
			if err := keyring.Set("relay-cli", legacyAccount, string(raw)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = keychain.DeleteLegacyToken(email) })

			cmd, args := build()
			out, err := runCommand(t, cmd, append(args, "--legacy")...)
			if err != nil {
				t.Fatalf("--legacy: %v", err)
			}
			if !strings.Contains(out, "Removed legacy session for "+email) {
				t.Errorf("unexpected output %q", out)
			}
			if keychain.HasLegacyToken(email) {
				t.Fatal("legacy session still present")
			}

			cmd, args = build()
			out, err = runCommand(t, cmd, append(args, "--legacy")...)
			if err != nil {
				t.Fatalf("second --legacy: %v", err)
			}
			if !strings.Contains(out, "No legacy session found") {
				t.Errorf("second run should report nothing to remove, got %q", out)
			}
		})
	}
}

func TestLegacySessionRemoval_RequiresIdentity(t *testing.T) {
	withNoGateway(t)
	t.Setenv("RELAY_EMAIL", "")
	_, err := runCommand(t, LogoutCmd(), "--legacy")
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected identity error, got %v", err)
	}
}

func TestLogin_FailsClosedWithoutGatewayOrWhenOffline(t *testing.T) {
	withNoGateway(t)
	for _, args := range [][]string{nil, {"--device"}} {
		if _, err := runCommand(t, LoginCmd(), args...); err == nil || !strings.Contains(err.Error(), offlineGuidance) {
			t.Errorf("args %v: expected offline guidance, got %v", args, err)
		}
	}

	withOffline(t)
	if _, err := runCommand(t, LoginCmd(), "--gateway-url", "https://gateway.example.com"); err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Errorf("--offline: expected offline guidance, got %v", err)
	}
}

func TestLogin_DeviceFlowServerErrorIsWrapped(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(http.StatusInternalServerError, map[string]string{"error": "down"}))
	t.Cleanup(srv.Close)
	withNoGateway(t)

	_, err := runCommand(t, LoginCmd(), "--device", "--gateway-url", srv.URL)
	if err == nil || !strings.HasPrefix(err.Error(), "login failed:") {
		t.Fatalf("expected wrapped login failure, got %v", err)
	}
}

func integrationsFixture() IntegrationsResponse {
	return IntegrationsResponse{
		OK: true, ServiceCount: 3, ToolCount: 3,
		Services: []DiscoveryService{
			{ID: "clockify", Name: "Clockify", Accessible: true, ToolCount: 2, Tools: []DiscoveryTool{
				{Name: "list_workspaces", Description: "List workspaces"},
				{Name: "raw_tool"},
			}},
			{ID: "github", Name: "GitHub", Accessible: false, ToolCount: 1, Tools: []DiscoveryTool{{Name: "list_repos"}}},
			{ID: "empty", Name: "Empty", Accessible: true},
		},
	}
}

func integrationsServer(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/integrations" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		jsonHandler(status, body)(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServices_ListsEveryServiceAndMarksInaccessible(t *testing.T) {
	withAPIKeyGateway(t, integrationsServer(t, http.StatusOK, integrationsFixture()))

	out, err := runCommand(t, ServicesCmd())
	if err != nil {
		t.Fatalf("services: %v", err)
	}
	if !strings.Contains(out, "Services (3 tools total):") {
		t.Errorf("missing header:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "github") && !strings.Contains(line, "(no access)") {
			t.Errorf("inaccessible service not marked: %q", line)
		}
		if strings.Contains(line, "clockify") && strings.Contains(line, "(no access)") {
			t.Errorf("accessible service marked inaccessible: %q", line)
		}
	}
}

func TestServices_ErrorResponses(t *testing.T) {
	cases := []struct {
		status int
		body   any
		want   string
	}{
		{http.StatusUnauthorized, nil, "token expired or revoked"},
		{http.StatusServiceUnavailable, nil, "integrations request failed (503)"},
		{http.StatusOK, IntegrationsResponse{OK: false, Error: "registry down"}, "integrations request failed: registry down"},
	}
	for _, tc := range cases {
		withAPIKeyGateway(t, integrationsServer(t, tc.status, tc.body))
		_, err := runCommand(t, ServicesCmd())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: got %v, want %q", tc.status, err, tc.want)
		}
	}
}

func TestHelpTools_AllServicesListsOnlyAccessibleOnes(t *testing.T) {
	withAPIKeyGateway(t, integrationsServer(t, http.StatusOK, integrationsFixture()))

	out, err := runCommand(t, HelpToolsCmd())
	if err != nil {
		t.Fatalf("help-tools: %v", err)
	}
	if !strings.Contains(out, "clockify (2 tools)") || !strings.Contains(out, "empty (0 tools)") {
		t.Errorf("accessible services missing:\n%s", out)
	}
	if strings.Contains(out, "github") {
		t.Errorf("inaccessible service listed without being requested:\n%s", out)
	}
	if !strings.Contains(out, "list_workspaces") || !strings.Contains(out, "List workspaces") || !strings.Contains(out, "  raw_tool\n") {
		t.Errorf("tool lines missing:\n%s", out)
	}
}

func TestHelpTools_NamedServiceIncludingInaccessible(t *testing.T) {
	withAPIKeyGateway(t, integrationsServer(t, http.StatusOK, integrationsFixture()))

	out, err := runCommand(t, HelpToolsCmd(), "github")
	if err != nil {
		t.Fatalf("help-tools github: %v", err)
	}
	if !strings.Contains(out, "github (1 tools)  (no access)") || strings.Contains(out, "clockify") {
		t.Errorf("unexpected output:\n%s", out)
	}

	if _, err := runCommand(t, HelpToolsCmd(), "nope"); err == nil || !strings.Contains(err.Error(), "unknown service: nope") {
		t.Errorf("expected unknown service error, got %v", err)
	}
}

func TestHelpTools_DegradesWhenNothingIsAccessible(t *testing.T) {
	noAccess := IntegrationsResponse{OK: true, Services: []DiscoveryService{{ID: "github", Accessible: false}}}
	for name, srvFn := range map[string]func(*testing.T) *httptest.Server{
		"unauthorized": func(t *testing.T) *httptest.Server { return integrationsServer(t, http.StatusUnauthorized, nil) },
		"no access":    func(t *testing.T) *httptest.Server { return integrationsServer(t, http.StatusOK, noAccess) },
		"empty catalog": func(t *testing.T) *httptest.Server {
			return integrationsServer(t, http.StatusOK, IntegrationsResponse{OK: true})
		},
	} {
		t.Run(name, func(t *testing.T) {
			withAPIKeyGateway(t, srvFn(t))
			out, err := runCommand(t, HelpToolsCmd())
			if err != nil {
				t.Fatalf("help-tools: %v", err)
			}
			if !strings.Contains(out, "No accessible services found.") {
				t.Errorf("unexpected output:\n%s", out)
			}
		})
	}
}

func TestHelpTools_ErrorsAndFailClosed(t *testing.T) {
	withAPIKeyGateway(t, integrationsServer(t, http.StatusInternalServerError, nil))
	if _, err := runCommand(t, HelpToolsCmd()); err == nil || !strings.Contains(err.Error(), "fetch integrations") {
		t.Errorf("expected fetch error, got %v", err)
	}

	withNoGateway(t)
	if _, err := runCommand(t, HelpToolsCmd()); err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Errorf("expected offline guidance, got %v", err)
	}

	if _, err := runCommand(t, HelpToolsCmd(), "a", "b"); err == nil {
		t.Error("expected an argument-count error")
	}
}

func TestBuildServiceCommands_RegistersCallableToolCommands(t *testing.T) {
	var toolCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/integrations":
			jsonHandler(http.StatusOK, integrationsFixture())(w, r)
		case "/api/clockify/mcp":
			toolCalls.Add(1)
			var req struct {
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Params.Name != "list_workspaces" || req.Params.Arguments["limit"] != float64(5) {
				t.Errorf("unexpected tools/call params: %+v", req.Params)
			}
			jsonHandler(http.StatusOK, map[string]any{
				"jsonrpc": "2.0", "id": 1,
				"result": map[string]any{"content": []map[string]string{{"type": "text", "text": "workspace-a"}}},
			})(w, r)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	withAPIKeyGateway(t, srv)

	root := &cobra.Command{Use: "relay"}
	if err := BuildServiceCommands(root, srv.URL); err != nil {
		t.Fatalf("BuildServiceCommands: %v", err)
	}
	var names []string
	for _, c := range root.Commands() {
		names = append(names, c.Name())
	}
	if strings.Join(names, ",") != "clockify" {
		t.Fatalf("registered services = %v, want only clockify", names)
	}

	out, err := runCommand(t, root, "clockify", "list_workspaces", "--arg", "limit=5")
	if err != nil {
		t.Fatalf("tool command: %v", err)
	}
	if toolCalls.Load() != 1 || !strings.Contains(out, "workspace-a") {
		t.Errorf("tool call not rendered (calls=%d): %q", toolCalls.Load(), out)
	}
}

func TestBuildServiceCommands_DegradesOnDiscoveryFailure(t *testing.T) {
	withAPIKeyGateway(t, integrationsServer(t, http.StatusInternalServerError, nil))
	root := &cobra.Command{Use: "relay"}
	if err := BuildServiceCommands(root, os.Getenv("GATEWAY_URL")); err != nil {
		t.Fatalf("discovery failure must not abort startup: %v", err)
	}
	if len(root.Commands()) != 0 {
		t.Errorf("no commands should be registered, got %d", len(root.Commands()))
	}
}

func TestCommandNameCollides_MatchesAliases(t *testing.T) {
	root := &cobra.Command{Use: "relay"}
	root.AddCommand(&cobra.Command{Use: "skill", Aliases: []string{"skills"}})
	for name, want := range map[string]bool{"skill": true, "skills": true, "completion": true, "clockify": false} {
		if got := commandNameCollides(root, name); got != want {
			t.Errorf("commandNameCollides(%q) = %v, want %v", name, got, want)
		}
	}
}
