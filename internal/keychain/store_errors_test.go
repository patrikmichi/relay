package keychain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/relay/internal/keychain"
)

func withBrokenKeychain(t *testing.T) {
	t.Helper()
	keyring.MockInitWithError(errors.New("no D-Bus session"))
	t.Cleanup(keyring.MockInit)
}

func TestWriteToken_KeychainFailureNamesSessionAndHint(t *testing.T) {
	withBrokenKeychain(t)
	err := keychain.WriteToken(testOrigin, "x@example.com", keychain.TokenData{AccessToken: "a"})
	if err == nil {
		t.Fatal("expected a keychain failure to surface")
	}
	for _, want := range []string{"x@example.com@" + testOrigin, "no D-Bus session", "gnome-keyring"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestReadToken_RequiresGatewayOrigin(t *testing.T) {
	if _, err := keychain.ReadToken("", "x@example.com"); err == nil {
		t.Fatal("expected an error without a gateway identity")
	}
}

func TestDeleteToken_KeychainFailureIsReported(t *testing.T) {
	withBrokenKeychain(t)
	if err := keychain.DeleteToken(testOrigin, "x@example.com"); err == nil {
		t.Fatal("expected a keychain failure, not a silent success")
	}
}

func TestDeleteLegacyToken_KeychainFailureIsReported(t *testing.T) {
	withBrokenKeychain(t)
	if err := keychain.DeleteLegacyToken("x@example.com"); err == nil {
		t.Fatal("expected a keychain failure, not a silent success")
	}
}

func TestReadLegacyToken_KeychainFailureIsUnavailableError(t *testing.T) {
	withBrokenKeychain(t)
	_, err := keychain.ReadLegacyToken("x@example.com")
	var unavailable *keychain.UnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(unavailable.Error(), "keychain unavailable") {
		t.Fatalf("err = %v, want *UnavailableError", err)
	}
}
