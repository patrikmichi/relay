// Package signingtest signs documents the way a gateway does, for tests of
// code that verifies them.
package signingtest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/patrikmichi/zuano/internal/signing"
)

// Signer holds a private key and the pinned form of its public key.
type Signer struct {
	Key  signing.Key
	priv ed25519.PrivateKey
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// New returns a signer whose key id is kid.
func New(kid string) *Signer {
	pub, priv := must2(ed25519.GenerateKey(rand.Reader))
	return &Signer{Key: signing.Key{KID: kid, Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(pub)}, priv: priv}
}

func must2[A, B any](a A, b B, err error) (A, B) {
	if err != nil {
		panic(err)
	}
	return a, b
}

// Sign sets the content signature headers for body on h.
func (s *Signer) Sign(h http.Header, body []byte) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	must(0, dec.Decode(&doc))
	msg := must(signing.CanonicalJSON(doc))
	h.Set(signing.HeaderContentSignature, base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, msg)))
	h.Set(signing.HeaderContentKeyID, s.Key.KID)
}
