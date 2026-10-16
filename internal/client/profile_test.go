package client_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/keychain"
)

const profileGateway = "https://gw.example.com"

func seedProfile(t *testing.T, profile keychain.Profile, origin, email, access, refresh string, expiresAt time.Time) {
	t.Helper()
	if err := keychain.WriteProfileToken(profile, origin, email, keychain.TokenData{
		AccessToken: access, RefreshToken: refresh, ExpiresIn: 3600, ExpiresAt: expiresAt.Unix(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResolve_AgentModeUsesTheAgentSession(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")
	t.Setenv("RELAY_AGENT", "1")
	seedProfile(t, keychain.ProfileHuman, profileGateway, "user@example.com", "human-access", "human-refresh", time.Now().Add(time.Hour))
	seedProfile(t, keychain.ProfileAgent, profileGateway, "user@example.com", "agent-access", "agent-refresh", time.Now().Add(time.Hour))
	if err := config.SetEmail("user@example.com"); err != nil {
		t.Fatal(err)
	}

	c, err := client.Resolve(profileGateway)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.AccessToken != "agent-access" || c.Profile() != keychain.ProfileAgent {
		t.Errorf("client = %q profile %q, want the agent session", c.AccessToken, c.Profile())
	}

	t.Setenv("RELAY_AGENT", "")
	c, err = client.Resolve(profileGateway)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.AccessToken != "human-access" || c.Profile() != keychain.ProfileHuman {
		t.Errorf("client = %q profile %q, want the human session", c.AccessToken, c.Profile())
	}
}

func TestResolve_AgentModeNeverFallsBackToTheHumanSession(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")
	t.Setenv("RELAY_AGENT", "1")
	seedProfile(t, keychain.ProfileHuman, profileGateway, "human-only@example.com", "human-access", "human-refresh", time.Now().Add(time.Hour))
	if err := config.SetEmail("human-only@example.com"); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Resolve(profileGateway); !errors.Is(err, client.ErrAgentNotLoggedIn) {
		t.Fatalf("Resolve = %v, want ErrAgentNotLoggedIn", err)
	}
}

func TestResolve_AgentModeWithNoIdentityIsAgentNotLoggedIn(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")
	t.Setenv("RELAY_AGENT", "1")

	if _, err := client.Resolve(profileGateway); !errors.Is(err, client.ErrAgentNotLoggedIn) {
		t.Fatalf("Resolve = %v, want ErrAgentNotLoggedIn", err)
	}
}

func TestResolve_APIKeyReportsTheActiveProfile(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "static-bearer-key")
	t.Setenv("RELAY_API_KEY_ORIGIN", profileGateway)
	t.Setenv("RELAY_AGENT", "1")

	c, err := client.Resolve(profileGateway)
	if err != nil {
		t.Fatal(err)
	}
	if c.Profile() != keychain.ProfileAgent {
		t.Errorf("profile = %q, want agent", c.Profile())
	}
}

func TestDo_AgentRefreshRotatesOnlyTheAgentSession(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")
	t.Setenv("RELAY_AGENT", "1")

	var refreshedWith string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cli/refresh" {
			raw, _ := io.ReadAll(r.Body)
			refreshedWith = string(raw)
			_, _ = io.WriteString(w, `{"access_token":"agent-rotated","refresh_token":"agent-refresh-2","token_type":"Bearer","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer agent-rotated" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	seedProfile(t, keychain.ProfileHuman, srv.URL, "user@example.com", "human-access", "human-refresh", time.Now().Add(time.Hour))
	seedProfile(t, keychain.ProfileAgent, srv.URL, "user@example.com", "agent-access", "agent-refresh", time.Now().Add(time.Hour))
	if err := config.SetEmail("user@example.com"); err != nil {
		t.Fatal(err)
	}

	c, err := client.Resolve(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("/api/test")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the refresh", resp.StatusCode)
	}

	if want := `{"refresh_token":"agent-refresh"}`; refreshedWith != want {
		t.Errorf("refresh body = %s, want the agent refresh token %s", refreshedWith, want)
	}
	agent, err := keychain.ReadProfileToken(keychain.ProfileAgent, srv.URL, "user@example.com")
	if err != nil || agent.RefreshToken != "agent-refresh-2" {
		t.Errorf("agent session = %+v, %v", agent, err)
	}
	human, err := keychain.ReadToken(srv.URL, "user@example.com")
	if err != nil || human.RefreshToken != "human-refresh" {
		t.Errorf("the human session changed: %+v, %v", human, err)
	}
}
