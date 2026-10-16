package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Exit statuses of the connector.
const (
	ExitConfig  = 2
	ExitRevoked = 3
)

// ErrRevoked ends the agent for good: the broker no longer knows it.
var ErrRevoked = errors.New("the gateway has revoked this agent or does not know it; enroll it again")

// ErrRestart ends Run after an update was installed: the caller starts the
// new binary.
var ErrRestart = errors.New("restart to run the updated binary")

// ErrVersionUnsupported means the broker refuses this version.
var ErrVersionUnsupported = errors.New("version_unsupported")

// ErrNetworkDisabled means the network is switched off.
var ErrNetworkDisabled = errors.New("network_disabled")

// Conn is one WebSocket carrying frames.
type Conn interface {
	ReadFrame() (Frame, error)
	WriteFrame(Frame) error
	Ping() error
	Close(code int, reason string) error
}

// DialFunc opens the connection to the broker.
type DialFunc func(ctx context.Context, rawURL string, header http.Header, cfg *Config) (Conn, error)

// AgentOptions configure an Agent.
type AgentOptions struct {
	ConfigPath string
	StateDir   string
	Stdout     io.Writer
	Dial       DialFunc
	HTTP       *http.Client
	Now        func() time.Time
	// Backoff replaces the reconnect delay; tests make it short.
	Backoff func(attempt int) time.Duration
	// Executable is the path of the running binary, for self-update and its
	// rollback; empty disables both.
	Executable string
	// UpdateKeys override the compiled-in manifest keys.
	UpdateKeys []ed25519.PublicKey
	// ReloadPoll is how often the config file is checked for changes.
	ReloadPoll time.Duration
	// StatusEvery is the STATUS interval.
	StatusEvery time.Duration
}

// Agent is the connector: identity, config, targets and the session loop.
type Agent struct {
	opts AgentOptions
	log  *Logger

	cfgPtr atomic.Pointer[Config]
	idMu   sync.RWMutex
	id     *Identity
	guard  *Guard

	httpx   *HTTPExecutor
	supMu   sync.Mutex
	stdio   map[string]*StdioSupervisor
	metrics *Metrics

	paused           atomic.Bool
	connected        atomic.Bool
	ready            atomic.Bool
	draining         atomic.Bool
	spillUnavailable atomic.Bool
	inflight         atomic.Int32
	started          time.Time
	lastErr          atomic.Value
	cfgMTime         time.Time
	cfgSize          int64
	reloadCh         chan struct{}
	statusNow        chan struct{}
	rotateNow        chan struct{}
	restartCh        chan struct{}
	restarting       atomic.Bool
	netAutoUpdate    atomic.Value
	offset           atomic.Int64
	sess             atomic.Pointer[session]
}

// NewAgent loads the config and identity. A config problem is an *ErrConfig.
func NewAgent(o AgentOptions) (*Agent, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Backoff == nil {
		o.Backoff = defaultBackoff
	}
	if o.ReloadPoll == 0 {
		o.ReloadPoll = 5 * time.Second
	}
	if o.StatusEvery == 0 {
		o.StatusEvery = 60 * time.Second
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if o.Dial == nil {
		o.Dial = dialWebSocket
	}
	cfg, err := LoadConfig(o.ConfigPath)
	if err != nil {
		return nil, err
	}
	if o.StateDir == "" {
		o.StateDir = cfg.StateDir
	}
	id, err := LoadIdentity(o.StateDir)
	if err != nil {
		return nil, err
	}
	if cfg.Network != id.NetworkID {
		return nil, &ErrConfig{[]string{fmt.Sprintf("network is %s in the config but this agent enrolled in %s", cfg.Network, id.NetworkID)}}
	}
	log, err := NewLogger(o.Stdout, cfg.Log.Format, cfg.Log.File, cfg.Log.MaxSizeMB, cfg.Log.MaxFiles, cfg.Log.Upload)
	if err != nil {
		return nil, &ErrConfig{[]string{"log.file: " + err.Error()}}
	}
	a := &Agent{
		opts: o, log: log, id: id, guard: NewGuard(id.OrgID, id.NetworkID), httpx: NewHTTPExecutor(),
		stdio: map[string]*StdioSupervisor{}, metrics: newMetrics(), started: o.Now(),
		reloadCh: make(chan struct{}, 1), statusNow: make(chan struct{}, 1), rotateNow: make(chan struct{}, 1), restartCh: make(chan struct{}, 1),
	}
	a.cfgPtr.Store(cfg)
	a.noteConfigFile()
	return a, nil
}

func defaultBackoff(attempt int) time.Duration {
	ceil := time.Second << min(attempt, 6)
	if ceil > 60*time.Second {
		ceil = 60 * time.Second
	}
	return time.Duration(rand.Int64N(int64(ceil)) + 1)
}

func (a *Agent) config() *Config { return a.cfgPtr.Load() }
func (a *Agent) now() time.Time  { return a.opts.Now() }
func (a *Agent) stateDir() string {
	return a.opts.StateDir
}

func (a *Agent) identity() *Identity {
	a.idMu.RLock()
	defer a.idMu.RUnlock()
	return a.id
}

func (a *Agent) checkGuard(h RequestHeader, now time.Time) error {
	a.idMu.Lock()
	defer a.idMu.Unlock()
	a.guard.Offset = time.Duration(a.offset.Load())
	return a.guard.Check(h, now)
}

func (a *Agent) noteError(code string) { a.lastErr.Store(code) }

// gatewayKeys are the keys requests may be signed with, narrowed to the kids
// the config pins.
func (a *Agent) gatewayKeys() (GatewayKeys, error) {
	keys, err := a.identity().GatewayKeys()
	if err != nil {
		return nil, err
	}
	pins := a.config().AllowedGatewayKIDs
	if len(pins) == 0 {
		return keys, nil
	}
	out := GatewayKeys{}
	for _, k := range pins {
		if pub, ok := keys[k]; ok {
			out[k] = pub
		}
	}
	return out, nil
}

func (a *Agent) supervisor(t Target) *StdioSupervisor {
	a.supMu.Lock()
	defer a.supMu.Unlock()
	if s, ok := a.stdio[t.Name]; ok {
		return s
	}
	s := NewStdioSupervisor(t, a.log)
	a.stdio[t.Name] = s
	return s
}

func (a *Agent) spillClient() *http.Client { return a.opts.HTTP }

// Pause refuses every exchange while the agent stays connected.
func (a *Agent) Pause() { a.paused.Store(true); a.requestStatus() }

// Resume undoes Pause.
func (a *Agent) Resume() { a.paused.Store(false); a.requestStatus() }

// Paused reports the local kill switch.
func (a *Agent) Paused() bool { return a.paused.Load() }

// RotateNow asks the run loop to rotate the keys.
func (a *Agent) RotateNow() {
	select {
	case a.rotateNow <- struct{}{}:
	default:
	}
}

// Reload asks the run loop to read the config again.
func (a *Agent) Reload() {
	select {
	case a.reloadCh <- struct{}{}:
	default:
	}
}

func (a *Agent) requestStatus() {
	select {
	case a.statusNow <- struct{}{}:
	default:
	}
}

func (a *Agent) noteConfigFile() {
	if info, err := os.Stat(a.opts.ConfigPath); err == nil {
		a.cfgMTime, a.cfgSize = info.ModTime(), info.Size()
	}
}

func (a *Agent) configFileChanged() bool {
	info, err := os.Stat(a.opts.ConfigPath)
	return err == nil && (!info.ModTime().Equal(a.cfgMTime) || info.Size() != a.cfgSize)
}

// reload reads the config again. An invalid file leaves the old one in force.
func (a *Agent) reload() {
	a.noteConfigFile()
	cfg, err := LoadConfig(a.opts.ConfigPath)
	if err != nil {
		a.log.Event("error", "config_reload_failed", map[string]any{"error": err.Error()})
		return
	}
	if cfg.Network != a.identity().NetworkID {
		a.log.Event("error", "config_reload_failed", map[string]any{"error": "network does not match the enrolled network"})
		return
	}
	old := a.config()
	a.cfgPtr.Store(cfg)
	a.supMu.Lock()
	for name, s := range a.stdio {
		nt, ok := cfg.Target(name)
		ot, _ := old.Target(name)
		if !ok || !sameStdio(nt, ot) {
			go s.Close()
			delete(a.stdio, name)
		}
	}
	a.supMu.Unlock()
	a.log.Event("info", "config_reloaded", map[string]any{"allowlist_hash": cfg.Allowlist().Hash, "targets": len(cfg.Targets)})
	a.requestStatus()
}

func sameStdio(a, b Target) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return string(ra) == string(rb)
}

// wsConn is the WebSocket connection of the real broker.
type wsConn struct {
	c  *websocket.Conn
	mu sync.Mutex
}

func (w *wsConn) ReadFrame() (Frame, error) {
	for {
		typ, data, err := w.c.ReadMessage()
		if err != nil {
			return Frame{}, err
		}
		if typ != websocket.BinaryMessage {
			continue
		}
		return ParseFrame(data)
	}
}

func (w *wsConn) WriteFrame(f Frame) error {
	raw, err := f.Marshal()
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return w.c.WriteMessage(websocket.BinaryMessage, raw)
}

func (w *wsConn) Ping() error {
	return w.c.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
}

func (w *wsConn) Close(code int, reason string) error {
	_ = w.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	return w.c.Close()
}

// refusalFromBody reads the code the broker gave in its refusal.
func refusalFromBody(body io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(body, 64<<10))
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Error != "" {
		return e.Error
	}
	return e.Code
}

func dialWebSocket(ctx context.Context, rawURL string, header http.Header, cfg *Config) (Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	d := &websocket.Dialer{Subprotocols: []string{Subprotocol}, HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment}
	if cfg.Broker.Proxy != "" {
		pu, err := url.Parse(cfg.Broker.Proxy)
		if err != nil {
			return nil, fmt.Errorf("broker.proxy: %w", err)
		}
		d.Proxy = http.ProxyURL(pu)
	}
	if cfg.Broker.CAFile != "" {
		pem, err := os.ReadFile(cfg.Broker.CAFile)
		if err != nil {
			return nil, fmt.Errorf("broker.ca_file: %w", err)
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("broker.ca_file holds no certificate")
		}
		d.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	c, resp, err := d.DialContext(ctx, u.String(), header)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			if code := refusalFromBody(resp.Body); code != "" {
				return nil, &refusal{code: code, status: resp.StatusCode}
			}
		}
		return nil, err
	}
	c.SetReadLimit(MaxPayload + headerLen + 1024)
	w := &wsConn{c: c}
	c.SetPongHandler(func(string) error { return c.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	return w, nil
}

// refusal is a connection the broker declined, with its reason.
type refusal struct {
	code   string
	status int
}

func (r *refusal) Error() string {
	return fmt.Sprintf("the broker refused the connection: %s (HTTP %d)", r.code, r.status)
}

// classifyRefusal maps a broker's reason onto what the run loop does next.
func classifyRefusal(err error) (retryAfter time.Duration, fatal error) {
	var code string
	var r *refusal
	var ce *websocket.CloseError
	switch {
	case errors.As(err, &r):
		code = r.code
	case errors.As(err, &ce):
		code = strings.TrimSpace(ce.Text)
	default:
		return 0, nil
	}
	switch code {
	case "agent_revoked", "agent_unknown", "key_expired":
		return 0, fmt.Errorf("%w (%s)", ErrRevoked, code)
	case "version_unsupported":
		return time.Hour, ErrVersionUnsupported
	case "network_disabled":
		return 10 * time.Minute, ErrNetworkDisabled
	}
	return 0, nil
}

// Run keeps the agent connected until ctx ends or the agent is revoked. When
// ctx ends it drains: in-flight exchanges get the shutdown grace.
func (a *Agent) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		select {
		case <-a.restartCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	go a.watchConfig(ctx)
	go a.maintain(ctx)
	go a.watchPending(ctx)
	attempt := 0
	for {
		stable, err := a.session(ctx)
		if ctx.Err() != nil {
			if a.restarting.Load() && parent.Err() == nil {
				return ErrRestart
			}
			return nil
		}
		a.connected.Store(false)
		a.ready.Store(false)
		wait := time.Duration(0)
		after, kind := classifyRefusal(err)
		switch {
		case err == nil:
		case errors.Is(kind, ErrRevoked):
			a.log.Event("error", "revoked", map[string]any{"error": kind.Error()})
			return kind
		default:
			a.log.Event("warn", "disconnected", map[string]any{"error": err.Error()})
		}
		if kind != nil {
			a.log.Event("warn", "refused", map[string]any{"reason": kind.Error()})
			wait = after
		}
		if stable {
			attempt = 0
		}
		if wait == 0 {
			wait = a.opts.Backoff(attempt)
			attempt++
		}
		a.metrics.reconnects.Add(1)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// watchConfig reloads on SIGHUP (via Reload) and when the file changes.
func (a *Agent) watchConfig(ctx context.Context) {
	t := time.NewTicker(a.opts.ReloadPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.reloadCh:
			a.reload()
		case <-t.C:
			if a.configFileChanged() {
				a.reload()
			}
		}
	}
}

// maintain rotates keys when they near their end and refreshes the gateway's
// keys every six hours.
func (a *Agent) maintain(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	go a.autoUpdateLoop(ctx)
	check := func(force bool) {
		id := a.identity()
		if force || id.NeedsRotation(a.now()) {
			if err := a.rotate(ctx); err != nil {
				a.log.Event("warn", "rotation_failed", map[string]any{"error": err.Error()})
			}
		}
		if a.now().Sub(id.JWKSFetched) > 6*time.Hour {
			cp := *a.identity()
			if err := RefreshGatewayKeys(ctx, a.opts.HTTP, &cp, a.stateDir(), a.now()); err != nil {
				a.log.Event("warn", "gateway_keys_refresh_failed", map[string]any{"error": err.Error()})
			} else {
				a.idMu.Lock()
				a.id.GatewayJWKS, a.id.JWKSFetched = cp.GatewayJWKS, cp.JWKSFetched
				a.idMu.Unlock()
			}
		}
	}
	check(false)
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.rotateNow:
			check(true)
		case <-t.C:
			check(false)
		}
	}
}

func (a *Agent) rotate(ctx context.Context) error {
	a.idMu.Lock()
	cp := *a.id
	cp.Keys = append([]AgentKey(nil), a.id.Keys...)
	a.idMu.Unlock()
	if err := RotateKeys(ctx, a.opts.HTTP, &cp, a.stateDir(), a.now()); err != nil {
		return err
	}
	a.idMu.Lock()
	a.id.Keys = cp.Keys
	a.idMu.Unlock()
	a.log.Event("info", "rotated", map[string]any{"kid": cp.Current().KID()})
	// The broker learns the new key from the gateway; reconnect so the
	// connection is authenticated with it.
	if s := a.sess.Load(); s != nil {
		s.conn.Close(websocket.CloseGoingAway, "rotated")
	}
	return nil
}

// Status is what the control socket and /readyz report.
type Status struct {
	State         string `json:"state"`
	Connected     bool   `json:"connected"`
	Paused        bool   `json:"paused"`
	Ready         bool   `json:"ready"`
	AgentID       string `json:"agent_id"`
	NetworkID     string `json:"network_id"`
	Broker        string `json:"broker"`
	Version       string `json:"version"`
	Proto         int    `json:"proto"`
	AllowlistHash string `json:"allowlist_hash"`
	InFlight      int32  `json:"in_flight"`
	LastError     string `json:"last_error,omitempty"`
	KeyAgeDays    int    `json:"key_age_days"`
	UpdateChannel string `json:"update_channel"`
	UptimeSeconds int64  `json:"uptime_s"`
	Targets       int    `json:"targets"`
}

// Status snapshots the agent.
func (a *Agent) Status() Status {
	id, cfg := a.identity(), a.config()
	st := Status{
		Connected: a.connected.Load(), Paused: a.paused.Load(), Ready: a.ready.Load(), AgentID: id.AgentID, NetworkID: id.NetworkID,
		Broker: id.BrokerURL, Version: Version, Proto: ProtoVersion, AllowlistHash: cfg.Allowlist().Hash, InFlight: a.inflight.Load(),
		KeyAgeDays: int(a.now().Sub(id.Current().CreatedAt).Hours() / 24), UpdateChannel: cfg.Update.Channel,
		UptimeSeconds: int64(a.now().Sub(a.started).Seconds()), Targets: len(cfg.Targets),
	}
	if v, _ := a.lastErr.Load().(string); v != "" {
		st.LastError = v
	}
	switch {
	case a.draining.Load():
		st.State = "draining"
	case a.paused.Load():
		st.State = "paused"
	case a.connected.Load():
		st.State = "connected"
	default:
		st.State = "connecting"
	}
	return st
}

// Close stops the targets' processes and closes the log.
func (a *Agent) Close() {
	a.httpx.Close()
	a.supMu.Lock()
	for _, s := range a.stdio {
		s.Close()
	}
	a.supMu.Unlock()
	_ = a.log.Close()
}
