package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/signing"
)

func keyFromSeed(b byte) (ed25519.PrivateKey, signing.Key) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, ed25519.SeedSize))
	return priv, signing.Key{KID: "k" + string(rune('0'+b)), Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))}
}

func keysServer(t *testing.T, keys *[]signing.Key) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != signing.KeysPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": *keys})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoginPinsKeysOnFirstUseAndRefusesChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, first := keyFromSeed(1)
	_, second := keyFromSeed(2)
	keys := []signing.Key{first}
	srv := keysServer(t, &keys)

	var out bytes.Buffer
	pinSigningKeys(context.Background(), srv.URL, &out)
	pinned, _ := config.PinnedSigningKeys(srv.URL)
	if len(pinned) != 1 || pinned[0] != first || !strings.Contains(out.String(), "first use") {
		t.Fatalf("first use did not pin: %v\n%s", pinned, out.String())
	}

	keys = []signing.Key{second}
	out.Reset()
	pinSigningKeys(context.Background(), srv.URL, &out)
	pinned, _ = config.PinnedSigningKeys(srv.URL)
	if pinned[0] != first {
		t.Fatalf("a changed key replaced the pinned one: %v", pinned)
	}
	if !strings.Contains(out.String(), "trust-key") {
		t.Errorf("no trust-key hint:\n%s", out.String())
	}
}

func TestTrustKeyReplacesPinnedKeysOnlyWhenConfirmed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZUANO_AGENT", "")
	_, first := keyFromSeed(1)
	_, second := keyFromSeed(2)
	keys := []signing.Key{second}
	srv := keysServer(t, &keys)
	if err := config.PinSigningKeys(srv.URL, []signing.Key{first}); err != nil {
		t.Fatal(err)
	}

	// No terminal: refuse.
	if _, err := runCommand(t, configTrustKeyCmd(), "--gateway-url", srv.URL); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Errorf("trust-key without a terminal = %v", err)
	}
	if out, err := runWithInput(t, configTrustKeyCmd(), "n\n", "--gateway-url", srv.URL); err == nil || !strings.Contains(out, signing.Fingerprint(second)) {
		t.Errorf("declined trust-key = %v\n%s", err, out)
	}
	if pinned, _ := config.PinnedSigningKeys(srv.URL); pinned[0] != first {
		t.Fatal("a declined trust-key changed the pinned key")
	}
	if out, err := runWithInput(t, configTrustKeyCmd(), "y\n", "--gateway-url", srv.URL); err != nil {
		t.Fatalf("confirmed trust-key: %v\n%s", err, out)
	}
	if pinned, _ := config.PinnedSigningKeys(srv.URL); pinned[0] != second {
		t.Fatal("trust-key did not pin the new key")
	}

	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, configTrustKeyCmd(), "--gateway-url", srv.URL, "--yes"); err == nil || !strings.Contains(err.Error(), "ZUANO_AGENT") {
		t.Errorf("trust-key in agent mode = %v", err)
	}
}
