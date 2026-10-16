package tunnel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func erpTarget(url, extra string) string {
	return "  - name: erp\n    kind: http\n    url: " + url + "\n    methods: [GET, POST]\n    paths: [\"/invoices/**\", \"/health\", \"/big\"]\n    allow_loopback: true\n" + extra
}

func TestAgentAdvertisesNamesOnlyInHello(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, "    cidrs: [127.0.0.0/8]\n    credentials: { header: Authorization, value: \"Bearer hello-secret-token\" }\n"), nil)
	h := r.conn.hello
	if h.AgentID != "nag_1" || h.NetworkID != "net_1" || len(h.Proto) != 1 || h.Proto[0] != 1 || h.MaxConcurrency != 32 {
		t.Errorf("hello = %+v", h)
	}
	raw, _ := json.Marshal(h)
	for _, leak := range []string{up.URL, "127.0.0", "hello-secret-token", "Authorization"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("HELLO leaks %q: %s", leak, raw)
		}
	}
	if len(h.Allowlist.Targets) != 1 || h.Allowlist.Targets[0].Name != "erp" || h.Allowlist.Hash != r.a.config().Allowlist().Hash {
		t.Errorf("allowlist = %+v", h.Allowlist)
	}
	hdr := <-r.b.headers
	if !strings.HasPrefix(hdr.Get("Authorization"), "Bearer ") || hdr.Get("X-Agent-Id") != "nag_1" {
		t.Errorf("connect headers = %v", hdr)
	}
	parts := strings.Split(strings.TrimPrefix(hdr.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %v", parts)
	}
	r.waitFor("the agent to be connected", func() bool { return r.a.Status().Connected })
	st := r.a.Status()
	if !st.Connected || st.State != "connected" || st.AgentID != "nag_1" || st.Targets != 1 {
		t.Errorf("status = %+v", st)
	}
}

func TestAgentServesAnHTTPExchangeEndToEnd(t *testing.T) {
	var gotAuth, gotBody, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotAuth, gotBody, gotPath = r.Header.Get("Authorization"), string(b), r.URL.Path
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"id":"inv_1"}`)
	}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL+"/api", "    credentials: { header: Authorization, value: \"Bearer agent-token-123\" }\n"), nil)
	req := r.seal(RequestHeader{Target: "erp", Method: "POST", Path: "/invoices", TraceID: "trc_1", AuditRef: "aud_1", Headers: map[string][]string{"Authorization": {"Bearer from-gateway"}, "Content-Type": {"application/json"}}}, []byte(`{"amount":`), []byte(`42}`))
	res := r.open(req, r.conn.run(req, exchangeOpts{}))
	if res.Header.Status != 201 || string(res.Body) != `{"id":"inv_1"}` || res.Header.Headers["X-Upstream"][0] != "yes" {
		t.Errorf("response = %+v %q", res.Header, res.Body)
	}
	if gotAuth != "Bearer agent-token-123" || gotBody != `{"amount":42}` || gotPath != "/api/invoices" {
		t.Errorf("upstream saw %q %q %q", gotAuth, gotBody, gotPath)
	}
	if res.Trailer.AgentKID != r.key.KID() || res.Trailer.Bytes != int64(len(res.Body)) {
		t.Errorf("trailer = %+v", res.Trailer)
	}
	r.waitFor("the exchange line", func() bool { return r.out.has("exchange") != nil })
	line := r.out.has("exchange")
	if line["target"] != "erp" || line["status"] != float64(201) || line["credentials"] != "agent" || line["audit_ref"] != "aud_1" || line["trace_id"] != "trc_1" || line["outcome"] != "ok" || line["query_present"] != false {
		t.Errorf("log line = %v", line)
	}
	if strings.Contains(r.out.String(), "agent-token-123") || strings.Contains(r.out.String(), `{"amount"`) {
		t.Error("a secret or a body reached the log")
	}
}

func TestAgentLogsTheQueryAsPresentNotAsText(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	req := r.seal(RequestHeader{Target: "erp", Path: "/health", Query: "ssn=123-45-6789"})
	r.open(req, r.conn.run(req, exchangeOpts{}))
	r.waitFor("the exchange line", func() bool { return r.out.has("exchange") != nil })
	if strings.Contains(r.out.String(), "123-45-6789") || r.out.has("exchange")["query_present"] != true {
		t.Errorf("log = %s", r.out.String())
	}
}

func TestAgentStreamsLargeBodiesBothWays(t *testing.T) {
	big := bytes.Repeat([]byte("0123456789abcdef"), 192<<10) // 3 MiB
	sum := sha256.Sum256(big)
	var gotSum string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			h := sha256.New()
			_, _ = io.Copy(h, r.Body)
			gotSum = hex.EncodeToString(h.Sum(nil))
			_, _ = io.WriteString(w, "stored")
			return
		}
		_, _ = w.Write(big)
	}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)

	var chunks [][]byte
	for i := 0; i < len(big); i += 60 << 10 {
		end := min(i+60<<10, len(big))
		chunks = append(chunks, big[i:end])
	}
	up1 := r.seal(RequestHeader{Target: "erp", Method: "POST", Path: "/invoices"}, chunks...)
	if got := r.open(up1, r.conn.run(up1, exchangeOpts{})); string(got.Body) != "stored" || gotSum != hex.EncodeToString(sum[:]) {
		t.Errorf("upload: %q, upstream digest %s", got.Body, gotSum)
	}
	down := r.seal(RequestHeader{Target: "erp", Method: "GET", Path: "/big", MaxResponseBytes: 8 << 20})
	got := r.open(down, r.conn.run(down, exchangeOpts{}))
	if gs := sha256.Sum256(got.Body); gs != sum {
		t.Errorf("download: %d bytes, wrong digest", len(got.Body))
	}
}

func TestAgentHonoursFlowControl(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 3<<20)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(big) }))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	req := r.seal(RequestHeader{Target: "erp", Method: "GET", Path: "/big", MaxResponseBytes: 8 << 20})
	id := r.conn.start(req, exchangeOpts{})
	// Without credit the agent sends about its initial window and then waits.
	var received int
	deadline := time.After(700 * time.Millisecond)
loop:
	for {
		select {
		case f := <-r.conn.stream(id):
			if f.Type == FrameData {
				received += len(f.Payload)
			}
			if f.Type == FrameEnd || f.Type == FrameError {
				t.Fatalf("the response finished without credit (%d bytes)", received)
			}
		case <-deadline:
			break loop
		}
	}
	if received < initialWindow/2 || received > initialWindow+responseChunk+64 {
		t.Errorf("sent %d bytes before any credit, window is %d", received, initialWindow)
	}
	// Granting credit lets it finish.
	grant, _ := json.Marshal(map[string]int{"credit_bytes": 8 << 20})
	r.conn.send(Frame{Type: FrameWindow, StreamID: id, Payload: grant})
	res := r.conn.collect(id, exchangeOpts{noCredit: true})
	if res.Err != nil || res.End == nil {
		t.Fatalf("after credit: %+v", res.Err)
	}
}

func TestAgentRefusesWhatItsAllowlistDoesNotName(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	for name, tc := range map[string]struct {
		h    RequestHeader
		code string
	}{
		"unknown target": {RequestHeader{Target: "payroll", Path: "/x"}, "target_unknown"},
		"wrong kind":     {RequestHeader{Target: "erp", Kind: "mcp_stdio", Path: "/x"}, "target_unknown"},
		"method":         {RequestHeader{Target: "erp", Method: "DELETE", Path: "/invoices/1"}, "method_not_allowed"},
		"path":           {RequestHeader{Target: "erp", Method: "GET", Path: "/admin"}, "path_not_allowed"},
		"traversal":      {RequestHeader{Target: "erp", Method: "GET", Path: "/invoices/../admin"}, "path_not_allowed"},
	} {
		req := r.seal(tc.h)
		res := r.conn.run(req, exchangeOpts{})
		if res.Err == nil || res.Err.Code != tc.code || res.Err.Dispatched {
			t.Errorf("%s: %+v", name, res.Err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("%d refused requests reached the upstream", hits.Load())
	}
	r.waitFor("a policy violation line", func() bool { return r.out.has("policy_violation") != nil })
	if r.a.metrics.violations.Load() < 3 {
		t.Errorf("violations = %d", r.a.metrics.violations.Load())
	}
	var prom bytes.Buffer
	r.a.WritePrometheus(&prom)
	for _, want := range []string{"zuano_connector_exchanges_total{target=\"erp\",outcome=\"refused\"}", "zuano_connector_connected 1", "zuano_connector_policy_violations_total", "zuano_connector_build_info"} {
		if !strings.Contains(prom.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, prom.String())
		}
	}
}

func TestAgentTreatsBrokenCryptoAsSecurityEvents(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	foreign, _ := GenerateKey(time.Now(), time.Hour)
	// Sealed to another agent: cannot be opened.
	req := r.gw.seal(RequestHeader{Target: "erp", Path: "/health"}, nil, foreign)
	if res := r.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Code != "decrypt_failed" || res.Err.Dispatched {
		t.Errorf("foreign key: %+v", res.Err)
	}
	// Signed by a gateway the agent does not know.
	other := newTestGateway(t)
	req = other.seal(RequestHeader{Target: "erp", Path: "/health"}, nil, r.key)
	if res := r.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Code != "signature_invalid" {
		t.Errorf("unknown gateway: %+v", res.Err)
	}
	// Replayed: the same sealed request on a new stream.
	req = r.seal(RequestHeader{Target: "erp", Path: "/health"})
	if res := r.conn.run(req, exchangeOpts{}); res.Err != nil {
		t.Fatalf("first use: %+v", res.Err)
	}
	if res := r.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Code != "replay" {
		t.Errorf("replay: %+v", res.Err)
	}
	// Stale.
	req = r.seal(RequestHeader{Target: "erp", Path: "/health", IssuedAtMs: time.Now().Add(-5 * time.Minute).UnixMilli()})
	if res := r.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Code != "expired" {
		t.Errorf("stale: %+v", res.Err)
	}
	// Another organisation's request.
	req = r.seal(RequestHeader{Target: "erp", Path: "/health", OrgID: "org_other", NetworkID: "net_1"})
	if res := r.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Code != "signature_invalid" {
		t.Errorf("other org: %+v", res.Err)
	}
	// Garbage OPEN.
	id := r.conn.next.Add(1)
	r.conn.send(Frame{Type: FrameOpen, StreamID: id, Payload: []byte("not json")})
	if res := r.conn.collect(id, exchangeOpts{}); res.Err == nil || res.Err.Code != "decrypt_failed" {
		t.Errorf("garbage: %+v", res.Err)
	}
	r.waitFor("security log lines", func() bool { return r.a.metrics.security.Load() >= 6 })
	if ev := r.out.has("security"); ev == nil || ev["level"] != "error" {
		t.Errorf("security event = %v", ev)
	}
	if hits := r.a.metrics.exchanges[[2]string{"", "protocol_error"}]; hits == 0 {
		t.Log("protocol errors are counted against the empty target")
	}
}

func TestAgentAbortsAnUploadWhoseSignatureFails(t *testing.T) {
	var done atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		done.Store(err == nil)
		_, _ = io.WriteString(w, "stored")
	}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	big := bytes.Repeat([]byte("z"), 2<<20)
	var chunks [][]byte
	for i := 0; i < len(big); i += 128 << 10 {
		chunks = append(chunks, big[i:min(i+128<<10, len(big))])
	}
	req := r.seal(RequestHeader{Target: "erp", Method: "POST", Path: "/invoices"}, chunks...)
	res := r.conn.run(req, exchangeOpts{tamperFinal: true})
	if res.Err == nil || res.Err.Code != "decrypt_failed" || !res.Err.Dispatched {
		t.Fatalf("tampered final: %+v", res.Err)
	}
	if done.Load() {
		t.Error("the upstream received a complete body that failed verification")
	}
	r.gw.badEndSig = true
	req = r.seal(RequestHeader{Target: "erp", Method: "POST", Path: "/invoices"}, chunks...)
	r.gw.badEndSig = false
	res = r.conn.run(req, exchangeOpts{})
	if res.Err == nil || res.Err.Code != "signature_invalid" || !res.Err.Dispatched {
		t.Fatalf("wrong end signature: %+v", res.Err)
	}
	if done.Load() {
		t.Error("the upstream received a complete body that failed verification")
	}
	// A small body is verified before the upstream sees any of it.
	var hits atomic.Int32
	small := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer small.Close()
	r2 := newRig(t, erpTarget(small.URL, ""), nil)
	r2.gw.badEndSig = true
	req = r2.seal(RequestHeader{Target: "erp", Method: "POST", Path: "/invoices"}, []byte("tiny"))
	if res := r2.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Code != "signature_invalid" || res.Err.Dispatched {
		t.Errorf("small body with a wrong signature: %+v", res.Err)
	}
	if hits.Load() != 0 {
		t.Error("an unverified small body reached the upstream")
	}
}

func TestAgentCancelStopsTheUpstreamRequest(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	req := r.seal(RequestHeader{Target: "erp", Path: "/health"})
	id := r.conn.start(req, exchangeOpts{})
	<-started
	r.conn.send(Frame{Type: FrameCancel, StreamID: id, Payload: []byte(`{"reason":"abort"}`)})
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("CANCEL did not reach the upstream request")
	}
	r.waitFor("the exchange to end", func() bool { return r.a.inflight.Load() == 0 })
}

func TestAgentPauseRefusesAndResumeServes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	r.a.Pause()
	if !r.a.Paused() || r.a.Status().State != "paused" {
		t.Error("not paused")
	}
	req := r.seal(RequestHeader{Target: "erp", Path: "/health"})
	if res := r.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Code != "agent_paused" || res.Err.Dispatched {
		t.Errorf("paused: %+v", res.Err)
	}
	r.waitFor("a STATUS that says paused", func() bool {
		for {
			select {
			case f := <-r.conn.control:
				var st statusPayload
				if f.Type == FrameStatus && json.Unmarshal(f.Payload, &st) == nil && st.Paused {
					return true
				}
			default:
				return false
			}
		}
	})
	r.a.Resume()
	req = r.seal(RequestHeader{Target: "erp", Path: "/health"})
	r.open(req, r.conn.run(req, exchangeOpts{}))
}

func TestAgentLimitsConcurrency(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer up.Close()
	defer close(release)
	r := newRig(t, erpTarget(up.URL, ""), nil)
	// Lower the limit through the config file.
	cfg := strings.Replace(readFileT(t, r.cfgPth), "shutdown_grace: 1s", "shutdown_grace: 1s\nmax_concurrency: 1", 1)
	writeFile(t, r.cfgPth, cfg)
	r.waitFor("the reload", func() bool { return r.a.config().MaxConcurrency == 1 })
	first := r.seal(RequestHeader{Target: "erp", Path: "/health"})
	r.conn.start(first, exchangeOpts{})
	r.waitFor("the first exchange", func() bool { return r.a.inflight.Load() == 1 })
	second := r.seal(RequestHeader{Target: "erp", Path: "/health"})
	if res := r.conn.run(second, exchangeOpts{}); res.Err == nil || res.Err.Code != "agent_busy" || res.Err.Dispatched || !res.Err.Retryable {
		t.Errorf("second exchange: %+v", res.Err)
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
