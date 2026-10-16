package tunnel

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

func httpTarget(url string, mod func(*Target)) Target {
	t := Target{Name: "erp", Kind: KindHTTP, URL: url, Methods: []string{"GET", "POST"}, Paths: []string{"/invoices/**", "/health"}, AllowLoopback: true, Timeout: Duration(5 * time.Second)}
	if mod != nil {
		mod(&t)
	}
	return t
}

func do(t *testing.T, x *HTTPExecutor, tg Target, h RequestHeader, body io.Reader) (*Result, *ExecError) {
	t.Helper()
	h = baseHeader(h)
	res, err := x.Do(context.Background(), tg, h, body)
	if err != nil {
		var ee *ExecError
		if !errors.As(err, &ee) {
			t.Fatalf("not an ExecError: %v", err)
		}
		return nil, ee
	}
	return res, nil
}

func readAll(t *testing.T, r *Result) string {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Reader())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNormalizePathAndGlobs(t *testing.T) {
	for in, want := range map[string]string{
		"/invoices/1":        "/invoices/1",
		"//invoices//1":      "/invoices/1",
		"/invoices/./1":      "/invoices/1",
		"/invoices/../admin": "/admin",
		"/../../etc/passwd":  "/etc/passwd",
		"/invoices/%41":      "/invoices/A",
		"/a/%2e%2e/b":        "/b",
		"/dir/":              "/dir/",
		"relative":           "/relative",
		"":                   "/",
	} {
		got, err := NormalizePath(in)
		if err != nil || got != want {
			t.Errorf("NormalizePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/a%2fb", "/a%2Fb", "/a%5cb", "/a\\b", "/a\x00b", "/a%zz", "/a\nb"} {
		if _, err := NormalizePath(bad); err == nil {
			t.Errorf("NormalizePath(%q) accepted", bad)
		}
	}
	patterns := []string{"/invoices/**", "/health", "/files/*.pdf", "/v?/ping"}
	for p, want := range map[string]bool{
		"/invoices": true, "/invoices/": true, "/invoices/1/lines/2": true, "/invoicesX": false,
		"/health": true, "/health/x": false, "/files/a.pdf": true, "/files/a/b.pdf": false, "/files/a.txt": false,
		"/v1/ping": true, "/v12/ping": false, "/admin": false,
	} {
		if got := MatchPath(patterns, p); got != want {
			t.Errorf("MatchPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestHTTPExecForwardsWithinTheAllowlist(t *testing.T) {
	var seen http.Header
	var seenPath, seenQuery, seenHost, seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, seenPath, seenQuery, seenHost = r.Header.Clone(), r.URL.Path, r.URL.RawQuery, r.Host
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("X-Internal", "1")
		w.Header().Set("X-Strip", "me")
		w.Header().Set("Connection", "close")
		w.WriteHeader(201)
		_, _ = io.WriteString(w, "created")
	}))
	defer srv.Close()
	secretFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(secretFile, []byte("agent-held-token"), 0o600)
	tg := httpTarget(srv.URL+"/api/v2", func(tg *Target) {
		tg.Credentials = &Credentials{Header: "Authorization", Value: "Bearer ${file:" + secretFile + "}"}
		tg.StripRequestHeaders = []string{"x-drop"}
		tg.StripResponseHeaders = []string{"x-strip"}
	})
	x := NewHTTPExecutor()
	defer x.Close()
	res, err := do(t, x, tg, RequestHeader{Method: "post", Path: "/invoices//1/../2", Query: "a=1&b=2", Headers: map[string][]string{
		"Authorization": {"Bearer from-gateway"}, "X-Drop": {"x"}, "X-Keep": {"k"}, "Host": {"evil.example"}, "Connection": {"keep-alive"}, "Content-Type": {"application/json"},
	}}, strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, res); got != "created" || res.Status != 201 {
		t.Errorf("response = %d %q", res.Status, got)
	}
	if seenPath != "/api/v2/invoices/2" || seenQuery != "a=1&b=2" || seenBody != `{"a":1}` {
		t.Errorf("upstream saw %s?%s %q", seenPath, seenQuery, seenBody)
	}
	if seen.Get("Authorization") != "Bearer agent-held-token" || seen.Get("X-Drop") != "" || seen.Get("X-Keep") != "k" || seenHost == "evil.example" {
		t.Errorf("headers = %v, host %q", seen, seenHost)
	}
	if res.Header.Get("X-Internal") != "1" || res.Header.Get("X-Strip") != "" || res.Header.Get("Connection") != "" {
		t.Errorf("response headers = %v", res.Header)
	}
}

func TestHTTPExecRefusesWhatTheAllowlistDoesNot(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	x := NewHTTPExecutor()
	tg := httpTarget(srv.URL, nil)
	for name, h := range map[string]RequestHeader{
		"method":           {Method: "DELETE", Path: "/invoices/1"},
		"path":             {Method: "GET", Path: "/admin"},
		"traversal":        {Method: "GET", Path: "/invoices/../admin"},
		"encoded slash":    {Method: "GET", Path: "/invoices/..%2fadmin"},
		"control char":     {Method: "GET", Path: "/invoices/\x01"},
		"agent creds none": {Method: "GET", Path: "/health", CredentialsMode: "agent"},
	} {
		_, err := do(t, x, tg, h, nil)
		if err == nil || err.Dispatched {
			t.Errorf("%s: %v", name, err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("%d refused requests reached the upstream", hits.Load())
	}
	_, e := do(t, x, tg, RequestHeader{Method: "GET", Path: "/admin"}, nil)
	if e.Code != "path_not_allowed" || !e.Violation {
		t.Errorf("code = %+v", e)
	}
	_, e = do(t, x, tg, RequestHeader{Method: "PUT", Path: "/health"}, nil)
	if e.Code != "method_not_allowed" {
		t.Errorf("code = %+v", e)
	}
	_, e = do(t, x, tg, RequestHeader{Method: "GET", Path: "/health", CredentialsMode: "agent"}, nil)
	if e.Code != "credentials_missing" {
		t.Errorf("code = %+v", e)
	}
	stdio := Target{Name: "s", Kind: KindMCPStdio}
	if _, e = do(t, x, stdio, RequestHeader{Method: "GET", Path: "/"}, nil); e.Code != "target_unknown" {
		t.Errorf("stdio target on the HTTP path: %+v", e)
	}
	missingSecret := httpTarget(srv.URL, func(tg *Target) {
		tg.Credentials = &Credentials{Bearer: "${file:/nonexistent/secret}"}
	})
	if _, e = do(t, x, missingSecret, RequestHeader{Method: "GET", Path: "/health"}, nil); e.Code != "credentials_missing" {
		t.Errorf("unreadable secret: %+v", e)
	}
}

func TestHTTPExecCredentialsKinds(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	x := NewHTTPExecutor()
	for name, c := range map[string]*Credentials{
		"bearer": {Bearer: "tok-123456"},
		"basic":  {Basic: &BasicAuth{"svc", "pw-123456"}},
	} {
		tg := httpTarget(srv.URL, func(tg *Target) { tg.Credentials = c })
		res, err := do(t, x, tg, RequestHeader{Method: "GET", Path: "/health", Headers: map[string][]string{"Authorization": {"gw"}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, res)
		if name == "bearer" && auth != "Bearer tok-123456" || name == "basic" && !strings.HasPrefix(auth, "Basic ") {
			t.Errorf("%s: Authorization = %q", name, auth)
		}
	}
}

func TestHTTPExecRefusesAddressesOutsideTheTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	x := NewHTTPExecutor()
	get := func(tg Target) *ExecError {
		_, err := do(t, x, tg, RequestHeader{Method: "GET", Path: "/health"}, nil)
		return err
	}
	// Loopback is refused unless the target allows it.
	if e := get(httpTarget(srv.URL, func(tg *Target) { tg.AllowLoopback = false })); e == nil || e.Code != "address_not_allowed" {
		t.Errorf("loopback: %v", e)
	}
	// An address outside the target's cidrs is refused.
	if e := get(httpTarget(srv.URL, func(tg *Target) { tg.CIDRs = []string{"10.0.0.0/8"} })); e == nil || e.Code != "address_not_allowed" {
		t.Errorf("outside cidrs: %v", e)
	}
	if e := get(httpTarget(srv.URL, func(tg *Target) { tg.CIDRs = []string{"127.0.0.0/8"} })); e != nil {
		t.Errorf("inside cidrs: %v", e)
	}
	// Cloud metadata is refused even without cidrs, and allowed only when listed.
	meta := httpTarget("http://169.254.169.254:"+itoa(port), func(tg *Target) { tg.Timeout = Duration(300 * time.Millisecond) })
	if e := get(meta); e == nil || e.Code != "address_not_allowed" {
		t.Errorf("metadata: %v", e)
	}
	listed := httpTarget("http://169.254.169.254:"+itoa(port), func(tg *Target) {
		tg.Timeout = Duration(300 * time.Millisecond)
		tg.CIDRs = []string{"169.254.169.254/32"}
	})
	if e := get(listed); e == nil || e.Code == "address_not_allowed" {
		t.Errorf("a listed metadata address must be tried: %v", e)
	}
	for _, ip := range []string{"[fd00:ec2::254]", "100.100.100.200", "[fe80::1]", "0.0.0.0"} {
		if e := get(httpTarget("http://"+ip+":"+itoa(port), nil)); e == nil || e.Code != "address_not_allowed" {
			t.Errorf("%s: %v", ip, e)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

type fakeResolver struct {
	answers [][]net.IPAddr
	calls   atomic.Int32
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	i := int(f.calls.Add(1)) - 1
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	if f.answers[i] == nil {
		return nil, errors.New("no such host")
	}
	return f.answers[i], nil
}

func TestHTTPExecPinsTheCheckedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "internal") }))
	defer srv.Close()
	port := itoa(srv.Listener.Addr().(*net.TCPAddr).Port)
	x := NewHTTPExecutor()
	// First answer is acceptable, a later one would be the metadata address.
	fr := &fakeResolver{answers: [][]net.IPAddr{{{IP: net.ParseIP("127.0.0.1")}}, {{IP: net.ParseIP("169.254.169.254")}}}}
	x.Resolver = fr
	tg := httpTarget("http://erp.internal.example:"+port, nil)
	res, e := do(t, x, tg, RequestHeader{Method: "GET", Path: "/health"}, nil)
	if e != nil || readAll(t, res) != "internal" {
		t.Fatalf("pinned request: %v", e)
	}
	if fr.calls.Load() != 1 {
		t.Errorf("resolved %d times for one connection", fr.calls.Load())
	}
	// An answer that mixes an allowed and a refused address is refused whole.
	x2 := NewHTTPExecutor()
	x2.Resolver = &fakeResolver{answers: [][]net.IPAddr{{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("169.254.169.254")}}}}
	if _, e := do(t, x2, tg, RequestHeader{Method: "GET", Path: "/health"}, nil); e == nil || e.Code != "address_not_allowed" {
		t.Errorf("mixed answer: %v", e)
	}
	x3 := NewHTTPExecutor()
	x3.Resolver = &fakeResolver{answers: [][]net.IPAddr{nil}}
	if _, e := do(t, x3, tg, RequestHeader{Method: "GET", Path: "/health"}, nil); e == nil || e.Code != "dns_failed" || e.Dispatched {
		t.Errorf("dns failure: %v", e)
	}
}

func TestHTTPExecRedirects(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "elsewhere") }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/invoices/old":
			http.Redirect(w, r, "/invoices/new", http.StatusFound)
		case "/invoices/new":
			_, _ = io.WriteString(w, "new")
		case "/invoices/admin":
			http.Redirect(w, r, "/admin", http.StatusFound)
		case "/invoices/away":
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		}
	}))
	defer srv.Close()
	x := NewHTTPExecutor()
	// By default a 3xx comes back as it is.
	res, e := do(t, x, httpTarget(srv.URL, nil), RequestHeader{Method: "GET", Path: "/invoices/old"}, nil)
	if e != nil || res.Status != 302 || res.Header.Get("Location") != "/invoices/new" {
		t.Fatalf("default: %v %+v", e, res)
	}
	res.Body.Close()
	follow := httpTarget(srv.URL, func(tg *Target) { tg.FollowSameOriginRedirects = true })
	res, e = do(t, x, follow, RequestHeader{Method: "GET", Path: "/invoices/old"}, nil)
	if e != nil || res.Status != 200 || readAll(t, res) != "new" {
		t.Fatalf("same origin allowed path: %v", e)
	}
	for _, p := range []string{"/invoices/admin", "/invoices/away"} {
		res, e = do(t, x, follow, RequestHeader{Method: "GET", Path: p}, nil)
		if e != nil || res.Status != 302 {
			t.Fatalf("%s: %v", p, e)
		}
		res.Body.Close()
	}
}

func TestHTTPExecTimeoutsAndConnectFailures(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)
	x := NewHTTPExecutor()
	slow := httpTarget(srv.URL, func(tg *Target) { tg.Timeout = Duration(150 * time.Millisecond) })
	_, e := do(t, x, slow, RequestHeader{Method: "GET", Path: "/health"}, nil)
	if e == nil || e.Code != "upstream_timeout" || !e.Dispatched {
		t.Errorf("timeout: %+v", e)
	}
	// A deadline from the gateway shorter than the target's wins.
	short := httpTarget(srv.URL, nil)
	start := time.Now()
	_, e = do(t, x, short, RequestHeader{Method: "GET", Path: "/health", DeadlineUnixMs: time.Now().Add(150 * time.Millisecond).UnixMilli()}, nil)
	if e == nil || e.Code != "upstream_timeout" || time.Since(start) > 3*time.Second {
		t.Errorf("gateway deadline: %+v after %s", e, time.Since(start))
	}
	// An expired deadline never connects.
	_, e = do(t, x, short, RequestHeader{Method: "GET", Path: "/health", DeadlineUnixMs: time.Now().Add(-time.Second).UnixMilli()}, nil)
	if e == nil || e.Code != "upstream_timeout" || e.Dispatched {
		t.Errorf("past deadline: %+v", e)
	}
	// Nothing listens: not dispatched.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().String()
	l.Close()
	_, e = do(t, x, httpTarget("http://"+closed, nil), RequestHeader{Method: "GET", Path: "/health"}, nil)
	if e == nil || e.Code != "upstream_connect_failed" || e.Dispatched {
		t.Errorf("connect failure: %+v", e)
	}
	// A cancelled context reports the shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := x.Do(ctx, short, baseHeader(RequestHeader{Method: "GET", Path: "/health"}), nil)
	var ee *ExecError
	if !errors.As(err, &ee) || ee.Code != "agent_shutdown" {
		t.Errorf("cancelled: %v", err)
	}
	// A connection reset after the request was written is dispatched.
	reset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
	}))
	defer reset.Close()
	_, e = do(t, x, httpTarget(reset.URL, nil), RequestHeader{Method: "GET", Path: "/health"}, nil)
	if e == nil || e.Code != "upstream_reset" || !e.Dispatched {
		t.Errorf("reset: %+v", e)
	}
}

func TestHTTPExecLocalRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	x := NewHTTPExecutor()
	now := time.Now()
	x.Now = func() time.Time { return now }
	tg := httpTarget(srv.URL, func(tg *Target) { tg.Rate = &Rate{PerSecond: 1, Burst: 2} })
	for i := 0; i < 2; i++ {
		res, e := do(t, x, tg, RequestHeader{Method: "GET", Path: "/health"}, nil)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
	}
	_, e := do(t, x, tg, RequestHeader{Method: "GET", Path: "/health"}, nil)
	if e == nil || e.Code != "rate_limited_local" || !e.Retryable || e.Dispatched {
		t.Errorf("third request: %+v", e)
	}
	now = now.Add(1500 * time.Millisecond)
	res, e := do(t, x, tg, RequestHeader{Method: "GET", Path: "/health"}, nil)
	if e != nil {
		t.Errorf("after a refill: %v", e)
	} else {
		res.Body.Close()
	}
}

func TestHTTPExecScrubsInjectedCredentialsFromResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "echo: "+r.Header.Get("Authorization")+" end")
	}))
	defer srv.Close()
	x := NewHTTPExecutor()
	tg := httpTarget(srv.URL, func(tg *Target) {
		tg.Credentials = &Credentials{Header: "Authorization", Value: "Bearer super-secret-value"}
	})
	res, e := do(t, x, tg, RequestHeader{Method: "GET", Path: "/health"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if got := readAll(t, res); strings.Contains(got, "super-secret-value") || !strings.Contains(got, "[redacted]") {
		t.Errorf("response = %q", got)
	}
	off := false
	tg.ScrubCredentialsInResponse = &off
	res, _ = do(t, x, tg, RequestHeader{Method: "GET", Path: "/health"}, nil)
	if got := readAll(t, res); !strings.Contains(got, "super-secret-value") {
		t.Errorf("scrubbing off still scrubbed: %q", got)
	}
}

func TestScrubberHandlesSecretsSplitAcrossReads(t *testing.T) {
	secret := []byte("0123456789abcdef")
	text := strings.Repeat("x", 100) + string(secret) + strings.Repeat("y", 100) + string(secret) + "z"
	for name, r := range map[string]io.Reader{
		"one byte at a time": iotest.OneByteReader(strings.NewReader(text)),
		"half reads":         iotest.HalfReader(strings.NewReader(text)),
		"whole":              strings.NewReader(text),
	} {
		got, err := io.ReadAll(newScrubber(r, [][]byte{secret}))
		if err != nil || bytes.Contains(got, secret) || strings.Count(string(got), "[redacted]") != 2 || !strings.HasSuffix(string(got), "z") {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	boom := errors.New("boom")
	if _, err := io.ReadAll(newScrubber(io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(boom)), [][]byte{secret})); !errors.Is(err, boom) {
		t.Errorf("a read error was lost: %v", err)
	}
}

func TestHTTPExecTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "secure") }))
	defer srv.Close()
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	x := NewHTTPExecutor()

	// Without the CA the handshake fails before anything is sent.
	_, e := do(t, x, httpTarget(srv.URL, nil), RequestHeader{Method: "GET", Path: "/health"}, nil)
	if e == nil || e.Code != "upstream_connect_failed" || e.Dispatched {
		t.Errorf("untrusted certificate: %+v", e)
	}
	res, e := do(t, x, httpTarget(srv.URL, func(tg *Target) { tg.TLS.CAFile = caFile }), RequestHeader{Method: "GET", Path: "/health"}, nil)
	if e != nil || readAll(t, res) != "secure" {
		t.Errorf("with ca_file: %v", e)
	}
	res, e = do(t, x, httpTarget(srv.URL, func(tg *Target) { tg.TLS.InsecureSkipVerify, tg.AllowInsecure = true, true }), RequestHeader{Method: "GET", Path: "/health"}, nil)
	if e != nil || readAll(t, res) != "secure" {
		t.Errorf("insecure opt-in: %v", e)
	}
	// Without allow_insecure the skip flag does nothing.
	if _, e = do(t, x, httpTarget(srv.URL, func(tg *Target) { tg.TLS.InsecureSkipVerify = true }), RequestHeader{Method: "GET", Path: "/health"}, nil); e == nil {
		t.Error("skip verify honoured without allow_insecure")
	}
	for name, mod := range map[string]func(*Target){
		"missing ca file": func(tg *Target) { tg.TLS.CAFile = filepath.Join(dir, "none.pem") },
		"empty ca file": func(tg *Target) {
			f := filepath.Join(dir, "empty.pem")
			_ = os.WriteFile(f, []byte("x"), 0o600)
			tg.TLS.CAFile = f
		},
		"bad client cert": func(tg *Target) {
			tg.TLS.ClientCert, tg.TLS.ClientKey = filepath.Join(dir, "a"), filepath.Join(dir, "b")
		},
	} {
		if _, e := do(t, x, httpTarget(srv.URL, mod), RequestHeader{Method: "GET", Path: "/health"}, nil); e == nil || e.Code != "upstream_connect_failed" {
			t.Errorf("%s: %+v", name, e)
		}
	}
}

func TestCheckMCPBody(t *testing.T) {
	tg := Target{Kind: KindMCPStdio, MCP: MCPPolicy{Methods: DefaultMCPMethods, Tools: []string{"employees_*", "leave_get"}}}
	for body, want := range map[string]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`:                                    "",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"employees_list"}}`: "",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"leave_get"}}`:      "",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"payroll_export"}}`: "mcp_tool_not_allowed",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`:                        "mcp_tool_not_allowed",
		`{"jsonrpc":"2.0","id":1,"method":"resources/read"}`:                                "mcp_method_not_allowed",
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`:                                        "mcp_method_not_allowed",
		``:                         "mcp_method_not_allowed",
		`not json`:                 "mcp_method_not_allowed",
		`{"jsonrpc":"2.0","id":1}`: "mcp_method_not_allowed",
	} {
		_, e := CheckMCPBody(tg, []byte(body))
		got := ""
		if e != nil {
			got = e.Code
		}
		if got != want {
			t.Errorf("%s = %q, want %q", body, got, want)
		}
	}
	open := tg
	open.MCP.Tools = nil
	if _, e := CheckMCPBody(open, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"anything"}}`)); e != nil {
		t.Errorf("no tool list means every tool: %v", e)
	}
}

func TestMCPHTTPTargetsAreReachedAtTheirOwnAddress(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()
	x := NewHTTPExecutor()
	tg := Target{Name: "crm", Kind: KindMCPHTTP, URL: srv.URL + "/mcp", AllowLoopback: true, Timeout: Duration(5 * time.Second)}
	tg.MCP.Methods = DefaultMCPMethods
	res, e := do(t, x, tg, RequestHeader{Method: "POST", Path: "/", Kind: KindMCPHTTP}, strings.NewReader(`{}`))
	if e != nil {
		t.Fatal(e)
	}
	readAll(t, res)
	if gotPath != "/mcp" || gotMethod != "POST" {
		t.Errorf("upstream saw %s %s", gotMethod, gotPath)
	}
	if _, e = do(t, x, tg, RequestHeader{Method: "GET", Path: "/", Kind: KindMCPHTTP}, nil); e == nil || e.Code != "method_not_allowed" {
		t.Errorf("GET: %+v", e)
	}
	if _, e = do(t, x, tg, RequestHeader{Method: "POST", Path: "/other", Kind: KindMCPHTTP}, nil); e == nil || e.Code != "path_not_allowed" {
		t.Errorf("another path: %+v", e)
	}
}

func TestExecErrorText(t *testing.T) {
	if (&ExecError{Code: "x"}).Error() != "x" || (&ExecError{Code: "x", Msg: "y"}).Error() != "x: y" {
		t.Error("Error text")
	}
	if shortErr(errors.New("plain")) != "plain" {
		t.Error("shortErr")
	}
}
