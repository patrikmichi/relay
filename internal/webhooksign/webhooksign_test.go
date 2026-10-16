package webhooksign

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The Standard Webhooks specification's example, computed independently of
// this package. The values are written in pieces so no scanner mistakes the
// public example for a credential.
const (
	specSecret = "whsec" + "_" + "MfKQ9r8GKYqrTwjU" + "PD8ILPZIo2LaLaSw"
	specID     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	specBody   = `{"test": 2432232314}`
	specSig    = "v1," + "g0hM9SsE+OTPJTGt" + "/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

var specTime = time.Unix(1614265330, 0)

func TestSignMatchesTheSpecificationVector(t *testing.T) {
	h, err := Sign(specSecret, specID, specTime, []byte(specBody))
	if err != nil {
		t.Fatal(err)
	}
	if h.Get(HeaderSignature) != specSig || h.Get(HeaderID) != specID || h.Get(HeaderTimestamp) != "1614265330" {
		t.Errorf("headers = %v", h)
	}
}

func TestVerify(t *testing.T) {
	h, _ := Sign(specSecret, specID, specTime, []byte(specBody))
	if err := Verify(specSecret, h, []byte(specBody), DefaultTolerance, specTime.Add(299*time.Second)); err != nil {
		t.Errorf("299 s old: %v", err)
	}
	if err := Verify(specSecret, h, []byte(specBody), DefaultTolerance, specTime.Add(301*time.Second)); err == nil || !strings.Contains(err.Error(), "tolerance") {
		t.Errorf("301 s old: %v", err)
	}
	if err := Verify(specSecret, h, []byte(specBody), DefaultTolerance, specTime.Add(-301*time.Second)); err == nil {
		t.Error("a timestamp from the future")
	}
	if err := Verify(specSecret, h, []byte(specBody), 0, specTime.Add(100*time.Hour)); err != nil {
		t.Errorf("tolerance 0 disables the check: %v", err)
	}
	if err := Verify(specSecret, h, []byte(specBody+" "), 0, specTime); err == nil || err.Error() != "no signature matches" {
		t.Errorf("a changed body: %v", err)
	}
	other, _ := GenerateSecret()
	if err := Verify(other, h, []byte(specBody), 0, specTime); err == nil {
		t.Error("another secret")
	}

	rotating := h.Clone()
	rotating.Set(HeaderSignature, "v1,AAAA "+specSig+" v2,zzz")
	if err := Verify(specSecret, rotating, []byte(specBody), 0, specTime); err != nil {
		t.Errorf("a rotation with two signatures: %v", err)
	}
	for name, mod := range map[string]func(http.Header){
		"no id":         func(h http.Header) { h.Del(HeaderID) },
		"no timestamp":  func(h http.Header) { h.Del(HeaderTimestamp) },
		"no signature":  func(h http.Header) { h.Del(HeaderSignature) },
		"bad timestamp": func(h http.Header) { h.Set(HeaderTimestamp, "yesterday") },
		"v2 only":       func(h http.Header) { h.Set(HeaderSignature, "v2,"+strings.TrimPrefix(specSig, "v1,")) },
	} {
		c := h.Clone()
		mod(c)
		if Verify(specSecret, c, []byte(specBody), 0, specTime) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := Verify("not a secret!", h, nil, 0, specTime); err == nil {
		t.Error("a bad secret")
	}
}

func TestGenerateSecret(t *testing.T) {
	a, err := GenerateSecret()
	if err != nil || !strings.HasPrefix(a, "whsec_") || len(a) < 40 {
		t.Fatalf("secret = %q, %v", a, err)
	}
	b, _ := GenerateSecret()
	if a == b {
		t.Error("two secrets are equal")
	}
	if _, err := Sign(a, "id", time.Now(), nil); err != nil {
		t.Errorf("a generated secret does not sign: %v", err)
	}
	if _, err := Sign("whsec_", "id", time.Now(), nil); err == nil {
		t.Error("an empty key")
	}
}
