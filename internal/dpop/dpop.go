// Package dpop binds a CLI session to a device key with DPoP proofs (RFC
// 9449). The key signs a short proof for each request, so a stolen access or
// refresh token is useless without the device that holds the key. The key
// lives in the machine's TPM where there is one, and otherwise in a file that
// only the owner can read.
package dpop

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// Key is a P-256 device key whose private half does the signing.
type Key interface {
	// Public is the public half, sent in each proof.
	Public() *ecdsa.PublicKey
	// Sign signs a SHA-256 digest and returns the 64-byte r||s signature a JWS
	// uses.
	Sign(digest []byte) ([]byte, error)
	// Backend names where the private key lives: "file" or "tpm".
	Backend() string
	// Hardware is true when the private key cannot be exported.
	Hardware() bool
	// Ref identifies the key for Load and Delete.
	Ref() string
}

// ErrNoHardwareKey is returned when a hardware-backed key is required and this
// machine offers none.
var ErrNoHardwareKey = errors.New("this machine has no hardware-backed key (TPM 2.0), which the organisation requires")

var b64 = base64.RawURLEncoding

type jwk struct {
	Crv string `json:"crv"`
	Kty string `json:"kty"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func jwkOf(pub *ecdsa.PublicKey) (jwk, error) {
	point, err := pub.Bytes()
	if err != nil || len(point) != 65 {
		return jwk{}, errors.New("dpop: the device key is not a P-256 public key")
	}
	return jwk{Crv: "P-256", Kty: "EC", X: b64.EncodeToString(point[1:33]), Y: b64.EncodeToString(point[33:65])}, nil
}

// Thumbprint is the RFC 7638 JWK thumbprint of pub, the "jkt" the gateway
// binds a token to.
func Thumbprint(pub *ecdsa.PublicKey) (string, error) {
	key, err := jwkOf(pub)
	if err != nil {
		return "", err
	}
	// The members in lexicographic order, with no whitespace: json.Marshal of
	// the struct above writes them in that order.
	raw, err := json.Marshal(key)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return b64.EncodeToString(sum[:]), nil
}

type proofHeader struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
	JWK jwk    `json:"jwk"`
}

type proofClaims struct {
	JTI string `json:"jti"`
	HTM string `json:"htm"`
	HTU string `json:"htu"`
	IAT int64  `json:"iat"`
	ATH string `json:"ath,omitempty"`
}

// htu is the request URL without its query and fragment, as the proof names it.
func htu(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("dpop: %q is not an absolute URL", rawURL)
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.EscapedPath(), nil
}

// Proof builds the DPoP proof JWT for one request. accessToken is empty for a
// token-endpoint request, which has no token to hash yet.
func Proof(k Key, method, rawURL, accessToken string, now time.Time, jti string) (string, error) {
	target, err := htu(rawURL)
	if err != nil {
		return "", err
	}
	claims := proofClaims{JTI: jti, HTM: strings.ToUpper(method), HTU: target, IAT: now.Unix()}
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		claims.ATH = b64.EncodeToString(sum[:])
	}
	key, err := jwkOf(k.Public())
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(proofHeader{Typ: "dpop+jwt", Alg: "ES256", JWK: key})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64.EncodeToString(header) + "." + b64.EncodeToString(body)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := k.Sign(digest[:])
	if err != nil {
		return "", fmt.Errorf("dpop: sign the proof: %w", err)
	}
	return signingInput + "." + b64.EncodeToString(sig), nil
}

// Signer makes proofs with a device key; it is what the gateway client calls
// for each request.
type Signer struct {
	Key Key
	// Now and NewID are for tests.
	Now   func() time.Time
	NewID func() string
}

// NewSigner returns a Signer that uses the current time and random ids.
func NewSigner(k Key) *Signer { return &Signer{Key: k} }

// Proof implements the gateway client's proof signer.
func (s *Signer) Proof(method, rawURL, accessToken string) (string, error) {
	now, id := time.Now, randomID
	if s.Now != nil {
		now = s.Now
	}
	if s.NewID != nil {
		id = s.NewID
	}
	return Proof(s.Key, method, rawURL, accessToken, now(), id())
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// rawSignature turns an ASN.1 DER ECDSA signature into the fixed-width r||s a
// JWS carries.
func rawSignature(der []byte) ([]byte, error) {
	var sig struct{ R, S *big.Int }
	if rest, err := asn1.Unmarshal(der, &sig); err != nil || len(rest) != 0 {
		return nil, errors.New("dpop: malformed ECDSA signature")
	}
	out := make([]byte, 64)
	if sig.R.BitLen() > 256 || sig.S.BitLen() > 256 || sig.R.Sign() <= 0 || sig.S.Sign() <= 0 {
		return nil, errors.New("dpop: ECDSA signature out of range")
	}
	sig.R.FillBytes(out[:32])
	sig.S.FillBytes(out[32:])
	return out, nil
}
