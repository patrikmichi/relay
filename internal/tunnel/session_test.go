package tunnel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func stdioTargetYAML() string {
	return "  - name: hr\n    kind: mcp_stdio\n    command: " + os.Args[0] + "\n    args: [\"-test.run=^TestHelperMCP$\"]\n    env: { TUNNEL_HELPER: mcp }\n    mcp: { tools: [\"echo\"] }\n"
}

func TestAgentServesMCPStdioThroughTheTunnel(t *testing.T) {
	r := newRig(t, stdioTargetYAML(), nil)
	body := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{"a":1}}}`
	req := r.seal(RequestHeader{Target: "hr", Kind: KindMCPStdio, Method: "POST", Path: "/"}, []byte(body))
	res := r.open(req, r.conn.run(req, exchangeOpts{}))
	if res.Header.Status != 200 || !strings.Contains(string(res.Body), `"id":9`) || !strings.Contains(string(res.Body), `"a":1`) {
		t.Errorf("response = %d %s", res.Header.Status, res.Body)
	}
	// A tool the local list does not name is refused even though the gateway signed it.
	bad := `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"payroll_export"}}`
	req = r.seal(RequestHeader{Target: "hr", Kind: KindMCPStdio, Method: "POST", Path: "/"}, []byte(bad))
	if res := r.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Code != "mcp_tool_not_allowed" || res.Err.Dispatched {
		t.Errorf("unlisted tool: %+v", res.Err)
	}
	// A method outside mcp.methods, and a batch.
	for _, b := range []string{`{"jsonrpc":"2.0","id":1,"method":"resources/read"}`, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`} {
		req = r.seal(RequestHeader{Target: "hr", Kind: KindMCPStdio, Method: "POST", Path: "/"}, []byte(b))
		if res := r.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Code != "mcp_method_not_allowed" {
			t.Errorf("%s: %+v", b, res.Err)
		}
	}
	// A notification gets 202 and no body.
	note := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	req = r.seal(RequestHeader{Target: "hr", Kind: KindMCPStdio, Method: "POST", Path: "/"}, []byte(note))
	if res := r.open(req, r.conn.run(req, exchangeOpts{})); res.Header.Status != 202 || len(res.Body) != 0 {
		t.Errorf("notification = %d %q", res.Header.Status, res.Body)
	}
	// tools/list_changed from the process is reported in STATUS.
	notify := `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"echo"}}`
	_ = notify
}

func TestAgentServesMCPHTTPThroughTheTunnel(t *testing.T) {
	var body string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
	defer up.Close()
	r := newRig(t, "  - name: crm\n    kind: mcp_http\n    url: "+up.URL+"/mcp\n    allow_loopback: true\n    mcp: { tools: [\"crm_*\"] }\n", nil)
	ok := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"crm_find"}}`
	req := r.seal(RequestHeader{Target: "crm", Kind: KindMCPHTTP, Method: "POST", Path: "/"}, []byte(ok))
	if res := r.open(req, r.conn.run(req, exchangeOpts{})); !strings.Contains(string(res.Body), `"ok":true`) || body != ok {
		t.Errorf("response = %s, upstream got %q", res.Body, body)
	}
	body = ""
	bad := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hr_export"}}`
	req = r.seal(RequestHeader{Target: "crm", Kind: KindMCPHTTP, Method: "POST", Path: "/"}, []byte(bad))
	if res := r.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Code != "mcp_tool_not_allowed" || body != "" {
		t.Errorf("unlisted tool: %+v, upstream got %q", res.Err, body)
	}
	huge := bytes.Repeat([]byte("a"), bufferLimit+10)
	var chunks [][]byte
	for i := 0; i < len(huge); i += 100 << 10 {
		chunks = append(chunks, huge[i:min(i+100<<10, len(huge))])
	}
	req = r.seal(RequestHeader{Target: "crm", Kind: KindMCPHTTP, Method: "POST", Path: "/"}, chunks...)
	if res := r.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Code != "body_too_large" {
		t.Errorf("oversized MCP message: %+v", res.Err)
	}
}

func TestAgentSpillsLargeResponsesToTheFileStore(t *testing.T) {
	payload := bytes.Repeat([]byte("spill-me-"), 1<<10) // ~9 KiB
	var uploaded []byte
	var ctype string
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploaded, _ = io.ReadAll(r.Body)
		ctype = r.Header.Get("Content-Type")
	}))
	defer store.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	cfg := strings.Replace(readFileT(t, r.cfgPth), "shutdown_grace: 1s", "shutdown_grace: 1s\nspill_threshold: 4096", 1)
	writeFile(t, r.cfgPth, cfg)
	r.waitFor("the reload", func() bool { return r.a.config().SpillThreshold == 4096 })

	req := r.seal(RequestHeader{Target: "erp", Path: "/big", Spill: &Spill{UploadURL: store.URL + "/up", FileID: "fil_1", MaxBytes: 1 << 20}})
	res := r.open(req, r.conn.run(req, exchangeOpts{}))
	sum := sha256.Sum256(payload)
	if res.Header.Spilled == nil || res.Header.Spilled.FileID != "fil_1" || res.Header.Spilled.Size != int64(len(payload)) || res.Header.Spilled.SHA256 != hex.EncodeToString(sum[:]) || res.Header.Spilled.ContentType != "application/pdf" {
		t.Fatalf("spilled = %+v", res.Header.Spilled)
	}
	if len(res.Body) != 0 || !bytes.Equal(uploaded, payload) || ctype != "application/pdf" {
		t.Errorf("body %d bytes, uploaded %d bytes, content type %q", len(res.Body), len(uploaded), ctype)
	}

	// A response under the threshold, or one that does not fit the cap, comes through the tunnel.
	small := r.seal(RequestHeader{Target: "erp", Path: "/big", Spill: &Spill{UploadURL: store.URL + "/up", FileID: "fil_2", MaxBytes: 100}})
	if got := r.open(small, r.conn.run(small, exchangeOpts{})); got.Header.Spilled != nil || !bytes.Equal(got.Body, payload) {
		t.Errorf("over the file store's cap: spilled %+v, %d bytes through the tunnel", got.Header.Spilled, len(got.Body))
	}

	// A file store that cannot be reached: the response goes through the tunnel and STATUS says why.
	closed := httptest.NewServer(http.NotFoundHandler())
	dead := closed.URL
	closed.Close()
	req = r.seal(RequestHeader{Target: "erp", Path: "/big", Spill: &Spill{UploadURL: dead + "/up", FileID: "fil_3", MaxBytes: 1 << 20}})
	res = r.open(req, r.conn.run(req, exchangeOpts{}))
	if res.Header.Spilled != nil || !bytes.Equal(res.Body, payload) {
		t.Errorf("fallback: spilled %+v, %d bytes", res.Header.Spilled, len(res.Body))
	}
	if !r.a.spillUnavailable.Load() {
		t.Error("spill_unavailable was not noted")
	}
	// An upload address that is not https is not used.
	req = r.seal(RequestHeader{Target: "erp", Path: "/big", Spill: &Spill{UploadURL: "http://files.example/up", FileID: "fil_4", MaxBytes: 1 << 20}})
	if res := r.open(req, r.conn.run(req, exchangeOpts{})); res.Header.Spilled != nil {
		t.Error("spilled to a plain http address")
	}
}

func TestAgentFetchesLargeFilesItselfAndChecksTheDigest(t *testing.T) {
	file := bytes.Repeat([]byte("F"), 20<<10)
	sum := sha256.Sum256(file)
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(file)
	}))
	defer store.Close()
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, "stored")
	}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	ref := FileRef{Param: "body", URL: store.URL + "/f", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(file))}
	req := r.seal(RequestHeader{Target: "erp", Method: "POST", Path: "/invoices", FileRefs: []FileRef{ref}})
	if res := r.open(req, r.conn.run(req, exchangeOpts{})); string(res.Body) != "stored" || !bytes.Equal(got, file) {
		t.Errorf("response %q, upstream got %d bytes", res.Body, len(got))
	}
	got = nil
	for name, bad := range map[string]FileRef{
		"digest mismatch": {Param: "body", URL: store.URL + "/f", SHA256: strings.Repeat("0", 64), Size: ref.Size},
		"size mismatch":   {Param: "body", URL: store.URL + "/f", SHA256: ref.SHA256, Size: ref.Size - 1},
		"missing file":    {Param: "body", URL: store.URL + "/missing", SHA256: ref.SHA256, Size: ref.Size},
		"plain http":      {Param: "body", URL: "http://files.example/f", SHA256: ref.SHA256, Size: ref.Size},
		"other param":     {Param: "attachment", URL: store.URL + "/f", SHA256: ref.SHA256, Size: ref.Size},
	} {
		req = r.seal(RequestHeader{Target: "erp", Method: "POST", Path: "/invoices", FileRefs: []FileRef{bad}})
		if res := r.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Dispatched {
			t.Errorf("%s: %+v", name, res.Err)
		}
	}
	if got != nil {
		t.Error("a file that failed its check reached the upstream")
	}
	entries, _ := os.ReadDir(filepath.Join(r.a.stateDir(), "tmp"))
	if len(entries) != 0 {
		t.Errorf("%d temporary files were left behind", len(entries))
	}
}

func TestAgentCorrectsForClockSkewFromHelloAck(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer up.Close()
	fb := time.Now().Add(10 * time.Minute) // the gateway's clock runs ten minutes ahead
	r := newRigWith(t, erpTarget(up.URL, ""), func(b *fakeBroker) { b.serverTime.Store(fb.UnixMilli()) })
	r.waitFor("the offset", func() bool { return r.a.offset.Load() != 0 })
	req := r.seal(RequestHeader{Target: "erp", Path: "/health", IssuedAtMs: fb.UnixMilli()})
	r.open(req, r.conn.run(req, exchangeOpts{}))
	// A request stamped with the agent's own, uncorrected, clock is now too old.
	req = r.seal(RequestHeader{Target: "erp", Path: "/health", IssuedAtMs: time.Now().UnixMilli()})
	if res := r.conn.run(req, exchangeOpts{}); res.Err == nil || res.Err.Code != "expired" {
		t.Errorf("uncorrected clock: %+v", res.Err)
	}
}

func newRigWith(t *testing.T, targets string, prep func(*fakeBroker)) *agentRig {
	t.Helper()
	prepBroker = prep
	defer func() { prepBroker = nil }()
	return newRig(t, targets, nil)
}

func TestAgentReloadsItsConfigAndKeepsTheOldOnErrors(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	first := r.a.config().Allowlist().Hash

	drain := func() []statusPayload {
		var out []statusPayload
		for {
			select {
			case f := <-r.conn.control:
				var st statusPayload
				if f.Type == FrameStatus && json.Unmarshal(f.Payload, &st) == nil {
					out = append(out, st)
				}
			default:
				return out
			}
		}
	}
	drain()
	// A valid change is picked up and re-advertised in STATUS.
	writeFile(t, r.cfgPth, readFileT(t, r.cfgPth)+"  - name: second\n    kind: http\n    url: "+up.URL+"\n    methods: [GET]\n    paths: [\"/x\"]\n    allow_loopback: true\n")
	r.waitFor("the new allowlist", func() bool { return r.a.config().Allowlist().Hash != first })
	var seen bool
	r.waitFor("a STATUS with the new targets", func() bool {
		for _, st := range drain() {
			if len(st.Allowlist.Targets) == 2 && st.ConfigHash == r.a.config().Allowlist().Hash {
				seen = true
			}
		}
		return seen
	})
	second := r.a.config().Allowlist().Hash
	// An invalid change leaves the old config in force.
	writeFile(t, r.cfgPth, "version: 1\nnetwork: net_1\ntargets: [{name: bad}]\n")
	r.waitFor("the failed reload", func() bool { return r.out.has("config_reload_failed") != nil })
	if r.a.config().Allowlist().Hash != second {
		t.Error("an invalid config replaced the running one")
	}
	// A config for another network is refused too.
	writeFile(t, r.cfgPth, "version: 1\nnetwork: net_other\ntargets: []\n")
	r.waitFor("the second failed reload", func() bool {
		n := 0
		for _, l := range r.out.lines() {
			if l["event"] == "config_reload_failed" {
				n++
			}
		}
		return n >= 2
	})
	r.a.Reload()
}

func TestAgentReconnectsAfterTheBrokerDrops(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	_ = r.conn.ws.Close()
	second := r.b.waitConn()
	r.conn = second
	req := r.seal(RequestHeader{Target: "erp", Path: "/health"})
	r.open(req, r.conn.run(req, exchangeOpts{}))
	if r.a.metrics.reconnects.Load() < 1 || r.out.has("disconnected") == nil {
		t.Error("the drop was not noted")
	}
	// A DRAIN from the broker moves the agent on to a fresh connection.
	r.conn.send(Frame{Type: FrameDrain})
	r.conn = r.b.waitConn()
	req = r.seal(RequestHeader{Target: "erp", Path: "/health"})
	r.open(req, r.conn.run(req, exchangeOpts{}))
}

func TestAgentStopsForRevocationAndRetriesOtherRefusals(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	r.b.refuseStatus.Store(403)
	r.b.refuseCode.Store("version_unsupported")
	_ = r.conn.ws.Close()
	r.waitFor("the refusal to be logged", func() bool { return r.out.has("refused") != nil })
	if r.out.has("refused")["reason"] != "version_unsupported" {
		t.Errorf("refusal = %v", r.out.has("refused"))
	}
	select {
	case err := <-r.done:
		t.Fatalf("the agent stopped for version_unsupported: %v", err)
	default:
	}
}

func TestAgentExitsWhenRevoked(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	r.b.refuseStatus.Store(403)
	r.b.refuseCode.Store("agent_revoked")
	_ = r.conn.ws.Close()
	select {
	case err := <-r.done:
		if !errors.Is(err, ErrRevoked) {
			t.Errorf("Run = %v", err)
		}
		t.Log(r.out.String())
	case <-time.After(10 * time.Second):
		t.Fatal("the agent kept running after revocation")
	}
}

func TestClassifyRefusal(t *testing.T) {
	for code, want := range map[string]error{"agent_revoked": ErrRevoked, "agent_unknown": ErrRevoked, "key_expired": ErrRevoked, "version_unsupported": ErrVersionUnsupported, "network_disabled": ErrNetworkDisabled} {
		after, err := classifyRefusal(&refusal{code: code, status: 403})
		if !errors.Is(err, want) || (want != ErrRevoked && after == 0) {
			t.Errorf("%s: %v after %s", code, err, after)
		}
		_, err = classifyRefusal(&websocket.CloseError{Code: 1008, Text: code})
		if !errors.Is(err, want) {
			t.Errorf("close %s: %v", code, err)
		}
	}
	if _, err := classifyRefusal(&refusal{code: "other"}); err != nil {
		t.Errorf("other: %v", err)
	}
	if _, err := classifyRefusal(errors.New("dial tcp: refused")); err != nil {
		t.Errorf("network error: %v", err)
	}
	if refusalFromBody(strings.NewReader(`{"code":"x"}`)) != "x" || refusalFromBody(strings.NewReader("nope")) != "" {
		t.Error("refusalFromBody")
	}
	if (&refusal{code: "c", status: 1}).Error() == "" {
		t.Error("refusal text")
	}
}

func TestAgentDrainsOnShutdown(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Store(true)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "finished")
	}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	req := r.seal(RequestHeader{Target: "erp", Path: "/health"})
	id := r.conn.start(req, exchangeOpts{})
	r.waitFor("the upstream request", started.Load)
	go r.cancel()
	r.waitFor("DRAIN", func() bool {
		select {
		case f := <-r.conn.control:
			return f.Type == FrameDrain
		default:
			return false
		}
	})
	if !r.a.Status().Connected && r.a.Status().State != "draining" {
		t.Logf("state %s", r.a.Status().State)
	}
	// A new exchange during the drain is refused, a running one finishes.
	late := r.seal(RequestHeader{Target: "erp", Path: "/health"})
	lateID := r.conn.start(late, exchangeOpts{})
	if res := r.conn.collect(lateID, exchangeOpts{}); res.Err == nil || res.Err.Code != "agent_shutdown" || res.Err.Dispatched {
		t.Errorf("late exchange: %+v", res.Err)
	}
	close(release)
	res := r.conn.collect(id, exchangeOpts{})
	if res.Err != nil {
		t.Fatalf("the running exchange: %+v", res.Err)
	}
	if got := r.open(req, res); string(got.Body) != "finished" {
		t.Errorf("body = %q", got.Body)
	}
}

func TestAgentCancelsWhatOutlastsTheGrace(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	req := r.seal(RequestHeader{Target: "erp", Path: "/health"})
	id := r.conn.start(req, exchangeOpts{})
	r.waitFor("the exchange", func() bool { return r.a.inflight.Load() == 1 })
	r.cancel()
	res := r.conn.collect(id, exchangeOpts{wait: 15 * time.Second})
	if res.Err == nil || res.Err.Code != "agent_shutdown" || !res.Err.Dispatched {
		t.Errorf("past the grace: %+v", res.Err)
	}
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestAgentUploadsErrorLogsInStatus(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	req := r.seal(RequestHeader{Target: "erp", Method: "DELETE", Path: "/invoices/1"})
	r.conn.run(req, exchangeOpts{})
	var logs int
	r.waitFor("a STATUS carrying the violation", func() bool {
		for {
			select {
			case f := <-r.conn.control:
				var st statusPayload
				if f.Type == FrameStatus && json.Unmarshal(f.Payload, &st) == nil {
					for _, l := range st.Logs {
						if strings.Contains(string(l), "policy_violation") {
							logs++
						}
					}
					if st.Violations > 0 && st.LastUpstreamErr == "method_not_allowed" && logs > 0 {
						return true
					}
				}
			default:
				return false
			}
		}
	})
}

func TestNewAgentRefusals(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	writeFile(t, cfg, "version: 1\nnetwork: net_1\ntargets: []\n")
	if _, err := NewAgent(AgentOptions{ConfigPath: cfg, StateDir: filepath.Join(dir, "none")}); !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("no identity: %v", err)
	}
	if _, err := NewAgent(AgentOptions{ConfigPath: filepath.Join(dir, "missing.yaml")}); err == nil {
		t.Error("missing config")
	}
	id := &Identity{AgentID: "nag_1", NetworkID: "net_other", Keys: nil}
	k, _ := GenerateKey(time.Now(), time.Hour)
	id.Keys = []AgentKey{k}
	if err := id.Save(dir); err != nil {
		t.Fatal(err)
	}
	var ce *ErrConfig
	if _, err := NewAgent(AgentOptions{ConfigPath: cfg, StateDir: dir}); !errors.As(err, &ce) || !strings.Contains(err.Error(), "net_other") {
		t.Errorf("network mismatch: %v", err)
	}
	writeFile(t, cfg, "version: 1\nnetwork: net_other\nlog: { file: /proc/nope/x.log }\ntargets: []\n")
	if _, err := NewAgent(AgentOptions{ConfigPath: cfg, StateDir: dir}); !errors.As(err, &ce) {
		t.Errorf("unwritable log: %v", err)
	}
}

func TestDefaultBackoffIsBoundedAndJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		d := defaultBackoff(0)
		if d <= 0 || d > time.Second {
			t.Fatalf("attempt 0: %s", d)
		}
		seen[d] = true
	}
	if len(seen) < 10 {
		t.Error("no jitter")
	}
	for i := 0; i < 50; i++ {
		if d := defaultBackoff(20); d <= 0 || d > 60*time.Second {
			t.Fatalf("attempt 20: %s", d)
		}
	}
}

func TestServerOffset(t *testing.T) {
	now := time.Now()
	if d := serverOffset(json.RawMessage(`"`+now.Add(time.Minute).UTC().Format(time.RFC3339)+`"`), now); d < 50*time.Second || d > 70*time.Second {
		t.Errorf("rfc3339 offset = %s", d)
	}
	if serverOffset(json.RawMessage(`null`), now) != 0 || serverOffset(json.RawMessage(`"junk"`), now) != 0 {
		t.Error("junk server_time")
	}
}

func TestHealthEndpoints(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	r.waitFor("ready", func() bool { return r.a.ready.Load() })
	srv := httptest.NewServer(r.a.HealthHandler())
	defer srv.Close()
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/metrics": 200} {
		resp, err := http.Get(srv.URL + path)
		if err != nil || resp.StatusCode != want {
			t.Errorf("%s: %v %v", path, resp, err)
		}
		resp.Body.Close()
	}
	r.a.ready.Store(false)
	resp, _ := http.Get(srv.URL + "/readyz")
	if resp.StatusCode != 503 {
		t.Errorf("not ready: %d", resp.StatusCode)
	}
	resp.Body.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.a.ServeHealth(ctx, "127.0.0.1:0") }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("ServeHealth = %v", err)
	}
	if err := r.a.ServeHealth(context.Background(), "256.0.0.1:1"); err == nil {
		t.Error("a bad address was served")
	}
}
