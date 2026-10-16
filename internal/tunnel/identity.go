package tunnel

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// IdentityFile is the name of the identity inside the state directory.
const IdentityFile = "identity.json"

// KeyOverlap is how long a rotated-out key still opens requests.
const KeyOverlap = 24 * time.Hour

// Identity is what enrollment gave the agent: who it is and the keys it holds.
type Identity struct {
	AgentID   string `json:"agent_id"`
	NetworkID string `json:"network_id"`
	OrgID     string `json:"org_id"`
	OrgSlug   string `json:"org_slug"`
	Name      string `json:"name"`
	BrokerURL string `json:"broker_url"`
	AppURL    string `json:"app_url"`
	// Keys holds the current key first, then keys that were rotated out.
	Keys        []AgentKey      `json:"-"`
	GatewayJWKS json.RawMessage `json:"gateway_jwks"`
	JWKSFetched time.Time       `json:"jwks_fetched_at"`
	// KeyValidity is how long a key lives; rotation starts a third before.
	KeyValidity time.Duration `json:"key_validity"`
}

type keyFile struct {
	SignPEM   string     `json:"sign_pem"`
	EncPEM    string     `json:"enc_pem"`
	CreatedAt time.Time  `json:"created_at"`
	NotAfter  time.Time  `json:"not_after"`
	RetiredAt *time.Time `json:"retired_at,omitempty"`
}

type identityFile struct {
	Version int `json:"version"`
	Identity
	KeyFiles []keyFile `json:"keys"`
}

// Current is the key the agent signs with and receives on.
func (id *Identity) Current() AgentKey { return id.Keys[0] }

// ActiveKeys are the keys that may still open a request: the current one and
// any retired less than KeyOverlap ago.
func (id *Identity) ActiveKeys(now time.Time) []AgentKey {
	var out []AgentKey
	for _, k := range id.Keys {
		if k.RetiredAt == nil || now.Sub(*k.RetiredAt) < KeyOverlap {
			out = append(out, k)
		}
	}
	return out
}

// NeedsRotation reports a key with less than a third of its life left.
func (id *Identity) NeedsRotation(now time.Time) bool {
	k := id.Current()
	life := k.NotAfter.Sub(k.CreatedAt)
	return life > 0 && k.NotAfter.Sub(now) <= life/3
}

// Rotate puts a new key first and retires the one it replaces.
func (id *Identity) Rotate(next AgentKey, now time.Time) {
	retired := id.Keys[0]
	retired.RetiredAt = &now
	var kept []AgentKey
	for _, k := range id.Keys[1:] {
		if k.RetiredAt != nil && now.Sub(*k.RetiredAt) < KeyOverlap {
			kept = append(kept, k)
		}
	}
	id.Keys = append([]AgentKey{next, retired}, kept...)
}

func encodePEM(blockType string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}))
}

func (k AgentKey) file() (keyFile, error) {
	sd, err := x509.MarshalPKCS8PrivateKey(k.SignPriv)
	if err != nil {
		return keyFile{}, err
	}
	ep, err := ecdh.X25519().NewPrivateKey(k.EncPriv)
	if err != nil {
		return keyFile{}, err
	}
	ed, err := x509.MarshalPKCS8PrivateKey(ep)
	if err != nil {
		return keyFile{}, err
	}
	return keyFile{SignPEM: encodePEM("PRIVATE KEY", sd), EncPEM: encodePEM("PRIVATE KEY", ed), CreatedAt: k.CreatedAt, NotAfter: k.NotAfter, RetiredAt: k.RetiredAt}, nil
}

func (f keyFile) key() (AgentKey, error) {
	sb, _ := pem.Decode([]byte(f.SignPEM))
	eb, _ := pem.Decode([]byte(f.EncPEM))
	if sb == nil || eb == nil {
		return AgentKey{}, errors.New("a key in the identity file is not PEM")
	}
	sk, err := x509.ParsePKCS8PrivateKey(sb.Bytes)
	if err != nil {
		return AgentKey{}, err
	}
	sign, ok := sk.(ed25519.PrivateKey)
	if !ok {
		return AgentKey{}, errors.New("the signing key is not Ed25519")
	}
	ek, err := x509.ParsePKCS8PrivateKey(eb.Bytes)
	if err != nil {
		return AgentKey{}, err
	}
	enc, ok := ek.(*ecdh.PrivateKey)
	if !ok || enc.Curve() != ecdh.X25519() {
		return AgentKey{}, errors.New("the encryption key is not X25519")
	}
	return AgentKey{SignPriv: sign, EncPriv: enc.Bytes(), CreatedAt: f.CreatedAt, NotAfter: f.NotAfter, RetiredAt: f.RetiredAt}, nil
}

// Save writes identity.json, readable by its owner only.
func (id *Identity) Save(stateDir string) error {
	f := identityFile{Version: 1, Identity: *id}
	for _, k := range id.Keys {
		kf, err := k.file()
		if err != nil {
			return err
		}
		f.KeyFiles = append(f.KeyFiles, kf)
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, ".identity-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(stateDir, IdentityFile))
}

// ErrNotEnrolled means the state directory has no identity.
var ErrNotEnrolled = errors.New("this agent is not enrolled")

// LoadIdentity reads identity.json and refuses one that others can read.
func LoadIdentity(stateDir string) (*Identity, error) {
	p := filepath.Join(stateDir, IdentityFile)
	info, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s does not exist; run `%s enroll --token nxt_...`", ErrNotEnrolled, p, BinaryName)
	}
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s can be read by others (mode %o); run chmod 600 on it", p, info.Mode().Perm())
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var f identityFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s is not an identity file: %w", p, err)
	}
	if f.Version != 1 || len(f.KeyFiles) == 0 || f.AgentID == "" {
		return nil, fmt.Errorf("%s is not a complete identity file", p)
	}
	id := f.Identity
	for _, kf := range f.KeyFiles {
		k, err := kf.key()
		if err != nil {
			return nil, err
		}
		id.Keys = append(id.Keys, k)
	}
	return &id, nil
}

// GatewayKeys parses the cached gateway key set.
func (id *Identity) GatewayKeys() (GatewayKeys, error) { return ParseJWKS(id.GatewayJWKS) }

// jwt builds a compact EdDSA token.
func jwt(key AgentKey, claims map[string]any) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": key.KID()})
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(body)
	return signing + "." + enc.EncodeToString(ed25519.Sign(key.SignPriv, []byte(signing))), nil
}

// MintJWT makes a 60-second token signed with the agent's identity key. iss is
// the agent id, aud the party it is for.
func MintJWT(key AgentKey, agentID, audience, networkID string, now time.Time) (string, error) {
	jti := make([]byte, 12)
	_, _ = rand.Read(jti)
	return jwt(key, map[string]any{
		"iss": agentID, "aud": audience, "net": networkID,
		"iat": now.Unix(), "exp": now.Add(60 * time.Second).Unix(), "jti": base64.RawURLEncoding.EncodeToString(jti),
	})
}
