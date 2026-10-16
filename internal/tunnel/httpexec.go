package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ExecError is a refusal or failure the agent reports with an ERROR frame.
type ExecError struct {
	Code       string
	Retryable  bool
	Dispatched bool
	Msg        string
	// Violation marks a request the local allowlist refused.
	Violation bool
}

func (e *ExecError) Error() string {
	if e.Msg != "" {
		return e.Code + ": " + e.Msg
	}
	return e.Code
}

func refuse(code, msg string) *ExecError {
	return &ExecError{Code: code, Msg: msg, Violation: true}
}

// Resolver looks up host names; tests replace it to stage DNS rebinding.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// blockedRanges are never reachable unless a target lists them explicitly:
// cloud metadata, link-local and similar.
var blockedRanges = mustPrefixes(
	"169.254.0.0/16", "fe80::/10", "fd00:ec2::254/128", "100.100.100.200/32", "192.0.0.192/32", "0.0.0.0/8", "::/128",
)

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}

var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Trailers": true, "Transfer-Encoding": true, "Upgrade": true,
	"Proxy-Connection": true, "Host": true, "Content-Length": true,
}

type targetKey struct{}

// HTTPExecutor runs requests against HTTP targets.
type HTTPExecutor struct {
	Resolver Resolver
	Now      func() time.Time

	mu         sync.Mutex
	transports map[string]*http.Transport
	buckets    map[string]*bucket
}

// NewHTTPExecutor uses the system resolver.
func NewHTTPExecutor() *HTTPExecutor {
	return &HTTPExecutor{Resolver: net.DefaultResolver, Now: time.Now, transports: map[string]*http.Transport{}, buckets: map[string]*bucket{}}
}

// Close drops idle connections.
func (x *HTTPExecutor) Close() {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, t := range x.transports {
		t.CloseIdleConnections()
	}
}

// Result is an upstream response, not yet sealed.
type Result struct {
	Status        int
	Header        http.Header
	Body          io.ReadCloser
	ContentLength int64
	ContentType   string
	UpstreamMs    int64
	scrub         [][]byte
}

// Reader returns the body with the injected credentials scrubbed out.
func (r *Result) Reader() io.Reader {
	if len(r.scrub) == 0 {
		return r.Body
	}
	return newScrubber(r.Body, r.scrub)
}

// glob compiles a path pattern: ** crosses segments, * and ? stay inside one,
// and a trailing /** also matches the directory itself.
func glob(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	trailing := strings.HasSuffix(pattern, "/**")
	if trailing {
		pattern = strings.TrimSuffix(pattern, "/**")
	}
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if trailing {
		b.WriteString("(/.*)?")
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// MatchPath reports whether p matches any of the patterns.
func MatchPath(patterns []string, p string) bool {
	for _, g := range patterns {
		if glob(g).MatchString(p) {
			return true
		}
	}
	return false
}

// NormalizePath resolves the path the gateway sent before it is matched: an
// encoded slash is refused, other escapes are decoded, dot segments are
// resolved and duplicate slashes collapse.
func NormalizePath(raw string) (string, error) {
	if strings.Contains(strings.ToLower(raw), "%2f") || strings.Contains(strings.ToLower(raw), "%5c") {
		return "", errors.New("an encoded slash in the path")
	}
	dec, err := url.PathUnescape(raw)
	if err != nil {
		return "", errors.New("the path has a bad escape")
	}
	for _, r := range dec {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", errors.New("the path has a control character or backslash")
		}
	}
	if !strings.HasPrefix(dec, "/") {
		dec = "/" + dec
	}
	clean := path.Clean(dec)
	if strings.HasSuffix(dec, "/") && clean != "/" {
		clean += "/"
	}
	return clean, nil
}

func ipBlocked(ip netip.Addr, t Target) (bool, error) {
	ip = ip.Unmap()
	listed := false
	for _, c := range t.CIDRs {
		if p, err := netip.ParsePrefix(c); err == nil && p.Contains(ip) {
			listed = true
		}
	}
	if len(t.CIDRs) > 0 && !listed {
		return true, fmt.Errorf("%s is outside the target's cidrs", ip)
	}
	if ip.IsLoopback() && !t.AllowLoopback {
		return true, fmt.Errorf("%s is a loopback address; set allow_loopback to reach it", ip)
	}
	for _, p := range blockedRanges {
		if p.Contains(ip) && !listed {
			return true, fmt.Errorf("%s is in a range that is refused unless the target lists it (cloud metadata and link-local)", ip)
		}
	}
	if ip.IsUnspecified() && !listed {
		return true, fmt.Errorf("%s is not an address of a host", ip)
	}
	return false, nil
}

// dialer resolves once, checks every address against the target and connects
// to the checked address, so a name that changes its answer cannot move the
// connection.
func (x *HTTPExecutor) dialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		t, _ := ctx.Value(targetKey{}).(Target)
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, &ExecError{Code: "upstream_connect_failed", Msg: err.Error()}
		}
		var ips []netip.Addr
		if ip, perr := netip.ParseAddr(host); perr == nil {
			ips = []netip.Addr{ip}
		} else {
			addrs, lerr := x.Resolver.LookupIPAddr(ctx, host)
			if lerr != nil || len(addrs) == 0 {
				return nil, &ExecError{Code: "dns_failed", Msg: fmt.Sprintf("%s did not resolve", host)}
			}
			for _, a := range addrs {
				if ip, ok := netip.AddrFromSlice(a.IP); ok {
					ips = append(ips, ip)
				}
			}
		}
		for _, ip := range ips {
			if blocked, why := ipBlocked(ip, t); blocked {
				return nil, &ExecError{Code: "address_not_allowed", Msg: why.Error(), Violation: true}
			}
		}
		var last error
		for _, ip := range ips {
			c, derr := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
			if derr == nil {
				return c, nil
			}
			last = derr
		}
		return nil, &ExecError{Code: "upstream_connect_failed", Msg: last.Error()}
	}
}

func (x *HTTPExecutor) transport(t Target) (*http.Transport, error) {
	key := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%v|%v", t.Name, t.URL, t.TLS.CAFile, t.TLS.ServerName, t.TLS.ClientCert, t.TLS.ClientKey, t.TLS.InsecureSkipVerify, t.AllowInsecure)
	x.mu.Lock()
	defer x.mu.Unlock()
	if tr, ok := x.transports[key]; ok {
		return tr, nil
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: t.TLS.ServerName}
	if t.TLS.InsecureSkipVerify && t.AllowInsecure {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // opted in by the operator with allow_insecure
	}
	if t.TLS.CAFile != "" {
		pem, err := os.ReadFile(t.TLS.CAFile)
		if err != nil {
			return nil, &ExecError{Code: "upstream_connect_failed", Msg: "tls.ca_file: " + err.Error()}
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, &ExecError{Code: "upstream_connect_failed", Msg: "tls.ca_file holds no certificate"}
		}
		tlsCfg.RootCAs = pool
	}
	if t.TLS.ClientCert != "" {
		cert, err := tls.LoadX509KeyPair(t.TLS.ClientCert, t.TLS.ClientKey)
		if err != nil {
			return nil, &ExecError{Code: "upstream_connect_failed", Msg: "tls client certificate: " + err.Error()}
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	tr := &http.Transport{
		DialContext:           x.dialer(),
		TLSClientConfig:       tlsCfg,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0,
		DisableCompression:    true,
	}
	x.transports[key] = tr
	return tr, nil
}

type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	rate   Rate
}

func (b *bucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens += now.Sub(b.last).Seconds() * b.rate.PerSecond
	if b.tokens > float64(b.rate.Burst) {
		b.tokens = float64(b.rate.Burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (x *HTTPExecutor) rateAllowed(t Target) bool {
	if t.Rate == nil {
		return true
	}
	x.mu.Lock()
	b, ok := x.buckets[t.Name]
	if !ok || b.rate != *t.Rate {
		b = &bucket{tokens: float64(t.Rate.Burst), last: x.Now(), rate: *t.Rate}
		x.buckets[t.Name] = b
	}
	x.mu.Unlock()
	return b.allow(x.Now())
}

// injectCredentials sets the target's own credentials, replacing any header of
// the same name the gateway sent, and returns the secret values to scrub.
func injectCredentials(h http.Header, c *Credentials) ([][]byte, error) {
	if c == nil {
		return nil, nil
	}
	var secrets [][]byte
	resolve := func(s string) (string, error) {
		v, err := ResolveSecret(s)
		if err != nil {
			return "", &ExecError{Code: "credentials_missing", Msg: err.Error()}
		}
		return v, nil
	}
	switch {
	case c.Header != "":
		v, err := resolve(c.Value)
		if err != nil {
			return nil, err
		}
		h.Set(c.Header, v)
		secrets = append(secrets, []byte(v))
		// The part after a scheme word is what must not come back.
		if _, rest, ok := strings.Cut(v, " "); ok && rest != "" {
			secrets = append(secrets, []byte(rest))
		}
	case c.Bearer != "":
		v, err := resolve(c.Bearer)
		if err != nil {
			return nil, err
		}
		h.Set("Authorization", "Bearer "+v)
		secrets = append(secrets, []byte(v))
	case c.Basic != nil:
		u, err := resolve(c.Basic.Username)
		if err != nil {
			return nil, err
		}
		p, err := resolve(c.Basic.Password)
		if err != nil {
			return nil, err
		}
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u+":"+p)))
		secrets = append(secrets, []byte(p))
	}
	var out [][]byte
	for _, s := range secrets {
		if len(s) >= 4 {
			out = append(out, s)
		}
	}
	return out, nil
}

// Do runs one request against an http target and returns the response head
// with the body still open. Everything the local allowlist can refuse is
// refused here, before the network is touched.
func (x *HTTPExecutor) Do(ctx context.Context, t Target, h RequestHeader, body io.Reader) (*Result, error) {
	if t.Kind != KindHTTP && t.Kind != KindMCPHTTP {
		return nil, refuse("target_unknown", "the target is not an HTTP target")
	}
	method := strings.ToUpper(h.Method)
	if t.Kind == KindMCPHTTP {
		if method != http.MethodPost && method != http.MethodDelete {
			return nil, refuse("method_not_allowed", method+" is not served by an MCP target")
		}
	} else if !contains(t.Methods, method) {
		return nil, refuse("method_not_allowed", method+" is not in the target's methods")
	}
	rel, err := NormalizePath(h.Path)
	if err != nil {
		return nil, refuse("path_not_allowed", err.Error())
	}
	if t.Kind == KindMCPHTTP {
		if rel != "/" {
			return nil, refuse("path_not_allowed", "an MCP target is reached at its own address only")
		}
	} else if !MatchPath(t.Paths, rel) {
		return nil, refuse("path_not_allowed", "the path is not in the target's paths")
	}
	if !x.rateAllowed(t) {
		return nil, &ExecError{Code: "rate_limited_local", Retryable: true}
	}
	base, err := url.Parse(t.URL)
	if err != nil {
		return nil, refuse("target_unknown", "the target's url is not valid")
	}
	full := strings.TrimRight(base.Path, "/") + rel
	if t.Kind == KindMCPHTTP {
		full = base.Path
	}
	target := url.URL{Scheme: base.Scheme, Host: base.Host, Path: full, RawQuery: h.Query}

	hdr := http.Header{}
	strip := map[string]bool{}
	for _, s := range t.StripRequestHeaders {
		strip[http.CanonicalHeaderKey(s)] = true
	}
	for k, vs := range h.Headers {
		ck := http.CanonicalHeaderKey(k)
		if hopByHop[ck] || strip[ck] {
			continue
		}
		hdr[ck] = append([]string(nil), vs...)
	}
	if h.CredentialsMode == "agent" && t.Credentials == nil {
		return nil, &ExecError{Code: "credentials_missing", Msg: "the request asks for agent-held credentials and the target has none"}
	}
	secrets, err := injectCredentials(hdr, t.Credentials)
	if err != nil {
		return nil, err
	}
	scrub := secrets
	if t.ScrubCredentialsInResponse != nil && !*t.ScrubCredentialsInResponse {
		scrub = nil
	}

	tr, err := x.transport(t)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(t.Timeout)
	if left := time.Until(time.UnixMilli(h.DeadlineUnixMs)); h.DeadlineUnixMs > 0 && left < timeout {
		timeout = left
	}
	if timeout > 300*time.Second {
		timeout = 300 * time.Second
	}
	if timeout <= 0 {
		return nil, &ExecError{Code: "upstream_timeout", Msg: "the deadline had passed"}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)

	var dispatched bool
	// The dialer reads the target from the context, so a pooled connection is
	// always checked against the target that asked for it.
	ctx = context.WithValue(ctx, targetKey{}, t)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: func() { dispatched = true }})
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		cancel()
		return nil, refuse("path_not_allowed", err.Error())
	}
	req.Header = hdr
	client := &http.Client{Transport: tr, CheckRedirect: x.redirectPolicy(t, base)}
	started := x.Now()
	resp, err := client.Do(req)
	if err != nil {
		classified := classifyTransportError(ctx, err, dispatched)
		cancel()
		return nil, classified
	}
	rh := http.Header{}
	stripResp := map[string]bool{}
	for _, s := range t.StripResponseHeaders {
		stripResp[http.CanonicalHeaderKey(s)] = true
	}
	for k, vs := range resp.Header {
		if hopByHop[k] || stripResp[k] {
			continue
		}
		rh[k] = vs
	}
	return &Result{
		Status: resp.StatusCode, Header: rh, ContentLength: resp.ContentLength, ContentType: resp.Header.Get("Content-Type"),
		Body: &cancelCloser{ReadCloser: resp.Body, cancel: cancel}, UpstreamMs: x.Now().Sub(started).Milliseconds(), scrub: scrub,
	}, nil
}

type cancelCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelCloser) Close() error { err := c.ReadCloser.Close(); c.cancel(); return err }

func classifyTransportError(ctx context.Context, err error, dispatched bool) error {
	var ee *ExecError
	if errors.As(err, &ee) {
		return ee
	}
	// The request body is the stream from the gateway: a failed check of it
	// surfaces here as the transport's error.
	if errors.Is(err, ErrSignature) {
		return &ExecError{Code: "signature_invalid", Dispatched: dispatched, Msg: "the request body did not verify"}
	}
	if errors.Is(err, ErrDecrypt) {
		return &ExecError{Code: "decrypt_failed", Dispatched: dispatched, Msg: "the request body could not be opened"}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return &ExecError{Code: "upstream_timeout", Dispatched: dispatched, Retryable: !dispatched}
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return &ExecError{Code: "agent_shutdown", Dispatched: dispatched}
	}
	if dispatched {
		return &ExecError{Code: "upstream_reset", Dispatched: true, Msg: "the connection broke after the request was sent"}
	}
	return &ExecError{Code: "upstream_connect_failed", Msg: shortErr(err), Retryable: true}
}

func shortErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func (x *HTTPExecutor) redirectPolicy(t Target, base *url.URL) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !t.FollowSameOriginRedirects || len(via) >= 5 {
			return http.ErrUseLastResponse
		}
		first := via[0].URL
		if req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
			return http.ErrUseLastResponse
		}
		rel := strings.TrimPrefix(req.URL.Path, strings.TrimRight(base.Path, "/"))
		norm, err := NormalizePath(rel)
		if err != nil || !MatchPath(t.Paths, norm) {
			return http.ErrUseLastResponse
		}
		return nil
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// scrubber replaces secret values in a stream with [redacted], including ones
// split across reads.
type scrubber struct {
	r       io.Reader
	secrets [][]byte
	hold    int
	pending []byte
	out     []byte
	eof     bool
	err     error
}

func newScrubber(r io.Reader, secrets [][]byte) *scrubber {
	hold := 0
	for _, s := range secrets {
		if len(s)-1 > hold {
			hold = len(s) - 1
		}
	}
	return &scrubber{r: r, secrets: secrets, hold: hold}
}

func (s *scrubber) replace(b []byte) []byte {
	for _, sec := range s.secrets {
		b = bytes.ReplaceAll(b, sec, []byte("[redacted]"))
	}
	return b
}

func (s *scrubber) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.eof {
			if s.err != nil && !errors.Is(s.err, io.EOF) {
				return 0, s.err
			}
			return 0, io.EOF
		}
		buf := make([]byte, 32<<10)
		n, err := s.r.Read(buf)
		s.pending = s.replace(append(s.pending, buf[:n]...))
		if err != nil {
			s.eof, s.err = true, err
			s.out, s.pending = s.pending, nil
			continue
		}
		// The last hold bytes may be the start of a secret that the next read
		// completes, so they wait.
		if len(s.pending) > s.hold {
			cut := len(s.pending) - s.hold
			s.out = s.pending[:cut:cut]
			s.pending = append([]byte(nil), s.pending[cut:]...)
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

// CheckMCPBody validates a JSON-RPC body against an MCP target: one message
// (never a batch), a method the target serves and, for tools/call, a tool the
// local allowlist names.
func CheckMCPBody(t Target, body []byte) (method string, err *ExecError) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] == '[' {
		return "", refuse("mcp_method_not_allowed", "a batch or empty body is not served")
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(trimmed, &msg) != nil || msg.Method == "" {
		return "", refuse("mcp_method_not_allowed", "the body is not a JSON-RPC request")
	}
	// The session notifications of a client are not requests: they carry no
	// id and do nothing a target could leak through.
	if len(msg.ID) == 0 && strings.HasPrefix(msg.Method, "notifications/") {
		return msg.Method, nil
	}
	if !contains(t.MCP.Methods, msg.Method) {
		return msg.Method, refuse("mcp_method_not_allowed", msg.Method+" is not in the target's mcp.methods")
	}
	if msg.Method == "tools/call" && len(t.MCP.Tools) > 0 {
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		ok := false
		for _, g := range t.MCP.Tools {
			if m, _ := path.Match(g, p.Name); m {
				ok = true
			}
		}
		if !ok {
			return msg.Method, refuse("mcp_tool_not_allowed", "the tool is not in the target's mcp.tools")
		}
	}
	return msg.Method, nil
}
