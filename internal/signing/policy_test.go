package signing

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func verdictHeader() string {
	return base64.StdEncoding.EncodeToString([]byte(`{"sast":"pass"}`))
}

func TestCheckRefusesMalformedOrMisattributedSignatures(t *testing.T) {
	priv, key := fixtureKey()
	art := Artifact{SHA256: ContentSHA256([]byte("x")), ID: "r", Version: "1", Verdict: verdictHeader()}
	payload := gatewayPayload(art.SHA256, "r", `{"sast":"pass"}`, "1")
	good := func() http.Header { return signedHeaders(priv, key.KID, payload, art.Verdict) }

	badKey := key
	badKey.PublicKey = "not base64!"
	shortKey := key
	shortKey.PublicKey = base64.StdEncoding.EncodeToString([]byte("short"))

	cases := map[string]struct {
		keys   []Key
		mutate func(http.Header)
		art    Artifact
		want   string
	}{
		"no key id":        {[]Key{key}, func(h http.Header) { h.Del(HeaderKeyID) }, art, "names no key"},
		"nothing pinned":   {nil, func(http.Header) {}, art, "no signing key is pinned"},
		"unpinned key id":  {[]Key{key}, func(h http.Header) { h.Set(HeaderKeyID, "other") }, art, "not pinned"},
		"undecodable key":  {[]Key{badKey}, func(http.Header) {}, art, "not a valid ed25519 public key"},
		"wrong size key":   {[]Key{shortKey}, func(http.Header) {}, art, "not a valid ed25519 public key"},
		"signature base64": {[]Key{key}, func(h http.Header) { h.Set(HeaderSignature, "***") }, art, "malformed signature"},
		"signature size":   {[]Key{key}, func(h http.Header) { h.Set(HeaderSignature, base64.StdEncoding.EncodeToString([]byte("tiny"))) }, art, "malformed signature"},
		"verdict not b64":  {[]Key{key}, func(http.Header) {}, Artifact{SHA256: art.SHA256, ID: "r", Version: "1", Verdict: "***"}, "not base64"},
		"verdict not json": {[]Key{key}, func(http.Header) {}, Artifact{SHA256: art.SHA256, ID: "r", Version: "1", Verdict: base64.StdEncoding.EncodeToString([]byte("nope"))}, "not base64 JSON"},
		"content changed":  {[]Key{key}, func(http.Header) {}, Artifact{SHA256: ContentSHA256([]byte("y")), ID: "r", Version: "1", Verdict: art.Verdict}, "does not match"},
		"version changed":  {[]Key{key}, func(http.Header) {}, Artifact{SHA256: art.SHA256, ID: "r", Version: "2", Verdict: art.Verdict}, "does not match"},
	}
	for name, c := range cases {
		h := good()
		c.mutate(h)
		signed, err := Policy{Keys: c.keys}.Check(h, c.art)
		if err == nil || signed || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: signed=%v err=%v, want error containing %q", name, signed, err, c.want)
		}
	}

	if signed, err := (Policy{Keys: []Key{key}}).Check(good(), art); err != nil || !signed {
		t.Errorf("control: signed=%v err=%v", signed, err)
	}
}

func TestRequireRefusesUnsignedWithTheSentinel(t *testing.T) {
	_, err := Policy{Require: true}.Check(http.Header{}, Artifact{})
	if err == nil || !strings.Contains(err.Error(), "--require-signature") {
		t.Fatalf("err = %v", err)
	}
}

func TestFingerprintIsStableAndFlagsBadKeys(t *testing.T) {
	_, key := fixtureKey()
	fp := Fingerprint(key)
	if !strings.HasPrefix(fp, "SHA256:") || fp != Fingerprint(key) {
		t.Errorf("fingerprint = %q", fp)
	}
	other := key
	other.PublicKey = base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	if Fingerprint(other) == fp {
		t.Error("different keys share a fingerprint")
	}
	if got := Fingerprint(Key{PublicKey: "%%%"}); got != "invalid" {
		t.Errorf("undecodable key fingerprint = %q", got)
	}
}

func TestSameComparesKeySetsIgnoringOrder(t *testing.T) {
	_, a := fixtureKey()
	b := Key{KID: "2027-01", Alg: "ed25519", PublicKey: a.PublicKey}
	cases := []struct {
		x, y []Key
		want bool
	}{
		{[]Key{a, b}, []Key{b, a}, true},
		{[]Key{a}, []Key{a, b}, false},
		{[]Key{a}, []Key{b}, false},
		{nil, nil, true},
	}
	for i, c := range cases {
		if got := Same(c.x, c.y); got != c.want {
			t.Errorf("case %d: Same = %v, want %v", i, got, c.want)
		}
	}
}

func TestVerifyMessage(t *testing.T) {
	priv, key := fixtureKey()
	msg := []byte("payload")
	sig := ed25519.Sign(priv, msg)

	if err := VerifyMessage([]Key{key}, key.KID, msg, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	badKey := key
	badKey.PublicKey = "%%%"
	cases := map[string]struct {
		keys []Key
		kid  string
		msg  []byte
		sig  []byte
		want string
	}{
		"nothing pinned": {nil, key.KID, msg, sig, "no signing key is pinned"},
		"unpinned id":    {[]Key{key}, "rotated", msg, sig, "not pinned"},
		"bad key":        {[]Key{badKey}, key.KID, msg, sig, "not a valid ed25519"},
		"tampered":       {[]Key{key}, key.KID, []byte("other"), sig, "does not match"},
		"short sig":      {[]Key{key}, key.KID, msg, sig[:10], "does not match"},
	}
	for name, c := range cases {
		if err := VerifyMessage(c.keys, c.kid, c.msg, c.sig); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestCanonicalJSONSortsKeysAndRejectsUnknownTypes(t *testing.T) {
	got, err := CanonicalJSON(map[string]any{"b": []any{1.5, "<&>", nil, true}, "a": map[string]any{"z": 1, "y": false}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":{"y":false,"z":1},"b":[1.5,"<&>",null,true]}`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if _, err := CanonicalJSON(make(chan int)); err == nil {
		t.Error("an unserialisable value was accepted")
	}
}

func TestFetchKeysRejectsBadDocuments(t *testing.T) {
	_, good := fixtureKey()
	dup := `{"keys":[{"kid":"a","alg":"ed25519","publicKey":"` + good.PublicKey + `"},{"kid":"a","alg":"ed25519","publicKey":"` + good.PublicKey + `"}]}`
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"not found":    {http.StatusNotFound, "", ErrNoKeys.Error()},
		"server error": {http.StatusInternalServerError, "", "HTTP 500"},
		"invalid json": {http.StatusOK, "{", "fetch signing keys"},
		"empty":        {http.StatusOK, `{"keys":[]}`, ErrNoKeys.Error()},
		"no id":        {http.StatusOK, `{"keys":[{"alg":"ed25519","publicKey":"` + good.PublicKey + `"}]}`, "no id"},
		"duplicate id": {http.StatusOK, dup, "share one"},
		"other alg":    {http.StatusOK, `{"keys":[{"kid":"a","alg":"rsa","publicKey":"` + good.PublicKey + `"}]}`, "unsupported algorithm"},
		"bad key":      {http.StatusOK, `{"keys":[{"kid":"a","alg":"ed25519","publicKey":"AAAA"}]}`, "not a valid ed25519"},
		"too large":    {http.StatusOK, `{"keys":[],"pad":"` + strings.Repeat("a", maxKeysBody) + `"}`, "too large"},
	}
	for name, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != KeysPath || r.Header.Get("Authorization") != "" {
				t.Errorf("%s: unexpected request %s auth=%q", name, r.URL.Path, r.Header.Get("Authorization"))
			}
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		_, err := FetchKeys(t.Context(), srv.URL+"/")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestFetchKeysRefusesRedirectsAndUnreachableGateways(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	if _, err := FetchKeys(t.Context(), srv.URL); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("redirect err = %v", err)
	}
	srv.Close()
	if _, err := FetchKeys(t.Context(), srv.URL); err == nil {
		t.Error("an unreachable gateway returned keys")
	}
	if _, err := FetchKeys(t.Context(), "http://bad host"); err == nil {
		t.Error("an invalid origin was accepted")
	}
}
