package client

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

const migrationEmail = "user@example.com"

func setupLegacySession(t *testing.T, savedGateway string) *bytes.Buffer {
	t.Helper()
	keyring.MockInit()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("GATEWAY_URL", "")
	t.Setenv("RELAY_EMAIL", migrationEmail)
	if savedGateway != "" {
		if err := config.Save(config.Config{GatewayURL: savedGateway}); err != nil {
			t.Fatal(err)
		}
	}
	if err := keyring.Set("relay-cli", "oauth-refresh-token:"+migrationEmail,
		`{"access_token":"legacy-access","refresh_token":"legacy-refresh","email":"user@example.com","expires_at":4102444800}`); err != nil {
		t.Fatal(err)
	}
	var notice bytes.Buffer
	orig := migrationNotice
	migrationNotice = &notice
	t.Cleanup(func() { migrationNotice = orig })
	return &notice
}

func TestResolve_MigratesLegacySessionToSavedHTTPSGateway(t *testing.T) {
	notice := setupLegacySession(t, "https://gw.example.com/")

	c, err := Resolve("https://gw.example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.AccessToken != "legacy-access" || c.refreshToken != "legacy-refresh" || c.expiresAt.Unix() != 4102444800 {
		t.Fatalf("migrated client = access %q refresh %q expiry %d", c.AccessToken, c.refreshToken, c.expiresAt.Unix())
	}
	bound, err := keychain.ReadToken("https://gw.example.com", migrationEmail)
	if err != nil {
		t.Fatalf("gateway-bound session missing after migration: %v", err)
	}
	if bound.GatewayOrigin != "https://gw.example.com" || bound.RefreshToken != "legacy-refresh" {
		t.Fatalf("bound session = %+v", bound)
	}
	if keychain.HasLegacyToken(migrationEmail) {
		t.Fatal("legacy entry must be deleted after migration")
	}
	if !strings.Contains(notice.String(), "moved your saved session") {
		t.Fatalf("notice = %q, want a one-line migration notice", notice.String())
	}

	notice.Reset()
	if _, err := Resolve("https://gw.example.com"); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if notice.Len() != 0 {
		t.Fatalf("migration notice printed again: %q", notice.String())
	}
}

func TestResolve_RefusesMigrationWithoutMatchingSavedGateway(t *testing.T) {
	cases := map[string]struct{ saved, requested string }{
		"no saved gateway (flag or env only)": {"", "https://gw.example.com"},
		"different saved gateway":             {"https://other.example.com", "https://gw.example.com"},
		"loopback http":                       {"http://127.0.0.1:4000", "http://127.0.0.1:4000"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			notice := setupLegacySession(t, tc.saved)
			_, err := Resolve(tc.requested)
			if !errors.Is(err, ErrSessionMigration) {
				t.Fatalf("err = %v, want ErrSessionMigration", err)
			}
			if err.Error() != "relay now keeps one session per gateway. Run `relay login` once to continue" {
				t.Fatalf("message = %q", err.Error())
			}
			if errors.Is(err, ErrNotLoggedIn) {
				t.Fatal("the migration message must not be masked by generic not-logged-in handling")
			}
			if !keychain.HasLegacyToken(migrationEmail) {
				t.Fatal("legacy entry must be kept when it is not migrated")
			}
			if _, readErr := keychain.ReadToken(tc.requested, migrationEmail); readErr == nil {
				t.Fatal("no gateway-bound session may be created without migration")
			}
			if notice.Len() != 0 {
				t.Fatalf("unexpected notice %q", notice.String())
			}
		})
	}
}

func TestResolve_NoSessionIsNotLoggedIn(t *testing.T) {
	keyring.MockInit()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", migrationEmail)
	if _, err := Resolve("https://gw.example.com"); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v, want ErrNotLoggedIn", err)
	}
}

func TestResolve_KeychainUnavailableIsReported(t *testing.T) {
	keyring.MockInitWithError(errors.New("no D-Bus session"))
	t.Cleanup(keyring.MockInit)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", migrationEmail)

	_, err := Resolve("https://gw.example.com")
	if err == nil || errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v, want a keychain error distinct from not logged in", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "keychain unavailable: ") || !strings.Contains(msg, "no D-Bus session") || !strings.Contains(msg, "GATEWAY_API_KEY") {
		t.Fatalf("message = %q", msg)
	}
}

func TestResolve_CorruptLegacySessionIsNotMigrated(t *testing.T) {
	setupLegacySession(t, "https://gw.example.com")
	if err := keyring.Set("relay-cli", "oauth-refresh-token:"+migrationEmail, "{not json"); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve("https://gw.example.com")
	if err == nil || strings.Contains(err.Error(), "keychain unavailable") || !strings.Contains(err.Error(), "relay login") {
		t.Fatalf("err = %v, want an unreadable-session error pointing at relay login", err)
	}
	if _, err := keychain.ReadToken("https://gw.example.com", migrationEmail); err == nil {
		t.Fatal("a corrupt legacy session must not be bound to the gateway")
	}
}
