package auth

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/keychain"
)

func TestWriteToken_Success(t *testing.T) {
	withTempHome(t)

	if err := writeToken(keychain.ProfileHuman, "https://gw.example.com", "user@example.com", "access-1", "refresh-1", 3600); err != nil {
		t.Fatalf("writeToken: %v", err)
	}

	tok, err := keychain.ReadToken("https://gw.example.com", "user@example.com")
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	if tok.AccessToken != "access-1" || tok.RefreshToken != "refresh-1" || tok.Email != "user@example.com" || tok.ExpiresIn != 3600 {
		t.Errorf("unexpected stored token: %+v", tok)
	}

	email, err := config.ResolveEmail()
	if err != nil {
		t.Fatalf("ResolveEmail: %v", err)
	}
	if email != "user@example.com" {
		t.Errorf("persisted email: got %q, want user@example.com", email)
	}
}

func TestWriteToken_PropagatesKeychainError(t *testing.T) {
	withTempHome(t)
	defer keyring.MockInit() // restore a working mock for subsequent tests

	keyring.MockInitWithError(errors.New("mock keychain failure"))

	err := writeToken(keychain.ProfileHuman, "https://gw.example.com", "user@example.com", "access-1", "refresh-1", 3600)
	if err == nil {
		t.Fatal("expected error when the keychain write fails, got nil")
	}
}

func TestWriteToken_RemovesUnboundSession(t *testing.T) {
	withTempHome(t)
	if err := keyring.Set("zuano-cli", "oauth-refresh-token:user@example.com", `{"access_token":"old"}`); err != nil {
		t.Fatal(err)
	}
	if err := writeToken(keychain.ProfileHuman, "https://gw.example.com", "user@example.com", "access-1", "refresh-1", 3600); err != nil {
		t.Fatalf("writeToken: %v", err)
	}
	if keychain.HasUnboundToken("user@example.com") {
		t.Fatal("a device-flow login must remove the email-only session it replaces")
	}
}
