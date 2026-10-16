package tunnel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// bufferLimit is how much of a request body is held and verified before
	// the upstream is contacted.
	bufferLimit = 1 << 20
	// unsafeBufferLimit applies to targets that cannot take a body that might
	// be rejected afterwards.
	unsafeBufferLimit = 8 << 20
	responseChunk     = 32 << 10
	initialWindow     = 1 << 20
)

// window is the credit the peer has granted for DATA frames of one stream.
type window struct {
	mu     sync.Mutex
	cond   *sync.Cond
	n      int64
	closed bool
	wakes  int
}

func newWindow(n int64) *window {
	w := &window{n: n}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *window) add(n int64) {
	w.mu.Lock()
	w.n += n
	w.mu.Unlock()
	w.cond.Broadcast()
}

func (w *window) close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.cond.Broadcast()
}

func (w *window) acquire(ctx context.Context, n int64) error {
	stop := context.AfterFunc(ctx, func() {
		// Taking the lock orders the wake-up after a waiter that checked
		// the context and is about to wait.
		w.mu.Lock()
		w.wakes++
		w.mu.Unlock()
		w.cond.Broadcast()
	})
	defer stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.n < n {
		if w.closed {
			return errors.New("the stream is closed")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		w.cond.Wait()
	}
	w.n -= n
	return nil
}

// exchange is one request being served on a stream.
type exchange struct {
	a       *Agent
	s       *session
	id      uint32
	in      chan Frame
	ctx     context.Context
	cancel  context.CancelFunc
	win     *window
	started time.Time

	req      *Request
	reqID    atomic.Value
	target   Target
	bytesIn  int64
	bytesOut int64
	status   int
	upstream int64
	path     string
	spilled  bool
}

func (ex *exchange) sendFrame(f Frame) error {
	f.StreamID = ex.id
	return ex.s.send(f)
}

func (ex *exchange) sendData(payload []byte, flags byte) error {
	if err := ex.win.acquire(ex.ctx, int64(len(payload))); err != nil {
		return err
	}
	return ex.sendFrame(Frame{Type: FrameData, Flags: flags, Payload: payload})
}

type errorPayload struct {
	Code       string `json:"code"`
	Retryable  bool   `json:"retryable"`
	Dispatched bool   `json:"dispatched"`
	RequestID  string `json:"request_id,omitempty"`
	Message    string `json:"message,omitempty"`
}

func (ex *exchange) sendError(e *ExecError) {
	p := errorPayload{Code: e.Code, Retryable: e.Retryable, Dispatched: e.Dispatched, Message: e.Msg}
	if id, ok := ex.reqID.Load().(string); ok {
		p.RequestID = id
	}
	raw, _ := json.Marshal(p)
	_ = ex.sendFrame(Frame{Type: FrameError, Payload: raw})
}

// cryptoError maps a failed unseal or signature check to the ERROR code.
func cryptoError(err error) *ExecError {
	switch {
	case errors.Is(err, ErrSignature):
		return &ExecError{Code: "signature_invalid", Msg: err.Error()}
	case errors.Is(err, ErrReplay):
		return &ExecError{Code: "replay"}
	case errors.Is(err, ErrExpired):
		return &ExecError{Code: "expired"}
	default:
		return &ExecError{Code: "decrypt_failed", Msg: err.Error()}
	}
}

func isSecurityCode(code string) bool {
	switch code {
	case "signature_invalid", "replay", "expired", "decrypt_failed":
		return true
	}
	return false
}

// run serves the exchange to its end, then reports it.
func (ex *exchange) run(open Frame) {
	defer ex.finish()
	e := ex.handle(open)
	outcome := "ok"
	if e != nil {
		ex.sendError(e)
		outcome = outcomeOf(e)
	}
	ex.report(e, outcome)
}

func outcomeOf(e *ExecError) string {
	switch {
	case isSecurityCode(e.Code):
		return "protocol_error"
	case e.Violation:
		return "refused"
	case e.Code == "upstream_timeout":
		return "timeout"
	case e.Code == "agent_paused" || e.Code == "rate_limited_local" || e.Code == "target_unknown":
		return "refused"
	}
	return "upstream_error"
}

func (ex *exchange) finish() {
	ex.cancel()
	ex.win.close()
	ex.s.mu.Lock()
	delete(ex.s.streams, ex.id)
	ex.s.mu.Unlock()
	ex.a.inflight.Add(-1)
}

func (ex *exchange) report(e *ExecError, outcome string) {
	a := ex.a
	a.metrics.exchange(ex.target.Name, outcome, time.Since(ex.started), time.Duration(ex.upstream)*time.Millisecond)
	fields := map[string]any{
		"network": a.identity().NetworkID, "target": ex.target.Name, "kind": ex.target.Kind, "status": ex.status,
		"bytes_in": ex.bytesIn, "bytes_out": ex.bytesOut, "duration_ms": time.Since(ex.started).Milliseconds(),
		"upstream_ms": ex.upstream, "outcome": outcome,
	}
	level, event := "info", "exchange"
	if ex.req != nil {
		h := ex.req.Header
		fields["request_id"], fields["trace_id"], fields["audit_ref"] = h.RequestID, h.TraceID, h.AuditRef
		fields["org"], fields["method"], fields["path"] = h.OrgID, h.Method, ex.path
		fields["query_present"] = h.Query != ""
		fields["credentials"] = "gateway"
		if ex.target.Credentials != nil {
			fields["credentials"] = "agent"
		}
		fields["gateway_kid"] = ex.s.gatewayKID(ex.req)
	}
	if e != nil {
		fields["code"] = e.Code
		level = "warn"
		switch {
		case isSecurityCode(e.Code):
			level, event = "error", "security"
			a.metrics.security.Add(1)
		case e.Violation:
			event = "policy_violation"
			a.metrics.violations.Add(1)
			a.noteError(e.Code)
		default:
			a.noteError(e.Code)
		}
	}
	a.log.Event(level, event, fields)
	if e != nil && event != "exchange" {
		// The exchange line is the one the dashboard's metadata view wants.
		a.log.Event("info", "exchange", fields)
	}
}

// handle unseals, checks and serves the request. A non-nil result is sent as
// ERROR; after a response has started it means the response was cut.
func (ex *exchange) handle(open Frame) *ExecError {
	a := ex.a
	var env OpenEnvelope
	if err := json.Unmarshal(open.Payload, &env); err != nil {
		return cryptoError(fmt.Errorf("%w: OPEN payload", ErrDecrypt))
	}
	gw, err := a.gatewayKeys()
	if err != nil {
		return &ExecError{Code: "decrypt_failed", Msg: err.Error()}
	}
	now := a.now()
	req, err := OpenRequest(env, a.identity().ActiveKeys(now), gw)
	if err != nil {
		return cryptoError(err)
	}
	ex.req = req
	ex.reqID.Store(req.Header.RequestID)
	if err := a.checkGuard(req.Header, now); err != nil {
		return cryptoError(err)
	}
	h := req.Header
	if a.paused.Load() {
		return &ExecError{Code: "agent_paused"}
	}
	cfg := a.config()
	t, ok := cfg.Target(h.Target)
	if !ok || t.Kind != h.Kind {
		return refuse("target_unknown", "no such target on this agent")
	}
	ex.target = t
	if norm, err := NormalizePath(h.Path); err == nil {
		ex.path = norm
	}
	switch t.Kind {
	case KindHTTP:
		return ex.serveHTTP(t)
	case KindMCPHTTP:
		return ex.serveMCPHTTP(t)
	default:
		return ex.serveMCPStdio(t)
	}
}

// bodyReader pulls the request body out of the stream, checking each chunk and
// the signature over the whole stream at the end.
type bodyReader struct {
	ex   *exchange
	buf  []byte
	done bool
	err  error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	for len(b.buf) == 0 {
		if b.done {
			if b.err != nil {
				return 0, b.err
			}
			return 0, io.EOF
		}
		select {
		case <-b.ex.ctx.Done():
			b.done, b.err = true, b.ex.ctx.Err()
		case f := <-b.ex.in:
			switch f.Type {
			case FrameData:
				pt, err := b.ex.req.Chunk(f.Payload)
				_ = b.ex.sendFrame(windowFrame(len(f.Payload)))
				if err != nil {
					b.done, b.err = true, err
					break
				}
				b.ex.bytesIn += int64(len(pt))
				b.buf = pt
			case FrameEnd:
				b.done, b.err = true, b.ex.req.Final(f.Payload)
			}
		}
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

func windowFrame(credit int) Frame {
	raw, _ := json.Marshal(map[string]int{"credit_bytes": credit})
	return Frame{Type: FrameWindow, Payload: raw}
}

// collect reads up to limit bytes. complete is true when the stream ended
// inside the limit, which also means its signature was checked.
func (b *bodyReader) collect(limit int64) (data []byte, complete bool, err error) {
	var out bytes.Buffer
	chunk := make([]byte, 32<<10)
	for int64(out.Len()) < limit {
		n, rerr := b.Read(chunk)
		out.Write(chunk[:n])
		if errors.Is(rerr, io.EOF) {
			return out.Bytes(), true, nil
		}
		if rerr != nil {
			return nil, false, rerr
		}
	}
	return out.Bytes(), false, nil
}

func bodyError(err error) *ExecError {
	switch {
	case errors.Is(err, ErrSignature), errors.Is(err, ErrDecrypt):
		code := "decrypt_failed"
		if errors.Is(err, ErrSignature) {
			code = "signature_invalid"
		}
		return &ExecError{Code: code, Msg: err.Error()}
	case errors.Is(err, context.Canceled):
		return &ExecError{Code: "agent_shutdown"}
	case errors.Is(err, context.DeadlineExceeded):
		return &ExecError{Code: "upstream_timeout"}
	}
	return &ExecError{Code: "upstream_reset", Msg: err.Error()}
}

func hasBody(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	}
	return false
}

func (ex *exchange) serveHTTP(t Target) *ExecError {
	h := ex.req.Header
	body := &bodyReader{ex: ex}
	limit := int64(bufferLimit)
	if t.BufferUnsafeBodies && hasBody(h.Method) {
		limit = unsafeBufferLimit
	}
	pre, complete, err := body.collect(limit)
	if err != nil {
		return bodyError(err)
	}
	if !complete && t.BufferUnsafeBodies && hasBody(h.Method) {
		return &ExecError{Code: "body_too_large", Msg: "the target buffers bodies up to 8 MiB"}
	}
	var rd io.Reader = http.NoBody
	switch {
	case len(h.FileRefs) > 0:
		spool, ferr := ex.fetchFileRef(h.FileRefs)
		if ferr != nil {
			return ferr
		}
		defer os.Remove(spool.Name())
		defer spool.Close()
		rd = spool
	case complete && len(pre) > 0:
		rd = bytes.NewReader(pre)
	case !complete:
		rd = io.MultiReader(bytes.NewReader(pre), body)
	}
	if h.BodySHA256 != "" && complete {
		if sum := sha256.Sum256(pre); hex.EncodeToString(sum[:]) != strings.ToLower(h.BodySHA256) {
			return &ExecError{Code: "signature_invalid", Msg: "the body does not match its digest"}
		}
	}
	res, err := ex.a.httpx.Do(ex.ctx, t, h, rd)
	if err != nil {
		return ex.mapDoError(err)
	}
	defer res.Body.Close()
	ex.status, ex.upstream = res.Status, res.UpstreamMs
	return ex.respond(res)
}

func (ex *exchange) mapDoError(err error) *ExecError {
	var ee *ExecError
	if errors.As(err, &ee) {
		return ee
	}
	if errors.Is(err, ErrSignature) || errors.Is(err, ErrDecrypt) {
		return &ExecError{Code: "signature_invalid", Msg: err.Error(), Dispatched: true}
	}
	return &ExecError{Code: "upstream_reset", Msg: err.Error(), Dispatched: true}
}

func (ex *exchange) serveMCPHTTP(t Target) *ExecError {
	h := ex.req.Header
	pre, complete, err := (&bodyReader{ex: ex}).collect(bufferLimit)
	if err != nil {
		return bodyError(err)
	}
	if !complete {
		return &ExecError{Code: "body_too_large", Msg: "an MCP message is limited to 1 MiB"}
	}
	if strings.EqualFold(h.Method, http.MethodPost) {
		if _, xerr := CheckMCPBody(t, pre); xerr != nil {
			return xerr
		}
	}
	var rd io.Reader = http.NoBody
	if len(pre) > 0 {
		rd = bytes.NewReader(pre)
	}
	res, err := ex.a.httpx.Do(ex.ctx, t, h, rd)
	if err != nil {
		return ex.mapDoError(err)
	}
	defer res.Body.Close()
	ex.status, ex.upstream = res.Status, res.UpstreamMs
	return ex.respond(res)
}

func (ex *exchange) serveMCPStdio(t Target) *ExecError {
	pre, complete, err := (&bodyReader{ex: ex}).collect(bufferLimit)
	if err != nil {
		return bodyError(err)
	}
	if !complete {
		return &ExecError{Code: "body_too_large", Msg: "an MCP message is limited to 1 MiB"}
	}
	if _, xerr := CheckMCPBody(t, pre); xerr != nil {
		return xerr
	}
	sup := ex.a.supervisor(t)
	started := time.Now()
	out, xerr := sup.Call(ex.ctx, pre, ex.req.Header.MaxResponseBytes)
	ex.upstream = time.Since(started).Milliseconds()
	if xerr != nil {
		return xerr
	}
	res := &Result{Status: http.StatusAccepted, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil)), UpstreamMs: ex.upstream}
	if out != nil {
		res = &Result{Status: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(out)), ContentLength: int64(len(out)), UpstreamMs: ex.upstream}
	}
	ex.status = res.Status
	return ex.respond(res)
}

// respond seals the upstream response and streams it back: a header, chunks,
// and a signed trailer. A response above the cap the gateway set is cut with
// response_too_large.
func (ex *exchange) respond(res *Result) *ExecError {
	h := ex.req.Header
	rs, err := NewResponseSealer(h.RespPub, h.RequestID, ex.a.identity().Current())
	if err != nil {
		return cryptoError(err)
	}
	var spilled *SpilledFile
	reader := res.Reader()
	if h.Spill != nil && res.ContentLength > ex.a.config().SpillThreshold && res.ContentLength <= h.Spill.MaxBytes {
		sf, serr := ex.spill(h.Spill, res, reader)
		switch {
		case serr == nil:
			spilled, reader, ex.spilled = sf, http.NoBody, true
		case serr.Code == "spill_unavailable":
			ex.a.spillUnavailable.Store(true)
		default:
			return serr
		}
	}
	hdr := http.Header{}
	for k, v := range res.Header {
		hdr[k] = v
	}
	payload, err := rs.Header(ResponseHeader{Status: res.Status, Headers: hdr, Spilled: spilled})
	if err != nil {
		return &ExecError{Code: "decrypt_failed", Msg: err.Error()}
	}
	if err := ex.sendData(payload, FlagHeader); err != nil {
		return &ExecError{Code: "agent_shutdown", Dispatched: true}
	}
	var total int64
	buf := make([]byte, responseChunk)
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			total += int64(n)
			if h.MaxResponseBytes > 0 && total > h.MaxResponseBytes {
				return &ExecError{Code: "response_too_large", Dispatched: true}
			}
			if err := ex.sendData(rs.Chunk(buf[:n]), 0); err != nil {
				return &ExecError{Code: "agent_shutdown", Dispatched: true}
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			if errors.Is(rerr, context.DeadlineExceeded) {
				return &ExecError{Code: "upstream_timeout", Dispatched: true}
			}
			return &ExecError{Code: "upstream_reset", Dispatched: true, Msg: "the upstream response broke"}
		}
	}
	ex.bytesOut = total
	end, err := rs.End(ResponseTrailer{UpstreamMs: res.UpstreamMs, AgentMs: time.Since(ex.started).Milliseconds() - res.UpstreamMs, Bytes: total})
	if err != nil {
		return &ExecError{Code: "decrypt_failed", Msg: err.Error()}
	}
	if err := ex.sendFrame(Frame{Type: FrameEnd, Payload: end}); err != nil {
		return &ExecError{Code: "agent_shutdown", Dispatched: true}
	}
	return nil
}

// spill streams a large response straight to the file store the gateway named
// and returns what was stored. A connection that cannot be made is reported
// as spill_unavailable so the response can go through the tunnel instead.
func (ex *exchange) spill(sp *Spill, res *Result, body io.Reader) (*SpilledFile, *ExecError) {
	u, err := url.Parse(sp.UploadURL)
	if err != nil || !secureURL(u) {
		return nil, &ExecError{Code: "spill_unavailable", Msg: "the upload address is not https"}
	}
	if sp.ExpiresAt > 0 && time.Now().UnixMilli() > sp.ExpiresAt {
		return nil, &ExecError{Code: "spill_unavailable", Msg: "the upload address expired"}
	}
	h := sha256.New()
	counted := &countReader{r: io.TeeReader(io.LimitReader(body, sp.MaxBytes+1), h)}
	req, err := http.NewRequestWithContext(ex.ctx, http.MethodPut, sp.UploadURL, counted)
	if err != nil {
		return nil, &ExecError{Code: "spill_unavailable", Msg: err.Error()}
	}
	req.ContentLength = res.ContentLength
	if res.ContentType != "" {
		req.Header.Set("Content-Type", res.ContentType)
	}
	resp, err := ex.a.spillClient().Do(req)
	if err != nil {
		var ne net.Error
		if counted.n == 0 && (errors.As(err, &ne) || strings.Contains(err.Error(), "connect")) {
			return nil, &ExecError{Code: "spill_unavailable", Msg: "the file store could not be reached from this network"}
		}
		return nil, &ExecError{Code: "upstream_reset", Dispatched: true, Msg: "the upload broke"}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &ExecError{Code: "upstream_reset", Dispatched: true, Msg: fmt.Sprintf("the file store answered %d", resp.StatusCode)}
	}
	if counted.n != res.ContentLength {
		return nil, &ExecError{Code: "upstream_reset", Dispatched: true, Msg: "the upload was cut short"}
	}
	return &SpilledFile{FileID: sp.FileID, Size: counted.n, SHA256: hex.EncodeToString(h.Sum(nil)), ContentType: res.ContentType}, nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// fetchFileRef downloads the file the gateway pointed at into a temporary file
// and checks its size and SHA-256 before anything is sent upstream.
func (ex *exchange) fetchFileRef(refs []FileRef) (*os.File, *ExecError) {
	if len(refs) != 1 || refs[0].Param != "body" {
		return nil, &ExecError{Code: "file_ref_unsupported", Msg: "only a single file as the request body is supported"}
	}
	ref := refs[0]
	u, err := url.Parse(ref.URL)
	if err != nil || !secureURL(u) {
		return nil, &ExecError{Code: "file_fetch_failed", Msg: "the file address is not https"}
	}
	req, err := http.NewRequestWithContext(ex.ctx, http.MethodGet, ref.URL, nil)
	if err != nil {
		return nil, &ExecError{Code: "file_fetch_failed", Msg: err.Error()}
	}
	resp, err := ex.a.spillClient().Do(req)
	if err != nil {
		return nil, &ExecError{Code: "file_fetch_failed", Msg: "the file store could not be reached", Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &ExecError{Code: "file_fetch_failed", Msg: fmt.Sprintf("the file store answered %d", resp.StatusCode)}
	}
	dir := ex.a.stateDir()
	_ = os.MkdirAll(filepath.Join(dir, "tmp"), 0o700)
	f, err := os.CreateTemp(filepath.Join(dir, "tmp"), "fileref-*")
	if err != nil {
		return nil, &ExecError{Code: "file_fetch_failed", Msg: err.Error()}
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, ref.Size+1))
	if err != nil || n != ref.Size || hex.EncodeToString(h.Sum(nil)) != strings.ToLower(ref.SHA256) {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, &ExecError{Code: "signature_invalid", Msg: "the file does not match the digest the gateway signed"}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, &ExecError{Code: "file_fetch_failed", Msg: err.Error()}
	}
	return f, nil
}
