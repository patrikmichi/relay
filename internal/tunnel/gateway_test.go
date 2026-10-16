package tunnel

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"
)

// testGateway plays the gateway: it seals requests to agent keys and opens the
// responses, with the same primitives the agent uses.
type testGateway struct {
	t    *testing.T
	kid  string
	priv ed25519.PrivateKey
	keys GatewayKeys
	// badEndSig signs the stream with a key the agent does not know.
	badEndSig bool
}

func newTestGateway(t *testing.T) *testGateway {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testGateway{t: t, kid: "2026-10", priv: priv, keys: GatewayKeys{"2026-10": pub}}
}

// sealedRequest is what the gateway puts on the wire for one exchange.
type sealedRequest struct {
	env     OpenEnvelope
	chunks  [][]byte
	final   []byte
	respKey *ecdh.PrivateKey
}

func baseHeader(h RequestHeader) RequestHeader {
	if h.V == 0 {
		h.V = 1
	}
	if h.RequestID == "" {
		h.RequestID = "req_" + randHex(6)
	}
	if h.OrgID == "" {
		h.OrgID, h.NetworkID = "org_1", "net_1"
	}
	if h.Kind == "" {
		h.Kind = "http"
	}
	if h.Method == "" {
		h.Method = "GET"
	}
	if h.Path == "" {
		h.Path = "/"
	}
	if h.CredentialsMode == "" {
		h.CredentialsMode = "gateway"
	}
	if h.IssuedAtMs == 0 {
		h.IssuedAtMs = time.Now().UnixMilli()
	}
	if h.DeadlineUnixMs == 0 {
		h.DeadlineUnixMs = time.Now().Add(30 * time.Second).UnixMilli()
	}
	if h.MaxResponseBytes == 0 {
		h.MaxResponseBytes = 5 << 20
	}
	if h.Nonce == nil {
		h.Nonce = randBytes(16)
	}
	return h
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func randHex(n int) string {
	const digits = "0123456789abcdef"
	b := randBytes(n)
	out := make([]byte, 0, 2*n)
	for _, x := range b {
		out = append(out, digits[x>>4], digits[x&15])
	}
	return string(out)
}

// seal builds the OPEN envelope, the body chunks and the final chunk for h,
// wrapping the content key to every key in agents.
func (g *testGateway) seal(h RequestHeader, body [][]byte, agents ...AgentKey) *sealedRequest {
	g.t.Helper()
	h = baseHeader(h)
	respKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		g.t.Fatal(err)
	}
	h.RespPub = respKey.PublicKey().Bytes()
	cek := randBytes(32)
	var wrapped []WrappedKey
	for _, a := range agents {
		pub, err := hpkeKEM.NewPublicKey(a.EncPub())
		if err != nil {
			g.t.Fatal(err)
		}
		enc, sender, err := hpke.NewSender(pub, hpkeKDF, hpkeAEAD, hpkeInfo("request key", h.RequestID))
		if err != nil {
			g.t.Fatal(err)
		}
		ct, err := sender.Seal(nil, cek)
		if err != nil {
			g.t.Fatal(err)
		}
		wrapped = append(wrapped, WrappedKey{KID: a.KID(), Enc: enc, CT: ct})
	}
	routing, _ := json.Marshal(Routing{RequestID: h.RequestID, Target: h.Target, Kind: h.Kind, DeadlineMs: h.DeadlineUnixMs})
	keysRaw, _ := json.Marshal(wrapped)
	stream, err := newStreamCipher(cek, "request")
	if err != nil {
		g.t.Fatal(err)
	}
	raw, _ := json.Marshal(h)
	sealedHeader := stream.seal(kindHeader, raw)
	d := requestDigest(routing, keysRaw, sealedHeader)
	out := &sealedRequest{respKey: respKey, env: OpenEnvelope{
		Routing: routing, Keys: keysRaw, GatewayKID: g.kid, SealedHeader: sealedHeader,
		HeaderSig: ed25519.Sign(g.priv, headerSigMessage(d)),
	}}
	for _, c := range body {
		out.chunks = append(out.chunks, stream.seal(kindData, c))
	}
	endKey := g.priv
	if g.badEndSig {
		_, endKey, _ = ed25519.GenerateKey(rand.Reader)
	}
	trailer, _ := json.Marshal(RequestTrailer{Sig: ed25519.Sign(endKey, endSigMessage(d, stream.roll))})
	out.final = stream.seal(kindFinal, trailer)
	return out
}

// openedResponse is a response the gateway decrypted and verified.
type openedResponse struct {
	Header  ResponseHeader
	Body    []byte
	Trailer ResponseTrailer
}

// openResponse reads a response frame stream: the header payload, the DATA
// chunks and the END payload.
func (g *testGateway) openResponse(req *sealedRequest, agent AgentKey, header []byte, chunks [][]byte, end []byte) (*openedResponse, error) {
	g.t.Helper()
	if len(header) < 32 {
		return nil, ErrDecrypt
	}
	enc, headerCT := header[:32], header[32:]
	priv, err := hpkeKEM.NewPrivateKey(req.respKey.Bytes())
	if err != nil {
		return nil, err
	}
	var routing Routing
	_ = json.Unmarshal(req.env.Routing, &routing)
	rcpt, err := hpke.NewRecipient(enc, priv, hpkeKDF, hpkeAEAD, hpkeInfo("response", routing.RequestID))
	if err != nil {
		return nil, err
	}
	cek, err := rcpt.Export("ctun.v1 response key", 32)
	if err != nil {
		return nil, err
	}
	stream, err := newStreamCipher(cek, "response")
	if err != nil {
		return nil, err
	}
	out := &openedResponse{}
	raw, err := stream.open(kindHeader, headerCT)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &out.Header); err != nil {
		return nil, err
	}
	if out.Header.RequestID != routing.RequestID {
		return nil, ErrSignature
	}
	for _, c := range chunks {
		p, err := stream.open(kindData, c)
		if err != nil {
			return nil, err
		}
		out.Body = append(out.Body, p...)
	}
	roll := stream.roll
	raw, err = stream.open(kindFinal, end)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &out.Trailer); err != nil {
		return nil, err
	}
	if !ed25519.Verify(agent.SignPub(), responseSigMessage(routing.RequestID, enc, headerCT, roll), out.Trailer.Sig) {
		return nil, ErrSignature
	}
	return out, nil
}
