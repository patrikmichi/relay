package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTripAndRefusals(t *testing.T) {
	for typ := range frameNames {
		f := Frame{Type: typ, Flags: 1, StreamID: 0xdeadbeef, Payload: []byte("payload")}
		raw, err := f.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseFrame(raw)
		if err != nil || got.Type != typ || got.Flags != 1 || got.StreamID != 0xdeadbeef || string(got.Payload) != "payload" {
			t.Fatalf("%s round trip = %+v, %v", typ, got, err)
		}
	}
	// The header layout is fixed: ver, type, flags, reserved, stream id, length.
	raw, _ := Frame{Type: FrameOpen, StreamID: 7, Payload: []byte{9}}.Marshal()
	if !bytes.Equal(raw, []byte{1, 3, 0, 0, 0, 0, 0, 7, 0, 0, 0, 1, 9}) {
		t.Errorf("wire bytes = %v", raw)
	}
	if _, err := (Frame{Type: FrameData, Payload: make([]byte, MaxPayload+1)}).Marshal(); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("oversized marshal = %v", err)
	}
	if _, err := (Frame{Type: FrameData, Payload: make([]byte, MaxPayload)}).Marshal(); err != nil {
		t.Errorf("the largest payload was refused: %v", err)
	}
	bad := map[string][]byte{
		"short":         {1, 3, 0},
		"version":       {2, 3, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0},
		"type":          {1, 99, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0},
		"reserved":      {1, 3, 0, 1, 0, 0, 0, 1, 0, 0, 0, 0},
		"length":        {1, 3, 0, 0, 0, 0, 0, 1, 0, 0, 0, 5, 1},
		"trailing":      {1, 3, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 7},
		"huge declared": {1, 3, 0, 0, 0, 0, 0, 1, 0, 4, 0, 1},
	}
	for name, b := range bad {
		if _, err := ParseFrame(b); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if FrameType(77).String() == "" || FrameOpen.String() != "OPEN" {
		t.Error("String")
	}
}

// The vectors are shared with the gateway and the broker: each side must encode
// and decode them byte for byte.
func TestFrameVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "frames.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Valid []struct {
			Name    string `json:"name"`
			Type    uint8  `json:"type"`
			Flags   uint8  `json:"flags"`
			Stream  uint32 `json:"stream"`
			Payload string `json:"payload"`
			Frame   string `json:"frame"`
		} `json:"valid"`
		Invalid []struct {
			Name  string `json:"name"`
			Frame string `json:"frame"`
		} `json:"invalid"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, v := range doc.Valid {
		payload, _ := hex.DecodeString(v.Payload)
		wire, err := Frame{Type: FrameType(v.Type), Flags: v.Flags, StreamID: v.Stream, Payload: payload}.Marshal()
		if err != nil || hex.EncodeToString(wire) != v.Frame {
			t.Errorf("%s: encoded %x (%v), want %s", v.Name, wire, err, v.Frame)
		}
		f, err := ParseFrame(wire)
		if err != nil || uint8(f.Type) != v.Type || f.StreamID != v.Stream || !bytes.Equal(f.Payload, payload) {
			t.Errorf("%s: decoded %+v (%v)", v.Name, f, err)
		}
	}
	for _, v := range doc.Invalid {
		wire, _ := hex.DecodeString(v.Frame)
		if _, err := ParseFrame(wire); err == nil {
			t.Errorf("%s was accepted", v.Name)
		}
	}
	if len(doc.Valid) < 6 || len(doc.Invalid) < 8 {
		t.Error("the vector file is missing cases")
	}
}

func TestSealedRequestRoundTrip(t *testing.T) {
	gw := newTestGateway(t)
	a1, _ := GenerateKey(time.Now(), time.Hour)
	a2, _ := GenerateKey(time.Now(), time.Hour)
	req := gw.seal(RequestHeader{Target: "erp", Path: "/invoices", Query: "a=1", Method: "POST"}, [][]byte{[]byte("hello "), []byte("world")}, a1, a2)

	// Either agent of the network can open it.
	for _, a := range []AgentKey{a1, a2} {
		got, err := OpenRequest(req.env, []AgentKey{a}, gw.keys)
		if err != nil {
			t.Fatal(err)
		}
		if got.Header.Target != "erp" || got.Header.Path != "/invoices" || got.Header.Query != "a=1" || len(got.Header.RespPub) != 32 {
			t.Errorf("header = %+v", got.Header)
		}
	}
	got, _ := OpenRequest(req.env, []AgentKey{a1}, gw.keys)
	var body []byte
	for _, c := range req.chunks {
		p, err := got.Chunk(c)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, p...)
	}
	if string(body) != "hello world" {
		t.Errorf("body = %q", body)
	}
	if err := got.Final(req.final); err != nil {
		t.Errorf("final: %v", err)
	}
}

func TestSealedRequestRefusals(t *testing.T) {
	gw := newTestGateway(t)
	agent, _ := GenerateKey(time.Now(), time.Hour)
	foreign, _ := GenerateKey(time.Now(), time.Hour)
	mk := func() *sealedRequest {
		return gw.seal(RequestHeader{Target: "erp"}, [][]byte{[]byte("one"), []byte("two")}, agent)
	}
	open := func(r *sealedRequest) *Request {
		q, err := OpenRequest(r.env, []AgentKey{agent}, gw.keys)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}

	r := mk()
	if _, err := OpenRequest(r.env, []AgentKey{foreign}, gw.keys); !errors.Is(err, ErrDecrypt) {
		t.Errorf("a foreign key opened the request: %v", err)
	}
	if _, err := OpenRequest(r.env, []AgentKey{agent}, GatewayKeys{}); !errors.Is(err, ErrSignature) {
		t.Errorf("an unknown gateway key: %v", err)
	}
	other := newTestGateway(t)
	other.kid = gw.kid
	if _, err := OpenRequest(r.env, []AgentKey{agent}, other.keys); !errors.Is(err, ErrSignature) {
		t.Errorf("a gateway key that did not sign: %v", err)
	}

	tampered := mk()
	tampered.env.SealedHeader[3] ^= 1
	if _, err := OpenRequest(tampered.env, []AgentKey{agent}, gw.keys); !errors.Is(err, ErrSignature) {
		t.Errorf("an altered header: %v", err)
	}
	reRouted := mk()
	var routing Routing
	_ = json.Unmarshal(reRouted.env.Routing, &routing)
	routing.Target = "hr"
	reRouted.env.Routing, _ = json.Marshal(routing)
	if _, err := OpenRequest(reRouted.env, []AgentKey{agent}, gw.keys); !errors.Is(err, ErrSignature) {
		t.Errorf("a re-routed request: %v", err)
	}

	r = mk()
	q := open(r)
	if _, err := q.Chunk(r.chunks[1]); err == nil {
		t.Error("a reordered chunk was accepted")
	}
	r = mk()
	q = open(r)
	flipped := append([]byte(nil), r.chunks[0]...)
	flipped[0] ^= 1
	if _, err := q.Chunk(flipped); err == nil {
		t.Error("a flipped bit was accepted")
	}
	r = mk()
	q = open(r)
	_, _ = q.Chunk(r.chunks[0])
	if err := q.Final(r.final); err == nil {
		t.Error("a stream cut short was accepted: the final chunk must follow every data chunk")
	}
	r = mk()
	q = open(r)
	if _, err := q.Chunk(r.final); err == nil {
		t.Error("the final chunk was accepted as data")
	}
	r = mk()
	q = open(r)
	_, _ = q.Chunk(r.chunks[0])
	_, _ = q.Chunk(r.chunks[1])
	if err := q.Final(r.chunks[0]); err == nil {
		t.Error("a data chunk was accepted as final")
	}

	// A final chunk signed by a different party fails.
	r = mk()
	q = open(r)
	for _, c := range r.chunks {
		_, _ = q.Chunk(c)
	}
	q.gateway = other.keys[gw.kid]
	if err := q.Final(r.final); !errors.Is(err, ErrSignature) {
		t.Errorf("a final chunk the gateway did not sign: %v", err)
	}
}

func TestResponseSealing(t *testing.T) {
	gw := newTestGateway(t)
	agent, _ := GenerateKey(time.Now(), time.Hour)
	req := gw.seal(RequestHeader{Target: "erp"}, nil, agent)
	q, err := OpenRequest(req.env, []AgentKey{agent}, gw.keys)
	if err != nil {
		t.Fatal(err)
	}
	seal := func() (header []byte, chunks [][]byte, end []byte, rs *ResponseSealer) {
		rs, err := NewResponseSealer(q.Header.RespPub, q.Header.RequestID, agent)
		if err != nil {
			t.Fatal(err)
		}
		header, err = rs.Header(ResponseHeader{Status: 201, Headers: map[string][]string{"X-A": {"b"}}})
		if err != nil {
			t.Fatal(err)
		}
		chunks = [][]byte{rs.Chunk([]byte("abc")), rs.Chunk([]byte("def"))}
		end, err = rs.End(ResponseTrailer{UpstreamMs: 5, AgentMs: 7, Bytes: 6})
		if err != nil {
			t.Fatal(err)
		}
		return header, chunks, end, rs
	}
	header, chunks, end, _ := seal()
	got, err := gw.openResponse(req, agent, header, chunks, end)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.Status != 201 || got.Header.RequestID != q.Header.RequestID || string(got.Body) != "abcdef" || got.Trailer.UpstreamMs != 5 || got.Trailer.AgentKID != agent.KID() {
		t.Errorf("response = %+v", got)
	}

	// Another agent's key does not verify; a response for another request does not open.
	stranger, _ := GenerateKey(time.Now(), time.Hour)
	if _, err := gw.openResponse(req, stranger, header, chunks, end); !errors.Is(err, ErrSignature) {
		t.Errorf("a response signed by another key: %v", err)
	}
	req2 := gw.seal(RequestHeader{Target: "erp"}, nil, agent)
	if _, err := gw.openResponse(req2, agent, header, chunks, end); err == nil {
		t.Error("a response replayed for another request was accepted")
	}
	if _, err := gw.openResponse(req, agent, header, [][]byte{chunks[1], chunks[0]}, end); err == nil {
		t.Error("reordered response chunks were accepted")
	}
	if _, err := gw.openResponse(req, agent, header, chunks[:1], end); err == nil {
		t.Error("a truncated response was accepted")
	}
	if _, err := gw.openResponse(req, agent, header[:10], chunks, end); err == nil {
		t.Error("a short header was accepted")
	}
	if _, err := NewResponseSealer([]byte("short"), "r", agent); err == nil {
		t.Error("a bad response key was accepted")
	}
}

func TestGuard(t *testing.T) {
	now := time.Now()
	g := NewGuard("org_1", "net_1")
	h := baseHeader(RequestHeader{Target: "erp"})
	if err := g.Check(h, now); err != nil {
		t.Fatal(err)
	}
	if err := g.Check(h, now); !errors.Is(err, ErrReplay) {
		t.Errorf("same nonce and id: %v", err)
	}
	same := baseHeader(RequestHeader{Target: "erp", Nonce: randBytes(16)})
	same.RequestID = h.RequestID
	if err := g.Check(same, now); !errors.Is(err, ErrReplay) {
		t.Errorf("same request id: %v", err)
	}
	again := baseHeader(RequestHeader{Target: "erp", Nonce: h.Nonce})
	if err := g.Check(again, now); !errors.Is(err, ErrReplay) {
		t.Errorf("same nonce: %v", err)
	}
	for name, mod := range map[string]func(*RequestHeader){
		"old":         func(h *RequestHeader) { h.IssuedAtMs = now.Add(-61 * time.Second).UnixMilli() },
		"future":      func(h *RequestHeader) { h.IssuedAtMs = now.Add(61 * time.Second).UnixMilli() },
		"other org":   func(h *RequestHeader) { h.OrgID = "org_2" },
		"other net":   func(h *RequestHeader) { h.NetworkID = "net_2" },
		"short nonce": func(h *RequestHeader) { h.Nonce = []byte{1} },
		"version":     func(h *RequestHeader) { h.V = 2 },
	} {
		x := baseHeader(RequestHeader{Target: "erp"})
		mod(&x)
		if err := g.Check(x, now); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// A skewed clock is corrected by the server's offset.
	g.Offset = -2 * time.Minute
	skewed := baseHeader(RequestHeader{Target: "erp", IssuedAtMs: now.Add(-2 * time.Minute).UnixMilli()})
	if err := g.Check(skewed, now); err != nil {
		t.Errorf("skew correction: %v", err)
	}
	// Old entries are forgotten after the memory window.
	if err := g.Check(h, now.Add(6*time.Minute)); errors.Is(err, ErrReplay) {
		t.Error("the replay memory never expires")
	}
}

func TestParseJWKS(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	doc := func(kty, crv, x string) []byte {
		raw, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": kty, "crv": crv, "kid": "k1", "x": x}}})
		return raw
	}
	keys, err := ParseJWKS(doc("OKP", "Ed25519", base64.RawURLEncoding.EncodeToString(pub)))
	if err != nil || !keys["k1"].Equal(pub) {
		t.Fatalf("keys = %v, %v", keys, err)
	}
	for name, d := range map[string][]byte{
		"not json":  []byte("{"),
		"no keys":   []byte(`{"keys":[]}`),
		"rsa only":  doc("RSA", "", ""),
		"short key": doc("OKP", "Ed25519", "AAAA"),
		"bad b64":   doc("OKP", "Ed25519", "!!!"),
	} {
		if _, err := ParseJWKS(d); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestAgentKeys(t *testing.T) {
	k, err := GenerateKey(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.KID()) != 16 || len(k.EncPub()) != 32 || !strings.HasPrefix(k.NotAfter.Format(time.RFC3339), "20") {
		t.Errorf("key = %+v", k)
	}
	if (AgentKey{EncPriv: []byte{1}}).EncPub() != nil {
		t.Error("a bad private key produced a public key")
	}
}
