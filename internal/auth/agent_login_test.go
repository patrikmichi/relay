package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

// tokenGateway answers /api/cli/token and records whether the redeem request
// carried an agent field, which the gateway ignores.
func tokenGateway(t *testing.T, sawAgent *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*sawAgent = r.PostForm.Get("agent")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "agent-access", "refresh_token": "agent-refresh",
			"token_type": "Bearer", "expires_in": 3600, "email": "user@example.com",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recordAuthorizeURL wraps the stubbed browser so a test can read the URL the
// login opened.
func recordAuthorizeURL(got *string) func() {
	inner := openBrowser
	openBrowser = func(u string) error {
		*got = u
		return inner(u)
	}
	return func() { openBrowser = inner }
}

func TestLogin_AgentProfileAsksForAgentAtAuthorizeAndKeepsHumanSession(t *testing.T) {
	withTempHome(t)
	var sawAgent, authorizeURL string
	srv := tokenGateway(t, &sawAgent)

	if err := keychain.WriteToken(srv.URL, "user@example.com", keychain.TokenData{AccessToken: "human-access", RefreshToken: "human-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := config.SetEmail("user@example.com"); err != nil {
		t.Fatal(err)
	}

	restore := driveCallback(t, map[string]string{"code": "fake-code"})
	defer restore()
	defer recordAuthorizeURL(&authorizeURL)()
	var diag bytes.Buffer
	if _, err := Login(context.Background(), srv.URL, keychain.ProfileAgent, &diag); err != nil {
		t.Fatalf("Login: %v", err)
	}

	if !strings.Contains(authorizeURL, "principal=cli_agent") || strings.Contains(authorizeURL, "agent=1") {
		t.Errorf("authorize URL %q lacks principal=cli_agent or still carries agent=1", authorizeURL)
	}
	if sawAgent != "" {
		t.Errorf("token request agent = %q, want none", sawAgent)
	}
	if !strings.Contains(diag.String(), "agent session") {
		t.Errorf("diagnostic %q does not name the agent session", diag.String())
	}
	agent, err := keychain.ReadProfileToken(keychain.ProfileAgent, srv.URL, "user@example.com")
	if err != nil || agent.AccessToken != "agent-access" {
		t.Fatalf("agent session = %+v, %v", agent, err)
	}
	human, err := keychain.ReadToken(srv.URL, "user@example.com")
	if err != nil || human.AccessToken != "human-access" {
		t.Fatalf("human session was changed: %+v, %v", human, err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentEmail != "user@example.com" || cfg.Email != "user@example.com" {
		t.Errorf("config = %+v", cfg)
	}
}

func TestLogin_AgentLoginDoesNotSetTheHumanEmail(t *testing.T) {
	withTempHome(t)
	var sawAgent string
	srv := tokenGateway(t, &sawAgent)

	restore := driveCallback(t, map[string]string{"code": "fake-code"})
	defer restore()
	if _, err := Login(context.Background(), srv.URL, keychain.ProfileAgent, &bytes.Buffer{}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Email != "" || cfg.AgentEmail != "user@example.com" {
		t.Errorf("config = %+v", cfg)
	}
}

func TestLogin_HumanProfileSendsNoAgentFlag(t *testing.T) {
	withTempHome(t)
	var sawAgent, authorizeURL string
	srv := tokenGateway(t, &sawAgent)

	restore := driveCallback(t, map[string]string{"code": "fake-code"})
	defer restore()
	defer recordAuthorizeURL(&authorizeURL)()
	var diag bytes.Buffer
	if _, err := Login(context.Background(), srv.URL, keychain.ProfileHuman, &diag); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !strings.Contains(authorizeURL, "principal=cli_personal") || strings.Contains(authorizeURL, "agent=") || sawAgent != "" {
		t.Errorf("personal login must ask for principal=cli_personal and send no agent flag (authorize %q, token %q)", authorizeURL, sawAgent)
	}
	if !strings.Contains(diag.String(), "personal session") {
		t.Errorf("diagnostic %q does not name the personal session", diag.String())
	}
	if _, err := keychain.ReadProfileToken(keychain.ProfileAgent, srv.URL, "user@example.com"); err == nil {
		t.Error("a human login must not create an agent session")
	}
}

func TestDeviceLogin_PrincipalGoesOnTheCodeRequestOnly(t *testing.T) {
	for _, tc := range []struct {
		profile keychain.Profile
		want    bool
	}{{keychain.ProfileAgent, true}, {keychain.ProfileHuman, false}} {
		t.Run(string(tc.profile), func(t *testing.T) {
			withTempHome(t)
			var codeBody, pollBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/cli/device/code":
					_ = json.NewDecoder(r.Body).Decode(&codeBody)
					_, _ = w.Write([]byte(validDeviceCode))
				case "/api/cli/device/token":
					_ = json.NewDecoder(r.Body).Decode(&pollBody)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"access_token": "access-1", "refresh_token": "refresh-1", "email": "user@example.com", "expires_in": 3600,
					})
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer srv.Close()

			var out bytes.Buffer
			if _, err := DeviceLogin(context.Background(), srv.URL, tc.profile, &out); err != nil {
				t.Fatalf("DeviceLogin: %v", err)
			}
			wantPrincipal := "cli_personal"
			if tc.want {
				wantPrincipal = "cli_agent"
			}
			if codeBody["principal"] != wantPrincipal {
				t.Errorf("principal in device-code body = %v, want %v", codeBody["principal"], wantPrincipal)
			}
			if _, present := codeBody["agent"]; present {
				t.Errorf("device-code body still carries the agent flag: %v", codeBody)
			}
			for _, key := range []string{"agent", "principal"} {
				if _, present := pollBody[key]; present {
					t.Errorf("poll body carries %s: %v", key, pollBody)
				}
			}
			want := "personal session"
			if tc.want {
				want = "agent session"
			}
			if !strings.Contains(out.String(), want) {
				t.Errorf("output %q does not name the %s", out.String(), want)
			}
			if _, err := keychain.ReadProfileToken(tc.profile, srv.URL, "user@example.com"); err != nil {
				t.Errorf("session not stored under the %s profile: %v", tc.profile, err)
			}
		})
	}
}
