package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

// seedSession seeds both the keychain token and the persisted login email,
// so a subsequent client.Resolve(gatewayURL) returns a Client with Email()
// set to email (client.Resolve is the only way to get an email-bound
// Client — its `email` field is unexported and set solely from
// config.ResolveEmail + a matching keychain entry).
func seedSession(t *testing.T, gatewayOrigin, email string, tok keychain.TokenData) {
	t.Helper()
	if err := keychain.WriteToken(gatewayOrigin, email, tok); err != nil {
		t.Fatalf("seed keychain token: %v", err)
	}
	if err := config.SetEmail(email); err != nil {
		t.Fatalf("seed config email: %v", err)
	}
}

func resolveSeededClient(t *testing.T, gatewayURL string) *client.Client {
	t.Helper()
	t.Setenv("GATEWAY_API_KEY", "")
	c, err := client.Resolve(gatewayURL)
	if err != nil {
		t.Fatalf("client.Resolve: %v", err)
	}
	return c
}

func TestLogout_Success(t *testing.T) {
	withTempHome(t)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	seedSession(t, srv.URL, "user@example.com", keychain.TokenData{AccessToken: "a", RefreshToken: "r"})
	c := resolveSeededClient(t, srv.URL)

	result, err := Logout(context.Background(), c)
	if err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if !result.RemoteRevoked {
		t.Errorf("expected RemoteRevoked=true, got warning: %q", result.RemoteWarning)
	}
	if gotPath != "/api/cli/logout" {
		t.Errorf("expected POST to /api/cli/logout, got %q", gotPath)
	}
	if _, err := keychain.ReadToken(srv.URL, "user@example.com"); err == nil {
		t.Error("expected keychain entry to be deleted after logout")
	}
}

func TestLogout_ToleratesUnreachableServer(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	unreachable := srv.URL
	srv.Close()

	seedSession(t, unreachable, "user@example.com", keychain.TokenData{AccessToken: "a", RefreshToken: "r"})
	c := resolveSeededClient(t, unreachable)

	result, err := Logout(context.Background(), c)
	if err != nil {
		t.Fatalf("Logout should tolerate an unreachable gateway, got: %v", err)
	}
	if result.RemoteRevoked {
		t.Error("expected RemoteRevoked=false for an unreachable gateway")
	}
	if result.RemoteWarning == "" {
		t.Error("expected a non-empty RemoteWarning distinguishing the failed remote revoke")
	}
	if _, err := keychain.ReadToken(unreachable, "user@example.com"); err == nil {
		t.Error("expected keychain entry to still be deleted despite unreachable server")
	}
}

func TestLogout_ToleratesNon200FromServer(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	seedSession(t, srv.URL, "user@example.com", keychain.TokenData{AccessToken: "a", RefreshToken: "r"})
	c := resolveSeededClient(t, srv.URL)

	result, err := Logout(context.Background(), c)
	if err != nil {
		t.Fatalf("Logout should tolerate a non-200 revoke response, got: %v", err)
	}
	if result.RemoteRevoked {
		t.Error("expected RemoteRevoked=false for a non-200 revoke response")
	}
	if _, err := keychain.ReadToken(srv.URL, "user@example.com"); err == nil {
		t.Error("expected keychain entry to still be deleted despite server error")
	}
}

func TestLogout_ReturnsErrorWhenKeychainDeleteFails(t *testing.T) {
	withTempHome(t)
	defer keyring.MockInit() // restore a working mock for subsequent tests

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	seedSession(t, srv.URL, "user@example.com", keychain.TokenData{AccessToken: "a", RefreshToken: "r"})
	c := resolveSeededClient(t, srv.URL)

	keyring.MockInitWithError(errors.New("mock keychain failure"))
	if _, err := Logout(context.Background(), c); err == nil {
		t.Fatal("expected error when the keychain delete fails, got nil")
	}
}

// TestLogout_RemovesOnlySelectedOriginAccount proves offline/local logout
// scope: deleting the (gatewayOrigin, email) entry for the client under
// logout must never touch a DIFFERENT origin's or account's session.
func TestLogout_RemovesOnlySelectedOriginAccount(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	seedSession(t, srv.URL, "user@example.com", keychain.TokenData{AccessToken: "a", RefreshToken: "r"})
	if err := keychain.WriteToken(srv.URL, "other@example.com", keychain.TokenData{AccessToken: "a2", RefreshToken: "r2"}); err != nil {
		t.Fatalf("seed other account: %v", err)
	}
	if err := keychain.WriteToken("https://other-gateway.example.invalid", "user@example.com", keychain.TokenData{AccessToken: "a3", RefreshToken: "r3"}); err != nil {
		t.Fatalf("seed other gateway: %v", err)
	}

	c := resolveSeededClient(t, srv.URL)
	if _, err := Logout(context.Background(), c); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	if _, err := keychain.ReadToken(srv.URL, "user@example.com"); err == nil {
		t.Error("selected (gatewayOrigin, email) entry should have been removed")
	}
	if _, err := keychain.ReadToken(srv.URL, "other@example.com"); err != nil {
		t.Errorf("a different account on the SAME gateway must survive logout: %v", err)
	}
	if _, err := keychain.ReadToken("https://other-gateway.example.invalid", "user@example.com"); err != nil {
		t.Errorf("the SAME account on a different gateway must survive logout: %v", err)
	}
}
