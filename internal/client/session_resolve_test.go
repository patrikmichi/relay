package client

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

const resolveEmail = "user@example.com"

func TestResolve_NoSessionIsNotLoggedIn(t *testing.T) {
	keyring.MockInit()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZUANO_API_KEY", "")
	t.Setenv("ZUANO_EMAIL", resolveEmail)
	if _, err := Resolve("https://gw.example.com"); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v, want ErrNotLoggedIn", err)
	}
}

func TestResolve_UnboundEntryIsNeverUsed(t *testing.T) {
	keyring.MockInit()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZUANO_API_KEY", "")
	t.Setenv("ZUANO_EMAIL", resolveEmail)
	if err := keyring.Set("relay-cli", "oauth-refresh-token:"+resolveEmail,
		`{"access_token":"stray-access","refresh_token":"stray-refresh","email":"user@example.com","expires_at":4102444800}`); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve("https://gw.example.com"); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v, want ErrNotLoggedIn: an email-only entry says nothing about which gateway issued it", err)
	}
}

func TestResolve_KeychainUnavailableIsReported(t *testing.T) {
	keyring.MockInitWithError(errors.New("no D-Bus session"))
	t.Cleanup(keyring.MockInit)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZUANO_API_KEY", "")
	t.Setenv("ZUANO_EMAIL", resolveEmail)

	_, err := Resolve("https://gw.example.com")
	if err == nil || errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v, want a keychain error distinct from not logged in", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "keychain unavailable: ") || !strings.Contains(msg, "no D-Bus session") || !strings.Contains(msg, "ZUANO_API_KEY") {
		t.Fatalf("message = %q", msg)
	}
}
