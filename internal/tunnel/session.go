package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// session is one connection to the broker and the exchanges running on it.
type session struct {
	a       *Agent
	conn    Conn
	wmu     sync.Mutex
	mu      sync.Mutex
	streams map[uint32]*exchange

	exCtx    context.Context
	exCancel context.CancelFunc
	// brokerDrain is set when the broker asked the agent to move on.
	brokerDrain bool
}

func (s *session) send(f Frame) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.conn.WriteFrame(f)
}

func (s *session) gatewayKID(r *Request) string { return r.GatewayKID }

type helloPayload struct {
	AgentID        string    `json:"agent_id"`
	NetworkID      string    `json:"network_id"`
	Name           string    `json:"name"`
	Proto          []int     `json:"proto"`
	Version        string    `json:"version"`
	OS             string    `json:"os"`
	Arch           string    `json:"arch"`
	Allowlist      Allowlist `json:"allowlist"`
	MaxConcurrency int       `json:"max_concurrency"`
}

type helloAck struct {
	ServerTime json.RawMessage `json:"server_time"`
	MinVersion string          `json:"min_version"`
	Heartbeat  int             `json:"heartbeat_s"`
	Drain      bool            `json:"drain"`
	// AutoUpdate is the network's setting: off, patch or minor.
	AutoUpdate string `json:"auto_update"`
}

// serverOffset reads the broker's clock, as milliseconds or an RFC 3339 time,
// and returns how far it is ahead of ours.
func serverOffset(raw json.RawMessage, now time.Time) time.Duration {
	var ms int64
	if json.Unmarshal(raw, &ms) == nil && ms > 0 {
		return time.UnixMilli(ms).Sub(now)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Sub(now)
		}
	}
	return 0
}

func brokerConnectURL(a *Agent) (string, error) {
	base := a.config().Broker.URL
	if base == "" {
		base = a.identity().BrokerURL
	}
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("the broker address %q is not valid", base)
	}
	return u.String() + "/v1/agent/connect", nil
}

// session connects, serves exchanges until the connection ends and reports
// whether it stayed up long enough to count as stable.
func (a *Agent) session(ctx context.Context) (stable bool, err error) {
	id := a.identity()
	endpoint, err := brokerConnectURL(a)
	if err != nil {
		return false, &ErrConfig{[]string{err.Error()}}
	}
	u, _ := url.Parse(endpoint)
	token, err := MintJWT(id.Current(), id.AgentID, u.Scheme+"://"+u.Host, id.NetworkID, a.now())
	if err != nil {
		return false, err
	}
	hdr := http.Header{"Authorization": {"Bearer " + token}, "X-Agent-Id": {id.AgentID}}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	conn, err := a.opts.Dial(dctx, endpoint, hdr, a.config())
	cancel()
	if err != nil {
		return false, err
	}
	exCtx, exCancel := context.WithCancel(context.Background())
	s := &session{a: a, conn: conn, streams: map[uint32]*exchange{}, exCtx: exCtx, exCancel: exCancel}
	a.sess.Store(s)
	defer func() {
		a.sess.CompareAndSwap(s, nil)
		exCancel()
		_ = conn.Close(websocket.CloseNormalClosure, "")
	}()

	if err := s.hello(); err != nil {
		return false, err
	}
	ack, err := s.waitAck()
	if err != nil {
		return false, err
	}
	a.offset.Store(int64(serverOffset(ack.ServerTime, a.now())))
	a.netAutoUpdate.Store(ack.AutoUpdate)
	a.draining.Store(false)
	a.connected.Store(true)
	a.ready.Store(true)
	a.log.Event("info", "connected", map[string]any{"network": id.NetworkID, "agent_id": id.AgentID, "allowlist_hash": a.config().Allowlist().Hash})
	connectedAt := a.now()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.keepalive(done, ack.Heartbeat) }()
	go func() { defer wg.Done(); s.statusLoop(done) }()
	go func() {
		select {
		case <-ctx.Done():
			s.drain()
		case <-done:
		}
	}()
	err = s.readLoop()
	close(done)
	wg.Wait()
	if s.brokerDrain {
		time.Sleep(time.Duration(rand.Int64N(int64(5 * time.Second))))
		return a.now().Sub(connectedAt) > 60*time.Second, nil
	}
	return a.now().Sub(connectedAt) > 60*time.Second, err
}

func (s *session) hello() error {
	id, cfg := s.a.identity(), s.a.config()
	host, _ := osHostname()
	name := cfg.Name
	if name == "" {
		name = host
	}
	raw, _ := json.Marshal(helloPayload{
		AgentID: id.AgentID, NetworkID: id.NetworkID, Name: name, Proto: []int{ProtoVersion}, Version: Version,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Allowlist: cfg.Allowlist(), MaxConcurrency: cfg.MaxConcurrency,
	})
	return s.send(Frame{Type: FrameHello, Payload: raw})
}

func (s *session) waitAck() (helloAck, error) {
	type result struct {
		f   Frame
		err error
	}
	ch := make(chan result, 1)
	go func() { f, err := s.conn.ReadFrame(); ch <- result{f, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			return helloAck{}, r.err
		}
		if r.f.Type != FrameHelloAck {
			return helloAck{}, fmt.Errorf("the broker answered HELLO with %s", r.f.Type)
		}
		var ack helloAck
		if err := json.Unmarshal(r.f.Payload, &ack); err != nil {
			return helloAck{}, fmt.Errorf("HELLO_ACK is not JSON: %w", err)
		}
		return ack, nil
	case <-time.After(10 * time.Second):
		_ = s.conn.Close(websocket.CloseProtocolError, "no HELLO_ACK")
		return helloAck{}, errors.New("the broker did not answer HELLO")
	}
}

func (s *session) keepalive(done <-chan struct{}, heartbeat int) {
	every := 20 * time.Second
	if heartbeat > 0 && time.Duration(heartbeat)*time.Second < every {
		every = time.Duration(heartbeat) * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := s.conn.Ping(); err != nil {
				_ = s.conn.Close(websocket.CloseGoingAway, "ping failed")
				return
			}
		}
	}
}

type statusPayload struct {
	InFlight        int32             `json:"in_flight"`
	Paused          bool              `json:"paused"`
	ConfigHash      string            `json:"config_hash"`
	UptimeS         int64             `json:"uptime_s"`
	LastUpstreamErr string            `json:"last_upstream_error,omitempty"`
	Version         string            `json:"version"`
	Allowlist       Allowlist         `json:"allowlist"`
	ListChanged     []string          `json:"list_changed,omitempty"`
	SpillUnavail    bool              `json:"spill_unavailable,omitempty"`
	Violations      int64             `json:"policy_violations"`
	Logs            []json.RawMessage `json:"logs,omitempty"`
}

func (s *session) statusFrame() Frame {
	a := s.a
	cfg := a.config()
	st := statusPayload{
		InFlight: a.inflight.Load(), Paused: a.paused.Load(), ConfigHash: cfg.Allowlist().Hash, UptimeS: int64(a.now().Sub(a.started).Seconds()),
		Version: Version, Allowlist: cfg.Allowlist(), SpillUnavail: a.spillUnavailable.Load(), Violations: a.metrics.violations.Load(),
		Logs: a.log.TakeUpload(),
	}
	if v, _ := a.lastErr.Load().(string); v != "" {
		st.LastUpstreamErr = v
	}
	a.supMu.Lock()
	for name, sup := range a.stdio {
		if sup.ListChanged() {
			st.ListChanged = append(st.ListChanged, name)
		}
	}
	a.supMu.Unlock()
	raw, _ := json.Marshal(st)
	return Frame{Type: FrameStatus, Payload: raw}
}

func (s *session) statusLoop(done <-chan struct{}) {
	t := time.NewTicker(s.a.opts.StatusEvery)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		case <-s.a.statusNow:
		}
		if err := s.send(s.statusFrame()); err != nil {
			return
		}
	}
}

// drain stops taking new exchanges, gives running ones the shutdown grace and
// cancels what is left with agent_shutdown.
func (s *session) drain() {
	a := s.a
	a.draining.Store(true)
	_ = s.send(Frame{Type: FrameDrain})
	deadline := time.Now().Add(time.Duration(a.config().ShutdownGrace))
	for a.inflight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	s.mu.Lock()
	left := make([]*exchange, 0, len(s.streams))
	for _, ex := range s.streams {
		left = append(left, ex)
	}
	s.mu.Unlock()
	for _, ex := range left {
		ex.sendError(&ExecError{Code: "agent_shutdown", Dispatched: true})
		ex.cancel()
	}
	_ = s.conn.Close(websocket.CloseGoingAway, "shutdown")
}

func (s *session) readLoop() error {
	for {
		f, err := s.conn.ReadFrame()
		if err != nil {
			if s.a.draining.Load() {
				return nil
			}
			return err
		}
		switch f.Type {
		case FrameOpen:
			s.open(f)
		case FrameData, FrameEnd:
			s.route(f)
		case FrameWindow:
			var w struct {
				Credit int64 `json:"credit_bytes"`
			}
			if json.Unmarshal(f.Payload, &w) == nil {
				if ex := s.lookup(f.StreamID); ex != nil {
					ex.win.add(w.Credit)
				}
			}
		case FrameCancel:
			if ex := s.lookup(f.StreamID); ex != nil {
				ex.cancel()
			}
		case FrameDrain:
			s.brokerDrain = true
			s.a.log.Event("info", "broker_drain", nil)
			return nil
		case FrameHelloAck:
			var ack helloAck
			if json.Unmarshal(f.Payload, &ack) == nil {
				s.a.offset.Store(int64(serverOffset(ack.ServerTime, s.a.now())))
			}
		}
	}
}

func (s *session) lookup(id uint32) *exchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[id]
}

func (s *session) route(f Frame) {
	ex := s.lookup(f.StreamID)
	if ex == nil {
		return
	}
	select {
	case ex.in <- f:
	case <-ex.ctx.Done():
	}
}

func (s *session) open(f Frame) {
	a := s.a
	if a.draining.Load() {
		_ = s.send(errorFrame(f.StreamID, &ExecError{Code: "agent_shutdown", Retryable: true}))
		return
	}
	if int(a.inflight.Load()) >= a.config().MaxConcurrency {
		_ = s.send(errorFrame(f.StreamID, &ExecError{Code: "agent_busy", Retryable: true}))
		return
	}
	ctx, cancel := context.WithCancel(s.exCtx)
	ex := &exchange{a: a, s: s, id: f.StreamID, in: make(chan Frame, 64), ctx: ctx, cancel: cancel, win: newWindow(initialWindow), started: a.now()}
	s.mu.Lock()
	if _, dup := s.streams[f.StreamID]; dup {
		s.mu.Unlock()
		cancel()
		_ = s.send(errorFrame(f.StreamID, &ExecError{Code: "replay", Msg: "stream id in use"}))
		return
	}
	s.streams[f.StreamID] = ex
	s.mu.Unlock()
	a.inflight.Add(1)
	go ex.run(f)
}

func errorFrame(stream uint32, e *ExecError) Frame {
	raw, _ := json.Marshal(errorPayload{Code: e.Code, Retryable: e.Retryable, Dispatched: e.Dispatched, Message: e.Msg})
	return Frame{Type: FrameError, StreamID: stream, Payload: raw}
}
