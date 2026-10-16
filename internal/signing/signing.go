// Package signing verifies the ed25519 signatures a gateway puts on skill,
// agent and plugin downloads. The wire contract is in
// docs/reference/signed-downloads.md.
package signing

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/patrikmichi/zuano/internal/config"
)

// Wire names of the contract.
const (
	maxKeysBody = 1 << 20

	HeaderSignature = "X-Skill-Signature"
	HeaderKeyID     = "X-Skill-Signing-Key-Id"
	KeysPath        = "/.well-known/relay-signing-keys"
)

// Key is one signing public key.
type Key = config.SigningKey

// ErrNoKeys means the gateway publishes no signing keys, so it does not sign.
var ErrNoKeys = errors.New("the gateway publishes no signing keys")

// ErrUnsigned means a download carried no signature.
var ErrUnsigned = errors.New("the download is not signed")

// Artifact is what a signature covers.
type Artifact struct {
	// SHA256 is the lowercase hex sha256 of the downloaded bytes, computed
	// locally, never taken from a header.
	SHA256  string
	ID      string
	Version string
	// Verdict is the X-Skill-Scan-Verdict header as sent (base64 JSON).
	Verdict string
}

// ContentSHA256 returns the hex sha256 of content.
func ContentSHA256(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum)
}

// Fingerprint is a short, comparable name for a key: the unpadded base64 of
// the sha256 of its public key bytes.
func Fingerprint(k Key) string {
	raw, err := base64.StdEncoding.DecodeString(k.PublicKey)
	if err != nil {
		return "invalid"
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// Policy decides what a download must carry.
type Policy struct {
	// Keys are the keys pinned for the gateway.
	Keys []Key
	// Require refuses an unsigned download.
	Require bool
}

// Check verifies the signature headers of a response against a. signed is
// false, with no error, for an unsigned download when signatures are not
// required; a signature that is present but wrong is always an error.
func (p Policy) Check(h http.Header, a Artifact) (signed bool, err error) {
	sig := strings.TrimSpace(h.Get(HeaderSignature))
	if sig == "" {
		if p.Require {
			return false, fmt.Errorf("%w and --require-signature is set", ErrUnsigned)
		}
		return false, nil
	}
	kid := strings.TrimSpace(h.Get(HeaderKeyID))
	if kid == "" {
		return false, fmt.Errorf("the download is signed but names no key (%s)", HeaderKeyID)
	}
	if len(p.Keys) == 0 {
		return false, errors.New("the download is signed but no signing key is pinned for this gateway; run `relay login`, or `relay config trust-key` to pin its keys")
	}
	idx := slices.IndexFunc(p.Keys, func(k Key) bool { return k.KID == kid })
	if idx < 0 {
		return false, fmt.Errorf("the download is signed with key %q, which is not pinned for this gateway; if the gateway rotated its keys, review them with `relay config trust-key`", kid)
	}
	pub, err := decodePublicKey(p.Keys[idx])
	if err != nil {
		return false, err
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return false, errors.New("the download carries a malformed signature")
	}
	msg, err := Message(a)
	if err != nil {
		return false, err
	}
	if !ed25519.Verify(pub, msg, raw) {
		return false, errors.New("the download's signature does not match its content, id, version and scan verdict")
	}
	return true, nil
}

func decodePublicKey(k Key) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(k.PublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pinned key %q is not a valid ed25519 public key", k.KID)
	}
	return ed25519.PublicKey(raw), nil
}

// FetchKeys reads the gateway's published signing keys. The request carries no
// credentials.
func FetchKeys(ctx context.Context, origin string) ([]Key, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(origin, "/")+KeysPath, nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("refusing to follow a redirect") },
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch signing keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoKeys
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch signing keys: HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Keys []Key `json:"keys"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKeysBody+1))
	if err == nil && len(body) > maxKeysBody {
		err = errors.New("response is too large")
	}
	if err == nil {
		err = json.Unmarshal(body, &doc)
	}
	if err != nil {
		return nil, fmt.Errorf("fetch signing keys: %w", err)
	}
	if len(doc.Keys) == 0 {
		return nil, ErrNoKeys
	}
	seen := map[string]bool{}
	for _, k := range doc.Keys {
		if k.KID == "" || seen[k.KID] {
			return nil, errors.New("fetch signing keys: a key has no id, or two keys share one")
		}
		seen[k.KID] = true
		if k.Alg != "ed25519" {
			return nil, fmt.Errorf("fetch signing keys: key %q uses unsupported algorithm %q", k.KID, k.Alg)
		}
		if _, err := decodePublicKey(k); err != nil {
			return nil, fmt.Errorf("fetch signing keys: %w", err)
		}
	}
	return doc.Keys, nil
}

// Same reports whether two key sets hold exactly the same keys.
func Same(a, b []Key) bool {
	if len(a) != len(b) {
		return false
	}
	for _, k := range a {
		if !slices.ContainsFunc(b, func(o Key) bool { return o == k }) {
			return false
		}
	}
	return true
}
