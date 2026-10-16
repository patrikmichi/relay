package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/keychain"
)

// withAgentAndHumanSessions signs the same account in under both profiles.
func withAgentAndHumanSessions(t *testing.T, gatewayURL string) (email, origin string) {
	t.Helper()
	email, origin = withLoggedInSession(t, gatewayURL)
	if err := keychain.WriteProfileToken(keychain.ProfileAgent, origin, email, keychain.TokenData{
		AccessToken: "agent-access-token", RefreshToken: "agent-refresh-token",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keychain.DeleteProfileToken(keychain.ProfileAgent, origin, email) })
	return email, origin
}

func TestWhoami_NamesTheProfileInUse(t *testing.T) {
	var bearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer = r.Header.Get("Authorization")
		jsonHandler(http.StatusOK, map[string]any{"email": "offline-test@example.com", "issuedAt": "now"})(w, r)
	}))
	defer srv.Close()
	withNoGateway(t)
	t.Setenv("ZUANO_API_KEY", "")
	withAgentAndHumanSessions(t, srv.URL)

	t.Setenv("ZUANO_AGENT", "")
	out, err := runCommand(t, WhoamiCmd(), "--gateway-url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Profile:  human") || bearer != "Bearer test-access-token" {
		t.Errorf("human whoami used %q:\n%s", bearer, out)
	}

	t.Setenv("ZUANO_AGENT", "1")
	out, err = runCommand(t, WhoamiCmd(), "--gateway-url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Profile:  agent (ZUANO_AGENT=1") || bearer != "Bearer agent-access-token" {
		t.Errorf("agent whoami used %q:\n%s", bearer, out)
	}
}

func TestWhoami_AgentModeWithoutAgentSessionDoesNotUseTheHumanOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the gateway must not be called without an agent session")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withNoGateway(t)
	t.Setenv("ZUANO_API_KEY", "")
	withLoggedInSession(t, srv.URL)
	t.Setenv("ZUANO_AGENT", "1")

	_, err := runCommand(t, WhoamiCmd(), "--gateway-url", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "relay login --agent") {
		t.Fatalf("err = %v, want the agent login hint", err)
	}
}

func TestLogout_AgentProfileRemovesOnlyTheAgentSession(t *testing.T) {
	srv, calls := logoutServer(t, http.StatusOK)
	withNoGateway(t)
	t.Setenv("ZUANO_API_KEY", "")
	email, origin := withAgentAndHumanSessions(t, srv.URL)
	t.Setenv("ZUANO_AGENT", "1")

	out, err := runCommand(t, LogoutCmd(), "--gateway-url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !strings.Contains(out, "(agent profile)") {
		t.Errorf("revokes = %d, output %q", calls.Load(), out)
	}
	if _, err := keychain.ReadProfileToken(keychain.ProfileAgent, origin, email); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("agent session not removed: %v", err)
	}
	if _, err := keychain.ReadToken(origin, email); err != nil {
		t.Errorf("human session was removed: %v", err)
	}
}

func TestLogin_OffersTheAgentFlag(t *testing.T) {
	if LoginCmd().Flags().Lookup("agent") == nil {
		t.Fatal("relay login has no --agent flag")
	}
}
