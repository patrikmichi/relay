package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

// init installs a mock keyring so these tests never touch the real OS keychain.
func init() {
	keyring.MockInit()
}

// withTempHome redirects os.UserHomeDir (via $HOME) to an isolated temp dir
// for the duration of the test, so config.Load/Save never touch the real
// developer machine's ~/.config/relay.
func withTempHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func seedSession(t *testing.T, email string, tok keychain.TokenData) {
	t.Helper()
	if err := keychain.WriteToken(email, tok); err != nil {
		t.Fatalf("seed keychain token: %v", err)
	}
	if err := config.SetEmail(email); err != nil {
		t.Fatalf("seed config email: %v", err)
	}
}

// ─── Resolve ────────────────────────────────────────────────────────────────

func TestResolve_GatewayAPIKeyTakesPrecedence(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "static-bearer-key")
	t.Setenv("RELAY_EMAIL", "someone@example.com") // present but must be ignored

	c, err := client.Resolve("https://gw.example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.AccessToken != "static-bearer-key" {
		t.Errorf("AccessToken: got %q, want the API key", c.AccessToken)
	}
	if c.Email() != "" {
		t.Errorf("Email(): got %q, want empty for an API-key client", c.Email())
	}
}

func TestResolve_NoSessionReturnsErrNotLoggedIn(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")

	_, err := client.Resolve("https://gw.example.com")
	if err != client.ErrNotLoggedIn {
		t.Errorf("expected ErrNotLoggedIn, got %v", err)
	}
}

func TestResolve_UsesPersistedLoginEmail(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")

	seedSession(t, "persisted@example.com", keychain.TokenData{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Email:        "persisted@example.com",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	})

	c, err := client.Resolve("https://gw.example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Email() != "persisted@example.com" {
		t.Errorf("Email(): got %q, want persisted@example.com", c.Email())
	}
	if c.AccessToken != "access-1" {
		t.Errorf("AccessToken: got %q, want access-1", c.AccessToken)
	}
}

func TestResolve_RelayEmailOverridesPersistedEmail(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")

	seedSession(t, "persisted@example.com", keychain.TokenData{
		AccessToken: "persisted-access", RefreshToken: "persisted-refresh", Email: "persisted@example.com",
	})
	if err := keychain.WriteToken("override@example.com", keychain.TokenData{
		AccessToken: "override-access", RefreshToken: "override-refresh", Email: "override@example.com",
	}); err != nil {
		t.Fatalf("seed override token: %v", err)
	}
	t.Setenv("RELAY_EMAIL", "override@example.com")

	c, err := client.Resolve("https://gw.example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Email() != "override@example.com" {
		t.Errorf("Email(): got %q, want override@example.com", c.Email())
	}
	if c.AccessToken != "override-access" {
		t.Errorf("AccessToken: got %q, want override-access", c.AccessToken)
	}
}

// ─── Do — reactive refresh-and-retry-once on 401 ──────────────────────────────

func TestDo_RefreshesAndRetriesOnceOn401(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")

	var apiHits int
	var refreshHits int
	var capturedAuthOnSuccess string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/cli/refresh", func(w http.ResponseWriter, r *http.Request) {
		refreshHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "rotated-access",
			"refresh_token": "rotated-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	mux.HandleFunc("/api/test", func(w http.ResponseWriter, r *http.Request) {
		apiHits++
		auth := r.Header.Get("Authorization")
		if auth == "Bearer stale-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		capturedAuthOnSuccess = auth
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	seedSession(t, "user@example.com", keychain.TokenData{
		AccessToken:  "stale-access",
		RefreshToken: "refresh-1",
		Email:        "user@example.com",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(), // not near expiry — only the reactive path should fire
	})

	c, err := client.Resolve(srv.URL)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	resp, err := c.Get("/api/test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("final status: got %d, want 200 (retry with rotated token should succeed)", resp.StatusCode)
	}
	if apiHits != 2 {
		t.Errorf("api hits: got %d, want 2 (initial 401 + retry)", apiHits)
	}
	if refreshHits != 1 {
		t.Errorf("refresh hits: got %d, want exactly 1", refreshHits)
	}
	if capturedAuthOnSuccess != "Bearer rotated-access" {
		t.Errorf("retry Authorization: got %q, want Bearer rotated-access", capturedAuthOnSuccess)
	}
	if c.AccessToken != "rotated-access" {
		t.Errorf("Client.AccessToken not updated: got %q", c.AccessToken)
	}

	// The rotated pair must be persisted — a fresh Resolve should see it.
	c2, err := client.Resolve(srv.URL)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if c2.AccessToken != "rotated-access" {
		t.Errorf("persisted AccessToken: got %q, want rotated-access", c2.AccessToken)
	}
}

func TestDo_ReturnsOriginalStyle401WhenRefreshFails(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/cli/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "refresh token reuse detected"})
	})
	mux.HandleFunc("/api/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	seedSession(t, "user@example.com", keychain.TokenData{
		AccessToken: "stale-access", RefreshToken: "dead-refresh", Email: "user@example.com",
	})

	c, err := client.Resolve(srv.URL)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	resp, err := c.Get("/api/test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401 (refresh failed, must degrade to the original re-login contract)", resp.StatusCode)
	}
}

func TestDo_NoRefreshCapability_SendsOnce(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	// client.New has no email/refresh token — matches a GATEWAY_API_KEY client.
	c := client.New(srv.URL, "static-key")
	resp, err := c.Get("/api/test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if hits != 1 {
		t.Errorf("hits: got %d, want exactly 1 (no refresh capability => no retry)", hits)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
}

// ─── Do — proactive refresh ahead of a known expiry ───────────────────────────

func TestDo_ProactivelyRefreshesNearExpiry(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")

	var refreshHits int
	var capturedAuth string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/cli/refresh", func(w http.ResponseWriter, r *http.Request) {
		refreshHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "fresh-access",
			"refresh_token": "fresh-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	mux.HandleFunc("/api/test", func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Expiry is already in the past — must trigger a PROACTIVE refresh
	// before the request is even sent (so the server should never see the
	// stale token at all).
	seedSession(t, "user@example.com", keychain.TokenData{
		AccessToken:  "about-to-expire",
		RefreshToken: "refresh-1",
		Email:        "user@example.com",
		ExpiresIn:    1,
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	})

	c, err := client.Resolve(srv.URL)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	resp, err := c.Get("/api/test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if refreshHits != 1 {
		t.Errorf("refresh hits: got %d, want exactly 1 (proactive)", refreshHits)
	}
	if capturedAuth != "Bearer fresh-access" {
		t.Errorf("Authorization sent: got %q, want Bearer fresh-access (stale token should never be sent)", capturedAuth)
	}
}

func TestDo_DoesNotRefreshWhenFarFromExpiry(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")

	var refreshHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/cli/refresh", func(w http.ResponseWriter, r *http.Request) {
		refreshHits++
	})
	mux.HandleFunc("/api/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	seedSession(t, "user@example.com", keychain.TokenData{
		AccessToken:  "still-valid",
		RefreshToken: "refresh-1",
		Email:        "user@example.com",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	})

	c, err := client.Resolve(srv.URL)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	resp, err := c.Get("/api/test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if refreshHits != 0 {
		t.Errorf("refresh hits: got %d, want 0 (token is nowhere near expiry)", refreshHits)
	}
}

// ─── Post retry (request body re-send) ────────────────────────────────────────

func TestDo_PostRetriesWithBodyIntactAfter401(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")

	var bodies []string
	var attempt int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/cli/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		attempt++
		buf := make([]byte, 128)
		n, _ := r.Body.Read(buf)
		bodies = append(bodies, string(buf[:n]))
		if attempt == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	seedSession(t, "user@example.com", keychain.TokenData{
		AccessToken: "stale", RefreshToken: "refresh-1", Email: "user@example.com",
		ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	c, err := client.Resolve(srv.URL)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	resp, err := c.Post("/api/echo", "application/json", strings.NewReader(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(bodies))
	}
	if bodies[0] != `{"hello":"world"}` || bodies[1] != `{"hello":"world"}` {
		t.Errorf("body not preserved across retry: %#v", bodies)
	}
}
