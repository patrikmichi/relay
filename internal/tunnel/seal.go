package tunnel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Chunk kinds bind each sealed chunk to its role, so a header cannot be
// replayed as a body chunk and a stream cannot be cut short unnoticed: only
// the last chunk is sealed as final.
const (
	kindHeader byte = 'H'
	kindData   byte = 'D'
	kindFinal  byte = 'F'
)

// Errors the exchange maps to ERROR codes.
var (
	ErrDecrypt   = errors.New("decrypt_failed")
	ErrSignature = errors.New("signature_invalid")
	ErrReplay    = errors.New("replay")
	ErrExpired   = errors.New("expired")
)

var (
	hpkeKEM  = hpke.DHKEM(ecdh.X25519())
	hpkeKDF  = hpke.HKDFSHA256()
	hpkeAEAD = hpke.AES256GCM()
)

// KeyID names a signing key: the first 8 bytes of its SHA-256, in hex.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// AgentKey is one identity key pair of the agent: Ed25519 for signatures and
// X25519 for the keys requests are sealed to.
type AgentKey struct {
	SignPriv  ed25519.PrivateKey `json:"-"`
	EncPriv   []byte             `json:"-"`
	CreatedAt time.Time          `json:"created_at"`
	NotAfter  time.Time          `json:"not_after"`
	// RetiredAt is set when a rotation replaced the key; it still opens
	// requests sealed to it until the gateway stops sealing to it.
	RetiredAt *time.Time `json:"retired_at,omitempty"`
}

// GenerateKey makes a fresh key pair valid for the given time.
func GenerateKey(now time.Time, valid time.Duration) (AgentKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return AgentKey{}, err
	}
	enc, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return AgentKey{}, err
	}
	return AgentKey{SignPriv: priv, EncPriv: enc.Bytes(), CreatedAt: now, NotAfter: now.Add(valid)}, nil
}

// SignPub is the public signing key.
func (k AgentKey) SignPub() ed25519.PublicKey { return k.SignPriv.Public().(ed25519.PublicKey) }

// KID is the key's id.
func (k AgentKey) KID() string { return KeyID(k.SignPub()) }

// EncPub is the public encryption key.
func (k AgentKey) EncPub() []byte {
	priv, err := ecdh.X25519().NewPrivateKey(k.EncPriv)
	if err != nil {
		return nil
	}
	return priv.PublicKey().Bytes()
}

// GatewayKeys are the gateway's request-signing keys by kid.
type GatewayKeys map[string]ed25519.PublicKey

// ParseJWKS reads a JWK set of Ed25519 keys.
func ParseJWKS(data []byte) (GatewayKeys, error) {
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			Kid string `json:"kid"`
			X   string `json:"x"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("the gateway key set is not JSON: %w", err)
	}
	out := GatewayKeys{}
	for _, k := range doc.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" {
			continue
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("gateway key %q is not an Ed25519 key", k.Kid)
		}
		out[k.Kid] = ed25519.PublicKey(x)
	}
	if len(out) == 0 {
		return nil, errors.New("the gateway key set has no Ed25519 key")
	}
	return out, nil
}

// Routing is the part of a request the broker may read.
type Routing struct {
	RequestID  string `json:"request_id"`
	Target     string `json:"target"`
	Kind       string `json:"kind"`
	DeadlineMs int64  `json:"deadline_ms"`
}

// WrappedKey is the content key sealed to one agent key with HPKE.
type WrappedKey struct {
	KID string `json:"kid"`
	Enc []byte `json:"enc"`
	CT  []byte `json:"ct"`
}

// OpenEnvelope is the payload of an OPEN frame.
type OpenEnvelope struct {
	Routing      json.RawMessage `json:"routing"`
	Keys         json.RawMessage `json:"keys"`
	GatewayKID   string          `json:"gateway_kid"`
	SealedHeader []byte          `json:"sealed_header"`
	HeaderSig    []byte          `json:"header_sig"`
}

// FileRef names a file the agent fetches itself.
type FileRef struct {
	Param  string `json:"param"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Spill is where the agent may stream a large response instead of sending it
// through the tunnel.
type Spill struct {
	UploadURL string `json:"upload_url"`
	FileID    string `json:"file_id"`
	MaxBytes  int64  `json:"max_bytes"`
	ExpiresAt int64  `json:"expires_at_ms"`
}

// RequestHeader is the sealed header of a request.
type RequestHeader struct {
	V                int                 `json:"v"`
	RequestID        string              `json:"request_id"`
	OrgID            string              `json:"org_id"`
	NetworkID        string              `json:"network_id"`
	Target           string              `json:"target"`
	Kind             string              `json:"kind"`
	Method           string              `json:"method"`
	Path             string              `json:"path"`
	Query            string              `json:"query,omitempty"`
	Headers          map[string][]string `json:"headers,omitempty"`
	CredentialsMode  string              `json:"credentials_mode"`
	MaxResponseBytes int64               `json:"max_response_bytes"`
	DeadlineUnixMs   int64               `json:"deadline_unix_ms"`
	IssuedAtMs       int64               `json:"issued_at_ms"`
	Nonce            []byte              `json:"nonce"`
	IdempotencyKey   string              `json:"idempotency_key,omitempty"`
	TraceID          string              `json:"trace_id,omitempty"`
	AuditRef         string              `json:"audit_ref,omitempty"`
	RespPub          []byte              `json:"resp_pub"`
	BodySHA256       string              `json:"body_sha256,omitempty"`
	FileRefs         []FileRef           `json:"file_refs,omitempty"`
	Spill            *Spill              `json:"spill,omitempty"`
}

// RequestTrailer is the sealed last chunk of a request stream.
type RequestTrailer struct {
	Sig []byte `json:"sig"`
}

// ResponseHeader is the sealed first chunk of a response.
type ResponseHeader struct {
	RequestID string              `json:"request_id"`
	Status    int                 `json:"status"`
	Headers   map[string][]string `json:"headers,omitempty"`
	Spilled   *SpilledFile        `json:"spilled,omitempty"`
}

// SpilledFile says a large response was streamed to the file store.
type SpilledFile struct {
	FileID      string `json:"file_id"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type,omitempty"`
}

// ResponseTrailer is the sealed last chunk of a response stream.
type ResponseTrailer struct {
	AgentKID   string `json:"agent_kid"`
	Sig        []byte `json:"sig"`
	UpstreamMs int64  `json:"upstream_ms"`
	AgentMs    int64  `json:"agent_ms"`
	Bytes      int64  `json:"bytes"`
}

// streamCipher is the STREAM construction over AES-256-GCM: the nonce is a
// 4-byte prefix and an 8-byte chunk counter, and the chunk kind is the
// associated data.
type streamCipher struct {
	aead   cipher.AEAD
	prefix [4]byte
	ctr    uint64
	roll   [32]byte
}

func newStreamCipher(cek []byte, label string) (*streamCipher, error) {
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &streamCipher{aead: aead, roll: sha256.Sum256([]byte("ctun.v1 stream"))}
	sum := sha256.Sum256(append(append([]byte(nil), cek...), label...))
	copy(s.prefix[:], sum[:4])
	return s, nil
}

func (s *streamCipher) nonce() []byte {
	n := make([]byte, 12)
	copy(n, s.prefix[:])
	binary.BigEndian.PutUint64(n[4:], s.ctr)
	return n
}

func (s *streamCipher) seal(kind byte, pt []byte) []byte {
	ct := s.aead.Seal(nil, s.nonce(), pt, []byte{kind})
	s.advance(kind, ct)
	return ct
}

func (s *streamCipher) open(kind byte, ct []byte) ([]byte, error) {
	pt, err := s.aead.Open(nil, s.nonce(), ct, []byte{kind})
	if err != nil {
		return nil, ErrDecrypt
	}
	s.advance(kind, ct)
	return pt, nil
}

func (s *streamCipher) advance(kind byte, ct []byte) {
	s.ctr++
	if kind == kindData {
		h := sha256.New()
		h.Write(s.roll[:])
		h.Write(ct)
		h.Sum(s.roll[:0])
	}
}

// requestDigest binds the routing header, the wrapped keys and the sealed
// header together, each with its length.
func requestDigest(routing, keys, sealed []byte) [32]byte {
	h := sha256.New()
	for _, part := range [][]byte{routing, keys, sealed} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

func headerSigMessage(d [32]byte) []byte { return digestOf("ctun.v1 header", d[:]) }

func endSigMessage(d [32]byte, roll [32]byte) []byte { return digestOf("ctun.v1 end", d[:], roll[:]) }

func digestOf(label string, parts ...[]byte) []byte {
	h := sha256.New()
	h.Write([]byte(label))
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func hpkeInfo(label, requestID string) []byte { return []byte("ctun.v1 " + label + " " + requestID) }

// Request is an OPEN frame that was unwrapped and checked.
type Request struct {
	Header     RequestHeader
	Routing    Routing
	GatewayKID string
	digest     [32]byte
	stream     *streamCipher
	gateway    ed25519.PublicKey
}

// OpenRequest unwraps the content key with one of the agent's keys, checks the
// gateway's signature over the header and opens the header. It does not check
// freshness or replay; see Guard.
func OpenRequest(env OpenEnvelope, keys []AgentKey, gw GatewayKeys) (*Request, error) {
	var routing Routing
	if err := json.Unmarshal(env.Routing, &routing); err != nil {
		return nil, fmt.Errorf("%w: routing header", ErrDecrypt)
	}
	var wrapped []WrappedKey
	if err := json.Unmarshal(env.Keys, &wrapped); err != nil {
		return nil, fmt.Errorf("%w: wrapped keys", ErrDecrypt)
	}
	pub, ok := gw[env.GatewayKID]
	if !ok {
		return nil, fmt.Errorf("%w: unknown gateway key %q", ErrSignature, env.GatewayKID)
	}
	d := requestDigest(env.Routing, env.Keys, env.SealedHeader)
	if !ed25519.Verify(pub, headerSigMessage(d), env.HeaderSig) {
		return nil, ErrSignature
	}
	var cek []byte
	for _, k := range keys {
		for _, w := range wrapped {
			if w.KID != k.KID() {
				continue
			}
			priv, err := hpkeKEM.NewPrivateKey(k.EncPriv)
			if err != nil {
				continue
			}
			rcpt, err := hpke.NewRecipient(w.Enc, priv, hpkeKDF, hpkeAEAD, hpkeInfo("request key", routing.RequestID))
			if err != nil {
				continue
			}
			if cek, err = rcpt.Open(nil, w.CT); err == nil {
				break
			}
			cek = nil
		}
		if cek != nil {
			break
		}
	}
	if cek == nil {
		return nil, fmt.Errorf("%w: no key of this agent opens the request", ErrDecrypt)
	}
	stream, err := newStreamCipher(cek, "request")
	if err != nil {
		return nil, ErrDecrypt
	}
	raw, err := stream.open(kindHeader, env.SealedHeader)
	if err != nil {
		return nil, err
	}
	var h RequestHeader
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("%w: request header", ErrDecrypt)
	}
	if h.RequestID != routing.RequestID || h.Target != routing.Target || h.Kind != routing.Kind {
		return nil, fmt.Errorf("%w: the routing header does not match the sealed one", ErrSignature)
	}
	return &Request{Header: h, Routing: routing, GatewayKID: env.GatewayKID, digest: d, stream: stream, gateway: pub}, nil
}

// Chunk opens the next body chunk.
func (r *Request) Chunk(ct []byte) ([]byte, error) { return r.stream.open(kindData, ct) }

// Final opens the last chunk and checks the gateway's signature over the whole
// stream, so a body that was cut, reordered or altered is refused.
func (r *Request) Final(ct []byte) error {
	raw, err := r.stream.open(kindFinal, ct)
	if err != nil {
		return err
	}
	var t RequestTrailer
	if err := json.Unmarshal(raw, &t); err != nil {
		return ErrDecrypt
	}
	if !ed25519.Verify(r.gateway, endSigMessage(r.digest, r.stream.roll), t.Sig) {
		return ErrSignature
	}
	return nil
}

// ResponseSealer seals a response to the gateway's ephemeral key and signs it
// with the agent's identity key.
type ResponseSealer struct {
	stream    *streamCipher
	enc       []byte
	requestID string
	key       AgentKey
	headerCT  []byte
}

// NewResponseSealer derives the response key with HPKE to resp_pub.
func NewResponseSealer(respPub []byte, requestID string, key AgentKey) (*ResponseSealer, error) {
	pub, err := hpkeKEM.NewPublicKey(respPub)
	if err != nil {
		return nil, fmt.Errorf("%w: response key", ErrDecrypt)
	}
	enc, sender, err := hpke.NewSender(pub, hpkeKDF, hpkeAEAD, hpkeInfo("response", requestID))
	if err != nil {
		return nil, err
	}
	cek, err := sender.Export("ctun.v1 response key", 32)
	if err != nil {
		return nil, err
	}
	stream, err := newStreamCipher(cek, "response")
	if err != nil {
		return nil, err
	}
	return &ResponseSealer{stream: stream, enc: enc, requestID: requestID, key: key}, nil
}

// Header returns the payload of the DATA frame that opens the response.
func (r *ResponseSealer) Header(h ResponseHeader) ([]byte, error) {
	h.RequestID = r.requestID
	raw, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	r.headerCT = r.stream.seal(kindHeader, raw)
	return append(append([]byte(nil), r.enc...), r.headerCT...), nil
}

// Chunk seals one body chunk.
func (r *ResponseSealer) Chunk(p []byte) []byte { return r.stream.seal(kindData, p) }

func responseSigMessage(requestID string, enc, headerCT []byte, roll [32]byte) []byte {
	return digestOf("ctun.v1 response", []byte(requestID), enc, headerCT, roll[:])
}

// End seals the trailer with the agent's signature over the response.
func (r *ResponseSealer) End(t ResponseTrailer) ([]byte, error) {
	t.AgentKID = r.key.KID()
	t.Sig = ed25519.Sign(r.key.SignPriv, responseSigMessage(r.requestID, r.enc, r.headerCT, r.stream.roll))
	raw, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	return r.stream.seal(kindFinal, raw), nil
}

// Guard refuses requests that are stale, repeated or meant for someone else.
type Guard struct {
	OrgID, NetworkID string
	Window           time.Duration
	Memory           time.Duration
	// Offset is the gateway's clock minus the agent's, from HELLO_ACK.
	Offset time.Duration
	seen   map[string]time.Time
}

// NewGuard starts a guard for one identity.
func NewGuard(orgID, networkID string) *Guard {
	return &Guard{OrgID: orgID, NetworkID: networkID, Window: 60 * time.Second, Memory: 5 * time.Minute, seen: map[string]time.Time{}}
}

// Check validates freshness, identity and uniqueness of a request header.
func (g *Guard) Check(h RequestHeader, now time.Time) error {
	if h.V != ProtoVersion {
		return fmt.Errorf("%w: header version %d", ErrDecrypt, h.V)
	}
	if h.OrgID != g.OrgID || h.NetworkID != g.NetworkID {
		return fmt.Errorf("%w: the request is for another organisation or network", ErrSignature)
	}
	issued := time.UnixMilli(h.IssuedAtMs)
	if d := now.Add(g.Offset).Sub(issued); d > g.Window || d < -g.Window {
		return ErrExpired
	}
	if len(h.Nonce) != 16 {
		return fmt.Errorf("%w: nonce", ErrDecrypt)
	}
	for k, t := range g.seen {
		if now.Sub(t) > g.Memory {
			delete(g.seen, k)
		}
	}
	nk, rk := "n:"+hex.EncodeToString(h.Nonce), "r:"+h.RequestID
	if _, dup := g.seen[nk]; dup {
		return ErrReplay
	}
	if _, dup := g.seen[rk]; dup {
		return ErrReplay
	}
	g.seen[nk], g.seen[rk] = now, now
	return nil
}
