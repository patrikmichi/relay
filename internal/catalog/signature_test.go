package catalog

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/signing"
)

// The verdict is the one in the gateway's signing reference; its header form
// is base64 of JSON with keys in arbitrary order.
const (
	gatewayVerdictJSON   = `{"ai_scan":"pass","duplicate":"pass","permissions":"pass","sast":"pass","schema":"pass","secrets":"pass","skillspector":"pass","static_scan":"na"}`
	shuffledVerdictJSON  = `{"static_scan":"na","skillspector":"pass","schema":"pass","secrets":"pass","sast":"pass","permissions":"pass","duplicate":"pass","ai_scan":"pass"}`
	signedFixtureID      = "res_1"
	signedFixtureVersion = "1.4.2"
)

func signingFixture() (ed25519.PrivateKey, signing.Key) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	return priv, signing.Key{KID: "2026-10", Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))}
}

// signedGateway serves bundle exactly as the gateway signs it: ed25519 over
// {"id","sha256","verdict":{decoded},"version"} with sorted keys.
func signedGateway(t *testing.T, bundle []byte, priv ed25519.PrivateKey, kid string) Doer {
	t.Helper()
	payload := `{"id":"` + signedFixtureID + `","sha256":"` + sha256HexOf(bundle) + `","verdict":` + gatewayVerdictJSON + `,"version":"` + signedFixtureVersion + `"}`
	return newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set(headerContentSha256, sha256HexOf(bundle))
		h.Set(headerScanVerdict, base64.StdEncoding.EncodeToString([]byte(shuffledVerdictJSON)))
		h.Set(headerVersion, signedFixtureVersion)
		h.Set(headerCatalogID, signedFixtureID)
		h.Set(signing.HeaderSignature, base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload))))
		h.Set(signing.HeaderKeyID, kid)
		_, _ = w.Write(bundle)
	})
}

func TestFetchSkill_AcceptsAGatewaySignedBundle(t *testing.T) {
	priv, key := signingFixture()
	doer := signedGateway(t, simpleBundle(t), priv, key.KID)
	var warn bytes.Buffer

	skill, err := FetchSkill(doer, signedFixtureID, signedFixtureVersion, "", WithSigning(signing.Policy{Keys: []signing.Key{key}, Require: true}, &warn))
	if err != nil || skill == nil {
		t.Fatalf("FetchSkill = %v", err)
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warning: %s", warn.String())
	}
}

func TestFetchSkill_RejectsSignatureProblems(t *testing.T) {
	priv, key := signingFixture()
	otherPriv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	policy := signing.Policy{Keys: []signing.Key{key}}

	cases := map[string]struct {
		doer    Doer
		version string
		pol     signing.Policy
		want    string
	}{
		"signed by another key": {signedGateway(t, simpleBundle(t), otherPriv, key.KID), signedFixtureVersion, policy, "does not match"},
		"unpinned key id":       {signedGateway(t, simpleBundle(t), priv, "rotated"), signedFixtureVersion, policy, "trust-key"},
		"no pinned keys":        {signedGateway(t, simpleBundle(t), priv, key.KID), signedFixtureVersion, signing.Policy{}, "no signing key is pinned"},
		"older signed release":  {signedGateway(t, simpleBundle(t), priv, key.KID), "1.4.3", policy, "not the requested"},
	}
	for name, tc := range cases {
		_, err := FetchSkill(tc.doer, signedFixtureID, tc.version, "", WithSigning(tc.pol, nil))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestFetchSkill_UnsignedWarnsUnlessRequired(t *testing.T) {
	bundle := simpleBundle(t)
	doer := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, "passed")
		w.Header().Set(headerVersion, "1.0.0")
		_, _ = w.Write(bundle)
	})
	var warn bytes.Buffer
	if _, err := FetchSkill(doer, "res_abc", "", "", WithSigning(signing.Policy{}, &warn)); err != nil {
		t.Fatalf("unsigned download refused without --require-signature: %v", err)
	}
	if !strings.Contains(warn.String(), "did not sign") {
		t.Errorf("no warning: %q", warn.String())
	}
	if _, err := FetchSkill(doer, "res_abc", "", "", WithSigning(signing.Policy{Require: true}, nil)); err == nil || !strings.Contains(err.Error(), "--require-signature") {
		t.Errorf("required but unsigned = %v", err)
	}
}
