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

const testOrigin = "https://gw-a.example.com"

func TestWriteAndReadToken(t *testing.T) {
	email := "test@example.com"
	data := keychain.TokenData{
		AccessToken:  "access-123",
		RefreshToken: "refresh-456",
		ExpiresIn:    3600,
	}

	if err := keychain.WriteToken(testOrigin, email, data); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}

	got, err := keychain.ReadToken(testOrigin, email)
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}

	if got.AccessToken != data.AccessToken {
		t.Errorf("AccessToken: got %q, want %q", got.AccessToken, data.AccessToken)
	}
	if got.RefreshToken != data.RefreshToken {
		t.Errorf("RefreshToken: got %q, want %q", got.RefreshToken, data.RefreshToken)
	}
	if got.Email != email {
		t.Errorf("Email: got %q, want %q", got.Email, email)
	}
	if got.GatewayOrigin != testOrigin {
		t.Errorf("GatewayOrigin: got %q, want %q", got.GatewayOrigin, testOrigin)
	}
	if got.ExpiresIn != data.ExpiresIn {
		t.Errorf("ExpiresIn: got %d, want %d", got.ExpiresIn, data.ExpiresIn)
	}
}

func TestWriteToken_RequiresGatewayOrigin(t *testing.T) {
	if err := keychain.WriteToken("", "someone@example.com", keychain.TokenData{}); err == nil {
		t.Error("expected an error when gatewayOrigin is empty, got nil")
	}
}

func TestWriteToken_RequiresEmail(t *testing.T) {
	if err := keychain.WriteToken(testOrigin, "", keychain.TokenData{}); err == nil {
		t.Error("expected an error when email is empty, got nil")
	}
}

func TestReadToken_NotFound(t *testing.T) {
	_, err := keychain.ReadToken(testOrigin, "nonexistent@example.com")
	if err == nil {
		t.Error("expected error for non-existent token, got nil")
	}
}

func TestReadToken_DoesNotCrossGatewayOrigins(t *testing.T) {
	const email = "shared@example.com"
	const otherOrigin = "https://gw-b.example.com"

	if err := keychain.WriteToken(testOrigin, email, keychain.TokenData{AccessToken: "gw-a-access"}); err != nil {
		t.Fatalf("WriteToken (gw-a): %v", err)
	}

	// No session written for otherOrigin — reading it must fail, not fall
	// back to the gw-a session for the same email.
	if _, err := keychain.ReadToken(otherOrigin, email); err == nil {
		t.Error("expected ReadToken for an unrelated gateway origin to fail, got nil")
	}
}

func TestWriteToken_SameEmailDifferentGatewaysCoexist(t *testing.T) {
	const email = "multi@example.com"
	const otherOrigin = "https://gw-b.example.com"

	if err := keychain.WriteToken(testOrigin, email, keychain.TokenData{AccessToken: "gw-a-access"}); err != nil {
		t.Fatalf("WriteToken (gw-a): %v", err)
	}
	if err := keychain.WriteToken(otherOrigin, email, keychain.TokenData{AccessToken: "gw-b-access"}); err != nil {
		t.Fatalf("WriteToken (gw-b): %v", err)
	}

	a, err := keychain.ReadToken(testOrigin, email)
	if err != nil {
		t.Fatalf("ReadToken (gw-a): %v", err)
	}
	b, err := keychain.ReadToken(otherOrigin, email)
	if err != nil {
		t.Fatalf("ReadToken (gw-b): %v", err)
	}
	if a.AccessToken != "gw-a-access" || b.AccessToken != "gw-b-access" {
		t.Errorf("sessions did not coexist independently: gw-a=%q gw-b=%q", a.AccessToken, b.AccessToken)
	}
}

func TestDeleteToken(t *testing.T) {
	email := "delete@example.com"
	data := keychain.TokenData{AccessToken: "to-delete", RefreshToken: "rf"}

	if err := keychain.WriteToken(testOrigin, email, data); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}

	if err := keychain.DeleteToken(testOrigin, email); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}

	_, err := keychain.ReadToken(testOrigin, email)
	if err == nil {
		t.Error("expected error after delete, got nil")
	}
}

func TestDeleteToken_Idempotent(t *testing.T) {
	// Deleting a non-existent token should return nil (idempotent).
	if err := keychain.DeleteToken(testOrigin, "nobody@example.com"); err != nil {
		t.Errorf("DeleteToken on non-existent should return nil, got: %v", err)
	}
}

func TestWriteToken_OverwritesExisting(t *testing.T) {
	email := "overwrite@example.com"

	first := keychain.TokenData{AccessToken: "first-token"}
	if err := keychain.WriteToken(testOrigin, email, first); err != nil {
		t.Fatalf("WriteToken (first): %v", err)
	}

	second := keychain.TokenData{AccessToken: "second-token"}
	if err := keychain.WriteToken(testOrigin, email, second); err != nil {
		t.Fatalf("WriteToken (second): %v", err)
	}

	got, err := keychain.ReadToken(testOrigin, email)
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
	account := "oauth-refresh-token:" + testOrigin + ":" + email

	// Write raw invalid JSON directly using the mock keyring.
	if err := keyring.Set(svcName, account, "not-json{{{"); err != nil {
		t.Fatalf("mock keyring Set: %v", err)
	}

	_, err := keychain.ReadToken(testOrigin, email)
	if err == nil {
		t.Error("expected error for corrupt JSON, got nil")
	}
	if !strings.Contains(err.Error(), "unmarshal token") {
		t.Errorf("expected unmarshal error, got: %v", err)
	}
}

// ─── Legacy (pre-gateway-identity) sessions ─────────────────────────────────

func TestHasLegacyToken(t *testing.T) {
	const svcName = "relay-cli"
	email := "legacy@example.com"

	if keychain.HasLegacyToken(email) {
		t.Fatal("expected no legacy token before writing one")
	}

	if err := keyring.Set(svcName, "oauth-refresh-token:"+email, `{"access_token":"legacy-access"}`); err != nil {
		t.Fatalf("mock keyring Set: %v", err)
	}

	if !keychain.HasLegacyToken(email) {
		t.Error("expected HasLegacyToken to find the legacy entry")
	}
}

func TestDeleteLegacyToken(t *testing.T) {
	const svcName = "relay-cli"
	email := "legacy-delete@example.com"

	if err := keyring.Set(svcName, "oauth-refresh-token:"+email, `{"access_token":"legacy-access"}`); err != nil {
		t.Fatalf("mock keyring Set: %v", err)
	}
	if err := keychain.DeleteLegacyToken(email); err != nil {
		t.Fatalf("DeleteLegacyToken: %v", err)
	}
	if keychain.HasLegacyToken(email) {
		t.Error("expected legacy token to be gone after DeleteLegacyToken")
	}
}

func TestDeleteLegacyToken_Idempotent(t *testing.T) {
	if err := keychain.DeleteLegacyToken("nobody-legacy@example.com"); err != nil {
		t.Errorf("DeleteLegacyToken on non-existent should return nil, got: %v", err)
	}
}
