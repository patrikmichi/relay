package signing

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gatewayPayload is the string the gateway's signatureHeaders signs, written
// out by hand the way its own test builds it: sorted keys, no whitespace, the
// verdict decoded.
func gatewayPayload(sha, id, verdictJSON, version string) string {
	return `{"id":"` + id + `","sha256":"` + sha + `","verdict":` + verdictJSON + `,"version":"` + version + `"}`
}

func fixtureKey() (ed25519.PrivateKey, Key) {
	priv := ed25519.NewKeyFromSeed([]byte(strings.Repeat("\x07", ed25519.SeedSize)))
	return priv, Key{KID: "2026-10", Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))}
}

func signedHeaders(priv ed25519.PrivateKey, kid, payload, verdictHeader string) http.Header {
	h := http.Header{}
	h.Set(HeaderSignature, base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload))))
	h.Set(HeaderKeyID, kid)
	h.Set("X-Skill-Scan-Verdict", verdictHeader)
	return h
}

func TestVerifiesWhatTheGatewayCanonicalises(t *testing.T) {
	priv, key := fixtureKey()
	body := []byte{1, 2, 3}
	sha := ContentSHA256(body)
	// The header's key order is not canonical on purpose.
	verdictHeader := base64.StdEncoding.EncodeToString([]byte(`{"schema":"pass","sast":"pass"}`))
	payload := gatewayPayload(sha, "res_1", `{"sast":"pass","schema":"pass"}`, "1.4.2")
	h := signedHeaders(priv, key.KID, payload, verdictHeader)

	art := Artifact{SHA256: sha, ID: "res_1", Version: "1.4.2", Verdict: verdictHeader}
	signed, err := Policy{Keys: []Key{key}}.Check(h, art)
	if err != nil || !signed {
		t.Fatalf("gateway-signed download rejected: signed=%v err=%v", signed, err)
	}
	msg, _ := Message(art)
	if string(msg) != payload {
		t.Fatalf("canonical form differs from the gateway's:\n got %s\nwant %s", msg, payload)
	}
}

func TestCanonicalFormMatchesJSStringifyForNestedAndEscapedValues(t *testing.T) {
	verdict := base64.StdEncoding.EncodeToString([]byte(`{"z":{"b":["x<y&z",{"d":1,"c":2}],"a":"é\u2028"},"a":"q\"\n"}`))
	msg, err := Message(Artifact{SHA256: "ab", ID: "i", Version: "v", Verdict: verdict})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"id\":\"i\",\"sha256\":\"ab\",\"verdict\":{\"a\":\"q\\\"\\n\",\"z\":{\"a\":\"é\u2028\",\"b\":[\"x<y&z\",{\"c\":2,\"d\":1}]}},\"version\":\"v\"}"
	if string(msg) != want {
		t.Fatalf("got  %s\nwant %s", msg, want)
	}
}

func TestRejectsTamperedOrUnpinnedDownloads(t *testing.T) {
	priv, key := fixtureKey()
	sha := ContentSHA256([]byte{1, 2, 3})
	verdictHeader := base64.StdEncoding.EncodeToString([]byte(`{"sast":"pass"}`))
	payload := gatewayPayload(sha, "res_1", `{"sast":"pass"}`, "1.0.0")
	h := signedHeaders(priv, key.KID, payload, verdictHeader)
	art := Artifact{SHA256: sha, ID: "res_1", Version: "1.0.0", Verdict: verdictHeader}
	p := Policy{Keys: []Key{key}}

	for name, mutate := range map[string]func(Artifact) Artifact{
		"content": func(a Artifact) Artifact { a.SHA256 = ContentSHA256([]byte{9}); return a },
		"version": func(a Artifact) Artifact { a.Version = "1.0.1"; return a },
		"id":      func(a Artifact) Artifact { a.ID = "res_2"; return a },
		"verdict": func(a Artifact) Artifact {
			a.Verdict = base64.StdEncoding.EncodeToString([]byte(`{"sast":"fail"}`))
			return a
		},
	} {
		if signed, err := p.Check(h, mutate(art)); err == nil || signed {
			t.Errorf("tampered %s accepted", name)
		}
	}

	other := h.Clone()
	other.Set(HeaderKeyID, "rotated")
	if _, err := p.Check(other, art); err == nil || !strings.Contains(err.Error(), "trust-key") {
		t.Errorf("unknown key id = %v, want a trust-key hint", err)
	}
	if _, err := (Policy{}).Check(h, art); err == nil || !strings.Contains(err.Error(), "no signing key is pinned") {
		t.Errorf("no pinned keys = %v", err)
	}
}

func TestUnsignedIsAllowedUnlessRequired(t *testing.T) {
	art := Artifact{SHA256: "x", ID: "i", Version: "v", Verdict: base64.StdEncoding.EncodeToString([]byte(`{}`))}
	if signed, err := (Policy{}).Check(http.Header{}, art); err != nil || signed {
		t.Fatalf("unsigned, not required: signed=%v err=%v", signed, err)
	}
	if _, err := (Policy{Require: true}).Check(http.Header{}, art); err == nil || !strings.Contains(err.Error(), "--require-signature") {
		t.Fatalf("unsigned, required = %v", err)
	}
}

func TestFetchKeys(t *testing.T) {
	_, key := fixtureKey()
	var body string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != KeysPath || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	body = `{"keys":[{"kid":"` + key.KID + `","alg":"ed25519","publicKey":"` + key.PublicKey + `"}]}`
	keys, err := FetchKeys(t.Context(), srv.URL)
	if err != nil || len(keys) != 1 || keys[0] != key {
		t.Fatalf("FetchKeys = %v, %v", keys, err)
	}
	for _, bad := range []string{
		`{"keys":[{"kid":"a","alg":"rsa","publicKey":"` + key.PublicKey + `"}]}`,
		`{"keys":[{"kid":"a","alg":"ed25519","publicKey":"AAAA"}]}`,
		`{"keys":[{"kid":"","alg":"ed25519","publicKey":"` + key.PublicKey + `"}]}`,
	} {
		body = bad
		if _, err := FetchKeys(t.Context(), srv.URL); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	body = `{"keys":[]}`
	if _, err := FetchKeys(t.Context(), srv.URL); err != ErrNoKeys {
		t.Errorf("empty list = %v, want ErrNoKeys", err)
	}
	status = http.StatusNotFound
	if _, err := FetchKeys(t.Context(), srv.URL); err != ErrNoKeys {
		t.Errorf("404 = %v, want ErrNoKeys", err)
	}
}
