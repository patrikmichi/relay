package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

// tokenGateway answers /api/cli/token and records whether the request asked
// for an agent session.
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

func TestLogin_AgentProfileRequestsAgentTokenAndKeepsHumanSession(t *testing.T) {
	withTempHome(t)
	var sawAgent string
	srv := tokenGateway(t, &sawAgent)

	if err := keychain.WriteToken(srv.URL, "user@example.com", keychain.TokenData{AccessToken: "human-access", RefreshToken: "human-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := config.SetEmail("user@example.com"); err != nil {
		t.Fatal(err)
	}

	restore := driveCallback(t, map[string]string{"code": "fake-code"})
	defer restore()
	if _, err := Login(context.Background(), srv.URL, keychain.ProfileAgent); err != nil {
		t.Fatalf("Login: %v", err)
	}

	if sawAgent != "1" {
		t.Errorf("token request agent = %q, want 1", sawAgent)
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
	if _, err := Login(context.Background(), srv.URL, keychain.ProfileAgent); err != nil {
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
	sawAgent := "unset"
	srv := tokenGateway(t, &sawAgent)

	restore := driveCallback(t, map[string]string{"code": "fake-code"})
	defer restore()
	if _, err := Login(context.Background(), srv.URL, keychain.ProfileHuman); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if sawAgent != "" {
		t.Errorf("token request agent = %q, want none", sawAgent)
	}
	if _, err := keychain.ReadProfileToken(keychain.ProfileAgent, srv.URL, "user@example.com"); err == nil {
		t.Error("a human login must not create an agent session")
	}
}

func TestPollDeviceToken_AgentProfileSendsAgentTrue(t *testing.T) {
	for _, tc := range []struct {
		profile keychain.Profile
		want    bool
	}{{keychain.ProfileAgent, true}, {keychain.ProfileHuman, false}} {
		t.Run(string(tc.profile), func(t *testing.T) {
			withTempHome(t)
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&body)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "access-1", "refresh_token": "refresh-1", "email": "user@example.com", "expires_in": 3600,
				})
			}))
			defer srv.Close()

			if _, err := pollDeviceTokenProfile(context.Background(), srv.URL, "device-1", 0, tc.profile); err != nil {
				t.Fatalf("pollDeviceTokenProfile: %v", err)
			}
			got, present := body["agent"].(bool)
			if present != tc.want || got != tc.want {
				t.Errorf("agent in poll body = %v (present %v), want %v", got, present, tc.want)
			}
			if _, err := keychain.ReadProfileToken(tc.profile, srv.URL, "user@example.com"); err != nil {
				t.Errorf("session not stored under the %s profile: %v", tc.profile, err)
			}
		})
	}
}
