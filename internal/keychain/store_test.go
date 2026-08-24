package keychain_test

import (
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/relay/internal/keychain"
)

// init installs a mock keyring so tests don't touch the real OS keychain.
func init() {
	keyring.MockInit()
}

func TestWriteAndReadToken(t *testing.T) {
	email := "test@example.com"
	data := keychain.TokenData{
		AccessToken:  "access-123",
		RefreshToken: "refresh-456",
		Email:        email,
		ExpiresIn:    3600,
	}

	if err := keychain.WriteToken(email, data); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}

	got, err := keychain.ReadToken(email)
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}

	if got.AccessToken != data.AccessToken {
		t.Errorf("AccessToken: got %q, want %q", got.AccessToken, data.AccessToken)
	}
	if got.RefreshToken != data.RefreshToken {
		t.Errorf("RefreshToken: got %q, want %q", got.RefreshToken, data.RefreshToken)
	}
	if got.Email != data.Email {
		t.Errorf("Email: got %q, want %q", got.Email, data.Email)
	}
	if got.ExpiresIn != data.ExpiresIn {
		t.Errorf("ExpiresIn: got %d, want %d", got.ExpiresIn, data.ExpiresIn)
	}
}

func TestReadToken_NotFound(t *testing.T) {
	_, err := keychain.ReadToken("nonexistent@example.com")
	if err == nil {
		t.Error("expected error for non-existent token, got nil")
	}
}

func TestDeleteToken(t *testing.T) {
	email := "delete@example.com"
	data := keychain.TokenData{
		AccessToken:  "to-delete",
		RefreshToken: "rf",
		Email:        email,
	}

	if err := keychain.WriteToken(email, data); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}

	if err := keychain.DeleteToken(email); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}

	_, err := keychain.ReadToken(email)
	if err == nil {
		t.Error("expected error after delete, got nil")
	}
}

func TestDeleteToken_Idempotent(t *testing.T) {
	// Deleting a non-existent token should return nil (idempotent).
	if err := keychain.DeleteToken("nobody@example.com"); err != nil {
		t.Errorf("DeleteToken on non-existent should return nil, got: %v", err)
	}
}

func TestWriteToken_OverwritesExisting(t *testing.T) {
	email := "overwrite@example.com"

	first := keychain.TokenData{AccessToken: "first-token", Email: email}
	if err := keychain.WriteToken(email, first); err != nil {
		t.Fatalf("WriteToken (first): %v", err)
	}

	second := keychain.TokenData{AccessToken: "second-token", Email: email}
	if err := keychain.WriteToken(email, second); err != nil {
		t.Fatalf("WriteToken (second): %v", err)
	}

	got, err := keychain.ReadToken(email)
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	if got.AccessToken != "second-token" {
		t.Errorf("expected overwritten token %q, got %q", "second-token", got.AccessToken)
	}
}

// TestReadToken_CorruptData verifies that malformed JSON returns an unmarshal error.
func TestReadToken_CorruptData(t *testing.T) {
	const svcName = "relay-cli"
	email := "corrupt@example.com"
	account := "oauth-refresh-token:" + email

	// Write raw invalid JSON directly using the mock keyring.
	if err := keyring.Set(svcName, account, "not-json{{{"); err != nil {
		t.Fatalf("mock keyring Set: %v", err)
	}

	_, err := keychain.ReadToken(email)
	if err == nil {
		t.Error("expected error for corrupt JSON, got nil")
	}
	if !strings.Contains(err.Error(), "unmarshal token") {
		t.Errorf("expected unmarshal error, got: %v", err)
	}
}
