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

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/signing"
)

const pluginVerdictJSON = `{"ai_scan":"pass","sast":"pass","schema":"pass","secrets":"pass","skillspector":"pass","static_scan":"na"}`

func keyFromSeed(b byte) (ed25519.PrivateKey, signing.Key) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, ed25519.SeedSize))
	return priv, signing.Key{KID: "k" + string(rune('0'+b)), Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))}
}

// signedMarketplace serves testManifest with plugin bundles signed the way the
// gateway signs them; tamper changes a bundle after signing.
func signedMarketplace(t *testing.T, priv ed25519.PrivateKey, kid string, sign, tamper bool) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/marketplace/manifest":
			_, _ = w.Write(marshalJSON(t, testManifest))
		case strings.HasPrefix(r.URL.Path, "/api/marketplace/plugin/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/marketplace/plugin/"), "/")
			id, rev := parts[0], parts[1]
			body := marshalJSON(t, map[string]any{"files": map[string]string{".mcp.json": `{"mcpServers":{}}`}})
			if sign {
				payload := `{"id":"` + id + `","sha256":"` + signing.ContentSHA256(body) + `","verdict":` + pluginVerdictJSON + `,"version":"` + rev + `"}`
				w.Header().Set(signing.HeaderSignature, base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload))))
				w.Header().Set(signing.HeaderKeyID, kid)
				w.Header().Set("X-Skill-Catalog-Id", id)
				w.Header().Set("X-Skill-Version", rev)
				w.Header().Set("X-Skill-Scan-Verdict", base64.StdEncoding.EncodeToString([]byte(pluginVerdictJSON)))
			}
			if tamper {
				body = marshalJSON(t, map[string]any{"files": map[string]string{".mcp.json": `{"mcpServers":{"evil":{"command":"sh"}}}`}})
			}
			_, _ = w.Write(body)
		case r.URL.Path == "/api/marketplace/managed-settings":
			queries = append(queries, r.URL.RawQuery)
			_, _ = w.Write([]byte(`{"enabledPlugins":["searcher-001"]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

func TestMarketplaceSync_VerifiesSignedPluginsAndShowsTheFragment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	priv, key := keyFromSeed(1)
	srv, queries := signedMarketplace(t, priv, key.KID, true, false)

	var out strings.Builder
	v := &syncVerify{policy: signing.Policy{Keys: []signing.Key{key}, Require: true}, warn: &out}
	if err := runSyncVerified(&testDoer{srv: srv}, t.TempDir(), false, v, &out); err != nil {
		t.Fatalf("signed sync: %v", err)
	}
	if !strings.Contains(out.String(), `{"enabledPlugins":["searcher-001"]}`) || !strings.Contains(out.String(), "unsigned") {
		t.Errorf("fragment was not shown before the guidance:\n%s", out.String())
	}
	for _, q := range *queries {
		if q != "" {
			t.Errorf("managed-settings request carried a query: %q", q)
		}
	}
}

func TestMarketplaceSync_RefusesTamperedAndUnsignedPlugins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	priv, key := keyFromSeed(1)
	policy := signing.Policy{Keys: []signing.Key{key}}

	tampered, _ := signedMarketplace(t, priv, key.KID, true, true)
	err := runSyncVerified(&testDoer{srv: tampered}, t.TempDir(), false, &syncVerify{policy: policy}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("tampered bundle = %v, want a signature mismatch", err)
	}

	unsigned, _ := signedMarketplace(t, priv, key.KID, false, false)
	required := signing.Policy{Keys: []signing.Key{key}, Require: true}
	err = runSyncVerified(&testDoer{srv: unsigned}, t.TempDir(), false, &syncVerify{policy: required}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "--require-signature") {
		t.Errorf("unsigned with --require-signature = %v", err)
	}

	var warn strings.Builder
	if err := runSyncVerified(&testDoer{srv: unsigned}, t.TempDir(), false, &syncVerify{policy: policy, warn: &warn}, &strings.Builder{}); err != nil {
		t.Errorf("unsigned without --require-signature: %v", err)
	}
	if !strings.Contains(warn.String(), "did not sign") {
		t.Errorf("no unsigned warning: %q", warn.String())
	}
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
	t.Setenv("RELAY_AGENT", "")
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

	t.Setenv("RELAY_AGENT", "1")
	if _, err := runCommand(t, configTrustKeyCmd(), "--gateway-url", srv.URL, "--yes"); err == nil || !strings.Contains(err.Error(), "RELAY_AGENT") {
		t.Errorf("trust-key in agent mode = %v", err)
	}
}
