package keychain_test

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/relay/internal/keychain"
)

func TestReadLegacyToken(t *testing.T) {
	const email = "legacy-read@example.com"
	if _, err := keychain.ReadLegacyToken(email); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("missing legacy entry: err = %v, want ErrNotFound", err)
	}
	if err := keyring.Set("relay-cli", "oauth-refresh-token:"+email, `{"access_token":"a","refresh_token":"r","expires_at":42}`); err != nil {
		t.Fatal(err)
	}
	got, err := keychain.ReadLegacyToken(email)
	if err != nil {
		t.Fatalf("ReadLegacyToken: %v", err)
	}
	if got.AccessToken != "a" || got.RefreshToken != "r" || got.ExpiresAt != 42 {
		t.Fatalf("legacy token = %+v", got)
	}
}

func TestReadLegacyToken_RejectsEmptyAndCorruptEntries(t *testing.T) {
	for name, raw := range map[string]string{"empty": `{"email":"x"}`, "corrupt": "{not json"} {
		email := name + "@example.com"
		if err := keyring.Set("relay-cli", "oauth-refresh-token:"+email, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := keychain.ReadLegacyToken(email); err == nil || errors.Is(err, keychain.ErrNotFound) {
			t.Errorf("%s: err = %v, want a read error", name, err)
		}
	}
}

func TestReadToken_KeychainFailureIsUnavailableError(t *testing.T) {
	keyring.MockInitWithError(errors.New("no D-Bus session"))
	t.Cleanup(keyring.MockInit)
	_, err := keychain.ReadToken(testOrigin, "x@example.com")
	var unavailable *keychain.UnavailableError
	if !errors.As(err, &unavailable) || errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("err = %v, want *UnavailableError", err)
	}
}
