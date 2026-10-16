package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// brokerConn is the broker's side of one agent connection.
type brokerConn struct {
	t       *testing.T
	ws      *websocket.Conn
	wmu     sync.Mutex
	mu      sync.Mutex
	streams map[uint32]chan Frame
	control chan Frame
	hello   helloPayload
	closed  chan struct{}
	next    atomic.Uint32
}

func (c *brokerConn) send(f Frame) {
	c.t.Helper()
	raw, err := f.Marshal()
	if err != nil {
		c.t.Fatal(err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.WriteMessage(websocket.BinaryMessage, raw)
}

func (c *brokerConn) stream(id uint32) chan Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.streams[id]
	if !ok {
		ch = make(chan Frame, 4096)
		c.streams[id] = ch
	}
	return ch
}

func (c *brokerConn) readLoop() {
	defer close(c.closed)
	for {
		typ, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		if typ != websocket.BinaryMessage {
			continue
		}
		f, err := ParseFrame(data)
		if err != nil {
			continue
		}
		if f.StreamID == 0 {
			select {
			case c.control <- f:
			default:
			}
			continue
		}
		c.stream(f.StreamID) <- f
	}
}

// brokerResult is what the broker saw come back on a stream.
type brokerResult struct {
	Header []byte
	Chunks [][]byte
	End    []byte
	Err    *errorPayload
}

// exchangeOpts shapes how the fake broker drives one exchange.
type exchangeOpts struct {
	// grant is how much WINDOW credit the broker returns per received DATA
	// frame; nil means it returns all of it.
	noCredit bool
	// cancelAfterOpen sends CANCEL right after OPEN.
	cancelAfterOpen bool
	// tamperFinal flips a bit in the last request chunk.
	tamperFinal bool
	// dropFinal leaves out END.
	dropFinal bool
	wait      time.Duration
}

// start sends OPEN, the body and END on a new stream and returns the stream id.
func (c *brokerConn) start(req *sealedRequest, o exchangeOpts) uint32 {
	c.t.Helper()
	id := c.next.Add(1)
	payload, _ := json.Marshal(req.env)
	c.send(Frame{Type: FrameOpen, StreamID: id, Payload: payload})
	if o.cancelAfterOpen {
		c.send(Frame{Type: FrameCancel, StreamID: id, Payload: []byte(`{"reason":"test"}`)})
		return id
	}
	for _, ch := range req.chunks {
		c.send(Frame{Type: FrameData, StreamID: id, Payload: ch})
	}
	if !o.dropFinal {
		fin := append([]byte(nil), req.final...)
		if o.tamperFinal {
			fin[len(fin)-1] ^= 1
		}
		c.send(Frame{Type: FrameEnd, StreamID: id, Payload: fin})
	}
	return id
}

// collect reads the response of a stream until END or ERROR.
func (c *brokerConn) collect(id uint32, o exchangeOpts) brokerResult {
	c.t.Helper()
	wait := o.wait
	if wait == 0 {
		wait = 10 * time.Second
	}
	var res brokerResult
	timeout := time.After(wait)
	for {
		select {
		case f := <-c.stream(id):
			switch f.Type {
			case FrameData:
				if !o.noCredit {
					raw, _ := json.Marshal(map[string]int{"credit_bytes": len(f.Payload)})
					c.send(Frame{Type: FrameWindow, StreamID: id, Payload: raw})
				}
				if f.Flags&FlagHeader != 0 {
					res.Header = f.Payload
				} else {
					res.Chunks = append(res.Chunks, f.Payload)
				}
			case FrameEnd:
				res.End = f.Payload
				return res
			case FrameError:
				var e errorPayload
				_ = json.Unmarshal(f.Payload, &e)
				res.Err = &e
				return res
			}
		case <-timeout:
			c.t.Fatalf("no response on stream %d (header %v, %d chunks)", id, res.Header != nil, len(res.Chunks))
		case <-c.closed:
			c.t.Fatal("the agent closed the connection")
		}
	}
}

func (c *brokerConn) run(req *sealedRequest, o exchangeOpts) brokerResult {
	c.t.Helper()
	return c.collect(c.start(req, o), o)
}

// fakeBroker accepts agents over a real WebSocket.
type fakeBroker struct {
	t       *testing.T
	srv     *httptest.Server
	conns   chan *brokerConn
	connect atomic.Int32
	// refuse, when set, answers the upgrade with this status and code.
	refuseStatus atomic.Int32
	refuseCode   atomic.Value
	serverTime   atomic.Int64
	headers      chan http.Header
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	b := &fakeBroker{t: t, conns: make(chan *brokerConn, 8), headers: make(chan http.Header, 8)}
	up := websocket.Upgrader{Subprotocols: []string{Subprotocol}, CheckOrigin: func(*http.Request) bool { return true }}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/connect" {
			http.NotFound(w, r)
			return
		}
		select {
		case b.headers <- r.Header.Clone():
		default:
		}
		if st := int(b.refuseStatus.Load()); st != 0 {
			code, _ := b.refuseCode.Load().(string)
			w.WriteHeader(st)
			_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
			return
		}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b.connect.Add(1)
		c := &brokerConn{t: t, ws: ws, streams: map[uint32]chan Frame{}, control: make(chan Frame, 64), closed: make(chan struct{})}
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		f, err := ParseFrame(data)
		if err != nil || f.Type != FrameHello {
			return
		}
		_ = json.Unmarshal(f.Payload, &c.hello)
		st := b.serverTime.Load()
		if st == 0 {
			st = time.Now().UnixMilli()
		}
		ack, _ := json.Marshal(map[string]any{"server_time": st, "min_version": "0.0.0", "heartbeat_s": 20})
		c.send(Frame{Type: FrameHelloAck, Payload: ack})
		b.conns <- c
		c.readLoop()
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeBroker) waitConn() *brokerConn {
	b.t.Helper()
	select {
	case c := <-b.conns:
		return c
	case <-time.After(10 * time.Second):
		b.t.Fatal("the agent did not connect")
	}
	return nil
}

// agentRig is an agent wired to a fake broker and a test gateway.
type agentRig struct {
	t      *testing.T
	a      *Agent
	b      *fakeBroker
	gw     *testGateway
	key    AgentKey
	dir    string
	cfgPth string
	out    *syncBuffer
	cancel context.CancelFunc
	done   chan error
	conn   *brokerConn
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) lines() []map[string]any {
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func (s *syncBuffer) has(event string) map[string]any {
	for _, m := range s.lines() {
		if m["event"] == event {
			return m
		}
	}
	return nil
}

// prepBroker lets a test adjust the fake broker before the agent connects.
var prepBroker func(*fakeBroker)

// newRig starts an agent against a fake broker with the given targets YAML.
func newRig(t *testing.T, targets string, mod func(*AgentOptions)) *agentRig {
	t.Helper()
	r := &agentRig{t: t, b: newFakeBroker(t), gw: newTestGateway(t), out: &syncBuffer{}, dir: t.TempDir(), done: make(chan error, 1)}
	if prepBroker != nil {
		prepBroker(r.b)
	}
	key, err := GenerateKey(time.Now(), 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r.key = key
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": r.gw.kid, "x": b64url(r.gw.keys[r.gw.kid])}}})
	id := &Identity{AgentID: "nag_1", NetworkID: "net_1", OrgID: "org_1", OrgSlug: "acme", Name: "test-agent", BrokerURL: r.b.srv.URL, AppURL: "https://app.example", Keys: []AgentKey{key}, GatewayJWKS: jwks, JWKSFetched: time.Now(), KeyValidity: 30 * 24 * time.Hour}
	state := filepath.Join(r.dir, "state")
	if err := id.Save(state); err != nil {
		t.Fatal(err)
	}
	r.cfgPth = filepath.Join(r.dir, "config.yaml")
	writeFile(t, r.cfgPth, "version: 1\nnetwork: net_1\nname: test-agent\nstate_dir: "+state+"\nshutdown_grace: 1s\nlog: { upload: errors }\ntargets:\n"+targets)
	opts := AgentOptions{ConfigPath: r.cfgPth, StateDir: state, Stdout: r.out, Backoff: func(int) time.Duration { return 30 * time.Millisecond }, ReloadPoll: 50 * time.Millisecond, StatusEvery: 200 * time.Millisecond, HTTP: &http.Client{Timeout: 5 * time.Second}}
	if mod != nil {
		mod(&opts)
	}
	a, err := NewAgent(opts)
	if err != nil {
		t.Fatal(err)
	}
	r.a = a
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	finished := make(chan struct{})
	go func() {
		err := a.Run(ctx)
		close(finished)
		r.done <- err
	}()
	r.conn = r.b.waitConn()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
		}
		a.Close()
	})
	return r
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seal builds a request for the rig's agent.
func (r *agentRig) seal(h RequestHeader, body ...[]byte) *sealedRequest {
	return r.gw.seal(h, body, r.key)
}

// open decrypts a finished exchange as the gateway would.
func (r *agentRig) open(req *sealedRequest, res brokerResult) *openedResponse {
	r.t.Helper()
	if res.Err != nil {
		r.t.Fatalf("exchange failed: %+v", res.Err)
	}
	out, err := r.gw.openResponse(req, r.key, res.Header, res.Chunks, res.End)
	if err != nil {
		r.t.Fatalf("response did not open: %v", err)
	}
	return out
}

func (r *agentRig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("timed out waiting for %s", what)
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
