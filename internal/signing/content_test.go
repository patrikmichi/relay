package signing_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/patrikmichi/zuano/internal/signing"
	"github.com/patrikmichi/zuano/internal/signing/signingtest"
)

func TestVerifyContent(t *testing.T) {
	s := signingtest.New("k1")
	other := signingtest.New("k2")
	body := []byte(`{"b":1,"a":[true,"x"]}`)
	signed := func(sg *signingtest.Signer, b []byte) http.Header {
		h := http.Header{}
		sg.Sign(h, b)
		return h
	}
	cases := map[string]struct {
		keys []signing.Key
		h    http.Header
		body []byte
		ok   bool
	}{
		"valid, key order does not matter": {[]signing.Key{s.Key}, signed(s, body), []byte(`{"a":[true,"x"],"b":1}`), true},
		"tampered body":                    {[]signing.Key{s.Key}, signed(s, body), []byte(`{"a":[true,"x"],"b":2}`), false},
		"unsigned":                         {[]signing.Key{s.Key}, http.Header{}, body, false},
		"unpinned key":                     {[]signing.Key{other.Key}, signed(s, body), body, false},
		"no keys":                          {nil, signed(s, body), body, false},
		"not base64":                       {[]signing.Key{s.Key}, http.Header{signing.HeaderContentSignature: {"!!"}, signing.HeaderContentKeyID: {"k1"}}, body, false},
		"not json":                         {[]signing.Key{s.Key}, signed(s, body), []byte(`nope`), false},
	}
	for name, c := range cases {
		err := signing.VerifyContent(c.keys, c.h, c.body)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", name, err)
		}
		var ce *signing.ContentError
		if err != nil && (!errors.As(err, &ce) || ce.ExitCode() != 4 || ce.Error() == "") {
			t.Errorf("%s: not a content error with exit 4: %v", name, err)
		}
	}
}
