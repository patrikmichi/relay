package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Version is the connector's version, set at build time.
var Version = "dev"

// StdioSupervisor runs the local process of one mcp_stdio target: it starts it
// lazily, owns the MCP session, correlates one JSON-RPC request per exchange
// and stops it when idle.
type StdioSupervisor struct {
	Target Target
	Log    *Logger
	// Backoff bounds how soon a crashed process is started again.
	MinBackoff, MaxBackoff time.Duration
	// Grace is how long a process gets to leave after its input closes, and
	// again after SIGTERM, before it is killed.
	Grace time.Duration

	mu           sync.Mutex
	proc         *stdioProc
	crashes      int
	lastExit     time.Time
	idle         *time.Timer
	listChanged  atomic.Bool
	nextID       atomic.Int64
	resolveEnv   func(string) (string, error)
	startCommand func(t Target, env []string) (*exec.Cmd, error)
}

type stdioProc struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	mu       sync.Mutex
	pending  map[int64]chan json.RawMessage
	initRes  json.RawMessage
	done     chan struct{}
	inFlight atomic.Int32
}

// NewStdioSupervisor supervises one target.
func NewStdioSupervisor(t Target, log *Logger) *StdioSupervisor {
	return &StdioSupervisor{Target: t, Log: log, MinBackoff: time.Second, MaxBackoff: time.Minute, Grace: 2 * time.Second, resolveEnv: ResolveSecret, startCommand: defaultStartCommand}
}

// ListChanged reports and clears a tools/list_changed notification, so the
// gateway can discover again.
func (s *StdioSupervisor) ListChanged() bool { return s.listChanged.Swap(false) }

func defaultStartCommand(t Target, env []string) (*exec.Cmd, error) {
	cmd := exec.Command(t.Command, t.Args...) //nolint:gosec // the command comes from the local config only
	cmd.Env = env
	cmd.Dir = t.Cwd
	if err := configureStdioProc(cmd, t.RunAsUser); err != nil {
		return nil, err
	}
	return cmd, nil
}

func (s *StdioSupervisor) environment() ([]string, error) {
	var env []string
	if s.Target.EnvInherit {
		env = os.Environ()
	}
	for k, v := range s.Target.Env {
		val, err := s.resolveEnv(v)
		if err != nil {
			return nil, &ExecError{Code: "credentials_missing", Msg: fmt.Sprintf("env %s: %v", k, err)}
		}
		env = append(env, k+"="+val)
	}
	return env, nil
}

// ensure starts the process if none is running and completes the MCP
// handshake with it.
func (s *StdioSupervisor) ensure(ctx context.Context) (*stdioProc, *ExecError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.proc; p != nil {
		select {
		case <-p.done:
			s.proc = nil
		default:
			s.armIdle()
			return p, nil
		}
	}
	if s.crashes > 0 {
		wait := s.MinBackoff << min(s.crashes-1, 10)
		if wait > s.MaxBackoff {
			wait = s.MaxBackoff
		}
		if since := time.Since(s.lastExit); since < wait {
			return nil, &ExecError{Code: "upstream_connect_failed", Retryable: true, Msg: fmt.Sprintf("the process stopped %s ago; it is started again after %s", since.Round(time.Second), wait)}
		}
	}
	env, eerr := s.environment()
	if eerr != nil {
		return nil, eerr.(*ExecError)
	}
	cmd, err := s.startCommand(s.Target, env)
	if err != nil {
		return nil, &ExecError{Code: "upstream_connect_failed", Msg: err.Error()}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, &ExecError{Code: "upstream_connect_failed", Msg: err.Error()}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &ExecError{Code: "upstream_connect_failed", Msg: err.Error()}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, &ExecError{Code: "upstream_connect_failed", Msg: err.Error()}
	}
	if err := cmd.Start(); err != nil {
		s.noteExit()
		return nil, &ExecError{Code: "upstream_connect_failed", Msg: "the process did not start: " + err.Error(), Retryable: true}
	}
	p := &stdioProc{cmd: cmd, stdin: stdin, pending: map[int64]chan json.RawMessage{}, done: make(chan struct{})}
	go s.readStderr(stderr)
	go s.readLoop(p, stdout)
	go func() {
		_ = cmd.Wait()
		close(p.done)
		p.failAll()
		s.mu.Lock()
		if s.proc == p {
			s.proc = nil
			s.noteExit()
		}
		s.mu.Unlock()
	}()
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	init := map[string]any{"jsonrpc": "2.0", "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": BinaryName, "version": Version},
	}}
	res, xerr := s.roundTrip(hctx, p, init)
	if xerr != nil {
		s.kill(p)
		return nil, xerr
	}
	var env2 struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(res, &env2) != nil || len(env2.Result) == 0 {
		s.kill(p)
		return nil, &ExecError{Code: "upstream_connect_failed", Msg: "the server did not accept initialize"}
	}
	p.initRes = env2.Result
	_ = writeLine(p, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	s.proc = p
	s.crashes = 0
	s.armIdle()
	return p, nil
}

func (s *StdioSupervisor) noteExit() {
	s.crashes++
	s.lastExit = time.Now()
}

func (s *StdioSupervisor) armIdle() {
	d := time.Duration(s.Target.IdleTimeout)
	if d <= 0 {
		return
	}
	if s.idle != nil {
		s.idle.Stop()
	}
	s.idle = time.AfterFunc(d, s.stopIfIdle)
}

func (s *StdioSupervisor) stopIfIdle() {
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if p != nil && p.inFlight.Load() == 0 {
		s.Log.Event("info", "mcp_idle_stop", map[string]any{"target": s.Target.Name})
		s.mu.Lock()
		s.proc = nil // a stop on request is not a crash
		s.mu.Unlock()
		s.kill(p)
	}
}

func (s *StdioSupervisor) kill(p *stdioProc) {
	_ = p.stdin.Close()
	select {
	case <-p.done:
		return
	case <-time.After(s.Grace):
	}
	terminateProc(p.cmd)
	select {
	case <-p.done:
	case <-time.After(s.Grace):
		killProc(p.cmd)
	}
}

// Close stops the process.
func (s *StdioSupervisor) Close() {
	s.mu.Lock()
	p := s.proc
	s.proc = nil
	if s.idle != nil {
		s.idle.Stop()
	}
	s.mu.Unlock()
	if p != nil {
		s.kill(p)
	}
}

func (p *stdioProc) failAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, ch := range p.pending {
		close(ch)
		delete(p.pending, id)
	}
}

func writeLine(p *stdioProc, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err = p.stdin.Write(append(raw, '\n'))
	return err
}

func (s *StdioSupervisor) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if len(line) > 512 {
			line = line[:512] + "..."
		}
		s.Log.Event("info", "mcp_stderr", map[string]any{"target": s.Target.Name, "line": line})
	}
}

// readLoop correlates responses, answers requests the server makes of its
// client with -32601 and notes list_changed.
func (s *StdioSupervisor) readLoop(p *stdioProc, r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLimitedLine(br, 16<<20)
		if err != nil {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			_ = writeLine(p, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(m.ID), "error": map[string]any{"code": -32601, "message": "not available through the connector"}})
		case m.Method == "notifications/tools/list_changed":
			s.listChanged.Store(true)
		case m.Method != "":
		case len(m.ID) > 0:
			id, perr := strconv.ParseInt(string(m.ID), 10, 64)
			if perr != nil {
				continue
			}
			p.mu.Lock()
			ch, ok := p.pending[id]
			delete(p.pending, id)
			p.mu.Unlock()
			if ok {
				ch <- append(json.RawMessage(nil), line...)
				close(ch)
			}
		}
	}
}

func readLimitedLine(br *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > max {
			return nil, errors.New("line too long")
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return bytes.TrimSpace(out), err
		}
		return bytes.TrimSpace(out), nil
	}
}

// roundTrip sends a request with a local id and waits for its response.
func (s *StdioSupervisor) roundTrip(ctx context.Context, p *stdioProc, msg map[string]any) ([]byte, *ExecError) {
	id := s.nextID.Add(1)
	msg["id"] = id
	ch := make(chan json.RawMessage, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()
	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	if err := writeLine(p, msg); err != nil {
		return nil, &ExecError{Code: "upstream_reset", Dispatched: true, Msg: "the process closed its input"}
	}
	select {
	case res, ok := <-ch:
		if !ok {
			return nil, &ExecError{Code: "upstream_reset", Dispatched: true, Msg: "the process stopped before it answered"}
		}
		return res, nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &ExecError{Code: "upstream_timeout", Dispatched: true}
		}
		return nil, &ExecError{Code: "agent_shutdown", Dispatched: true}
	}
}

// Call serves one JSON-RPC request. The id is rewritten to a local counter
// and restored on the way back; initialize is answered from the session the
// agent opened itself. A notification is dropped.
func (s *StdioSupervisor) Call(ctx context.Context, req []byte, maxBytes int64) ([]byte, *ExecError) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(req, &m); err != nil {
		return nil, refuse("mcp_method_not_allowed", "the body is not a JSON-RPC object")
	}
	origID, hasID := m["id"]
	if !hasID {
		return nil, nil
	}
	timeout := time.Duration(s.Target.Timeout)
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p, xerr := s.ensure(ctx)
	if xerr != nil {
		return nil, xerr
	}
	var method string
	_ = json.Unmarshal(m["method"], &method)
	if method == "initialize" {
		return mustMarshal(map[string]any{"jsonrpc": "2.0", "id": origID, "result": json.RawMessage(p.initRes)}), nil
	}
	local := map[string]any{"jsonrpc": "2.0", "method": method}
	if params, ok := m["params"]; ok {
		local["params"] = params
	}
	res, xerr := s.roundTrip(ctx, p, local)
	if xerr != nil {
		return nil, xerr
	}
	if maxBytes > 0 && int64(len(res)) > maxBytes {
		return nil, &ExecError{Code: "response_too_large", Dispatched: true}
	}
	var back map[string]json.RawMessage
	if json.Unmarshal(res, &back) != nil {
		return nil, &ExecError{Code: "upstream_reset", Dispatched: true, Msg: "the server answered with something that is not JSON"}
	}
	back["id"] = origID
	return mustMarshal(back), nil
}

func mustMarshal(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}
