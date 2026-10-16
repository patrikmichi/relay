package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/patrikmichi/zuano/internal/tunnel"
)

// testToken has the shape of an enrollment token and is assembled at run time.
var testToken = "nxt_" + strings.Repeat("A", 8) + "BCDEFG"

func runCmd(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(t.Context(), args, &o, &e)
	return code, o.String(), e.String()
}

// setup writes a config and an identity into a temp dir.
func setup(t *testing.T, brokerURL, appURL, targets string) (cfgPath, stateDir string, key tunnel.AgentKey) {
	t.Helper()
	dir := t.TempDir()
	stateDir = filepath.Join(dir, "state")
	key, err := tunnel.GenerateKey(time.Now(), 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": "k1", "x": base64.RawURLEncoding.EncodeToString(pub)}}})
	id := &tunnel.Identity{AgentID: "nag_1", NetworkID: "net_1", OrgID: "org_1", Name: "t", BrokerURL: brokerURL, AppURL: appURL, Keys: []tunnel.AgentKey{key}, GatewayJWKS: jwks, JWKSFetched: time.Now(), KeyValidity: 30 * 24 * time.Hour}
	if err := id.Save(stateDir); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("version: 1\nnetwork: net_1\nstate_dir: "+stateDir+"\nshutdown_grace: 1s\ntargets:\n"+targets), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, stateDir, key
}

func TestVersionHelpAndUsage(t *testing.T) {
	version = "1.2.3"
	defer func() { version = "dev" }()
	if code, out, _ := runCmd(t, "version"); code != 0 || !strings.Contains(out, "zuano-connector 1.2.3") || !strings.Contains(out, "protocol 1") {
		t.Errorf("version: %d %q", code, out)
	}
	if code, out, _ := runCmd(t, "--help"); code != 0 || !strings.Contains(out, "Commands:") {
		t.Errorf("help: %d %q", code, out)
	}
	if code, _, errOut := runCmd(t); code != 2 || !strings.Contains(errOut, "Usage:") {
		t.Errorf("no command: %d %q", code, errOut)
	}
	if code, _, errOut := runCmd(t, "frobnicate"); code != 2 || !strings.Contains(errOut, "unknown command") {
		t.Errorf("unknown: %d %q", code, errOut)
	}
	if code, _, _ := runCmd(t, "status", "stray"); code != 2 {
		t.Errorf("stray argument: %d", code)
	}
	if code, _, _ := runCmd(t, "status", "--nope"); code != 2 {
		t.Errorf("unknown flag: %d", code)
	}
	if code, _, _ := runCmd(t, "status", "-h"); code != 0 {
		t.Errorf("-h: %d", code)
	}
}

func TestEnrollWritesTheIdentityAndRefusesASecondTime(t *testing.T) {
	var proofs atomic.Int32
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
			Proof []byte `json:"proof"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Proof) == ed25519.SignatureSize {
			proofs.Add(1)
		}
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		jwks := `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k1","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}]}`
		_, _ = w.Write([]byte(`{"agent_id":"nag_7","network_id":"net_7","broker_url":"https://broker.example","gateway_jwks":` + jwks + `}`))
	}))
	defer app.Close()
	dir := t.TempDir()
	cfg, state := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state")
	code, out, errOut := runCmd(t, "enroll", "--token", testToken, "--url", app.URL, "--config", cfg, "--state-dir", state, "--name", "dc1-a")
	if code != 0 || !strings.Contains(out, "Enrolled as nag_7 in network net_7") || !strings.Contains(out, "approves this agent") {
		t.Fatalf("enroll: %d %q %q", code, out, errOut)
	}
	if proofs.Load() != 1 {
		t.Error("no key proof was sent")
	}
	if _, err := tunnel.LoadIdentity(state); err != nil {
		t.Error(err)
	}
	if code, _, errOut := runCmd(t, "enroll", "--token", testToken, "--url", app.URL, "--config", cfg, "--state-dir", state); code != 1 || !strings.Contains(errOut, "already enrolled") {
		t.Errorf("second enroll: %d %q", code, errOut)
	}
	if code, _, _ := runCmd(t, "enroll", "--config", cfg, "--state-dir", filepath.Join(dir, "other")); code != 2 {
		t.Errorf("no token: %d", code)
	}
	t.Setenv("ZUANO_CONNECTOR_ENROLL_TOKEN", "bad")
	if code, _, errOut := runCmd(t, "enroll", "--url", app.URL, "--state-dir", filepath.Join(dir, "third")); code != 1 || !strings.Contains(errOut, "nxt_") {
		t.Errorf("bad token from the environment: %d %q", code, errOut)
	}
}

func TestCheckCommand(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	defer up.Close()
	good := "  - name: erp\n    kind: http\n    url: " + up.URL + "\n    methods: [GET]\n    paths: [\"/**\"]\n    allow_loopback: true\n    credentials: { bearer: literal-secret-value }\n"
	cfg, _, _ := setup(t, "https://b", "https://a", good)
	code, out, _ := runCmd(t, "check", "--config", cfg)
	if code != 0 || !strings.Contains(out, "1 target(s) checked: ok") || !strings.Contains(out, "without TLS") {
		t.Errorf("good: %d %q", code, out)
	}
	if code, out, _ := runCmd(t, "check", "--config", cfg, "--print"); code != 0 || strings.Contains(out, "literal-secret-value") || !strings.Contains(out, "<literal hidden>") {
		t.Errorf("print: %d %q", code, out)
	}
	bad, _, _ := setup(t, "https://b", "https://a", "  - name: hr\n    kind: mcp_stdio\n    command: /nonexistent/x\n")
	if code, out, _ := runCmd(t, "check", "--config", bad); code != 1 || !strings.Contains(out, "problems found") {
		t.Errorf("bad: %d %q", code, out)
	}
	invalid := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(invalid, []byte("version: 3\nnetwork: x\ntargets: []\n"), 0o644)
	if code, _, errOut := runCmd(t, "check", "--config", invalid); code != 2 || !strings.Contains(errOut, "not valid") {
		t.Errorf("invalid: %d %q", code, errOut)
	}

	// --hints asks the gateway which targets its bindings use.
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"targets":["erp","payroll"]}`))
	}))
	defer app.Close()
	withApp, state, _ := setup(t, "https://b", app.URL, good)
	code, out, _ = runCmd(t, "check", "--config", withApp, "--hints")
	if code != 0 || !strings.Contains(out, "warn: payroll") {
		t.Errorf("hints: %d %q", code, out)
	}
	if code, _, _ := runCmd(t, "check", "--config", withApp, "--hints", "--state-dir", filepath.Join(state, "none")); code != 2 {
		t.Errorf("hints without an identity: %d", code)
	}
}

func TestStatusPauseAndResumeUseTheControlSocket(t *testing.T) {
	st := &tunnel.Status{State: "connected", AgentID: "nag_1", NetworkID: "net_1", Broker: "https://b", Version: "1.0.0", Proto: 1, AllowlistHash: "abc", Targets: 2, UptimeSeconds: 90, KeyAgeDays: 4, UpdateChannel: "stable", LastError: "path_not_allowed", Paused: true}
	old := controlRequest
	defer func() { controlRequest = old }()
	var last string
	controlRequest = func(_, cmd string) (*tunnel.Status, error) {
		last = cmd
		cp := *st
		cp.Paused = cmd == "pause"
		return &cp, nil
	}
	cfg, _, _ := setup(t, "https://b", "https://a", "")
	code, out, _ := runCmd(t, "status", "--config", cfg)
	if code != 0 || !strings.Contains(out, "State:      connected") || !strings.Contains(out, "Last error: path_not_allowed") || !strings.Contains(out, "(2 targets)") {
		t.Errorf("status: %d %q", code, out)
	}
	code, out, _ = runCmd(t, "status", "--config", cfg, "--json")
	var back tunnel.Status
	if code != 0 || json.Unmarshal([]byte(out), &back) != nil || back.AgentID != "nag_1" {
		t.Errorf("status --json: %d %q", code, out)
	}
	if code, out, _ := runCmd(t, "pause", "--config", cfg); code != 0 || last != "pause" || !strings.Contains(out, "agent_paused") {
		t.Errorf("pause: %d %q %q", code, out, last)
	}
	if code, out, _ := runCmd(t, "resume", "--config", cfg); code != 0 || last != "resume" || !strings.Contains(out, "Resumed") {
		t.Errorf("resume: %d %q", code, out)
	}
	controlRequest = func(string, string) (*tunnel.Status, error) { return nil, errors.New("the agent is not running here") }
	if code, _, errOut := runCmd(t, "status", "--config", cfg); code != 1 || !strings.Contains(errOut, "not running") {
		t.Errorf("no agent: %d %q", code, errOut)
	}
	if code, _, _ := runCmd(t, "pause", "--config", cfg); code != 1 {
		t.Errorf("pause without an agent: %d", code)
	}
}

func TestRotateAsksTheRunningAgentOrDoesItItself(t *testing.T) {
	old := controlRequest
	defer func() { controlRequest = old }()
	var asked string
	controlRequest = func(_, cmd string) (*tunnel.Status, error) { asked = cmd; return &tunnel.Status{}, nil }
	cfg, state, key := setup(t, "https://b", "http://127.0.0.1:1", "")
	if code, out, _ := runCmd(t, "rotate", "--config", cfg); code != 0 || asked != "rotate" || !strings.Contains(out, "asked to rotate") {
		t.Errorf("running agent: %d %q %q", code, out, asked)
	}

	// With no agent running the command rotates the keys itself.
	controlRequest = func(string, string) (*tunnel.Status, error) { return nil, errors.New("not running") }
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/challenge"):
			_, _ = w.Write([]byte(`{"nonce":"` + base64.StdEncoding.EncodeToString([]byte("nonce-nonce")) + `"}`))
		default:
			var body struct {
				NewSign   []byte `json:"new_sign_pub"`
				NewEnc    []byte `json:"new_enc_pub"`
				Nonce     []byte `json:"nonce"`
				Signature []byte `json:"signature"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			h := sha256.New()
			h.Write([]byte("ctun.v1 rotate"))
			h.Write(body.Nonce)
			h.Write(body.NewSign)
			h.Write(body.NewEnc)
			if !ed25519.Verify(key.SignPub(), h.Sum(nil), body.Signature) {
				w.WriteHeader(403)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer app.Close()
	cfg2, state2, key2 := setup(t, "https://b", app.URL, "")
	key = key2
	_ = state
	code, out, errOut := runCmd(t, "rotate", "--config", cfg2)
	if code != 0 || !strings.Contains(out, "Rotated") {
		t.Fatalf("direct rotation: %d %q %q", code, out, errOut)
	}
	id, err := tunnel.LoadIdentity(state2)
	if err != nil || len(id.Keys) != 2 {
		t.Errorf("identity after rotation: %v %v", id, err)
	}
	// An app that refuses fails the command.
	cfg3, _, _ := setup(t, "https://b", "http://127.0.0.1:1", "")
	if code, _, _ := runCmd(t, "rotate", "--config", cfg3); code != 1 {
		t.Errorf("unreachable app: %d", code)
	}
	if code, _, _ := runCmd(t, "rotate", "--config", cfg2, "--state-dir", t.TempDir()); code != 2 {
		t.Errorf("not enrolled: %d", code)
	}
}

func TestUpdateCommand(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	binary := []byte("new binary")
	sum := sha256.Sum256(binary)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := tunnel.UpdateManifest{Version: "2.0.0", MinProto: 1, Artifacts: []tunnel.UpdateArtifact{{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: srv.URL + "/bin", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(binary))}}}
		raw, _ := json.Marshal(m)
		switch r.URL.Path {
		case "/connector/stable/manifest.json":
			_, _ = w.Write(raw)
		case "/connector/stable/manifest.json.sig":
			_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw))))
		case "/bin":
			_, _ = w.Write(binary)
		}
	}))
	defer srv.Close()
	tunnel.UpdateKeyHex1 = hex.EncodeToString(pub)
	tunnel.Version = "1.0.0"
	defer func() { tunnel.UpdateKeyHex1, tunnel.Version = "", "dev" }()
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	cfg, _, _ := setup(t, "https://b", "https://a", "")
	t.Setenv("ZUANO_CONNECTOR_RELEASES_URL", srv.URL)
	code, out, _ := runCmd(t, "update", "--check", "--config", cfg)
	if code != 0 || !strings.Contains(out, "2.0.0 is available (major update") {
		t.Errorf("check: %d %q", code, out)
	}
	tunnel.Version = "2.0.0"
	if code, out, _ := runCmd(t, "update", "--check", "--config", cfg); code != 0 || !strings.Contains(out, "up to date") {
		t.Errorf("current: %d %q", code, out)
	}
	tunnel.Version = "1.0.0"
	t.Setenv("ZUANO_CONNECTOR_RELEASES_URL", "")
	if code, _, errOut := runCmd(t, "update", "--check", "--config", cfg); code != 2 || !strings.Contains(errOut, "releases address") {
		t.Errorf("no url: %d %q", code, errOut)
	}
	t.Setenv("ZUANO_CONNECTOR_CONTAINER", "1")
	if code, _, errOut := runCmd(t, "update", "--config", cfg); code != 1 || !strings.Contains(errOut, "immutable") {
		t.Errorf("container: %d %q", code, errOut)
	}
	t.Setenv("ZUANO_CONNECTOR_CONTAINER", "")
	pkg := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(pkg, []byte("version: 1\nnetwork: net_1\nupdate: { method: package, url: "+srv.URL+" }\ntargets: []\n"), 0o644)
	if code, _, errOut := runCmd(t, "update", "--config", pkg); code != 1 || !strings.Contains(errOut, "package manager") {
		t.Errorf("package install: %d %q", code, errOut)
	}
}

// brokerFor accepts agents, answers HELLO and then waits for them to leave.
func brokerFor(t *testing.T, refuse string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hellos atomic.Int32
	up := websocket.Upgrader{Subprotocols: []string{tunnel.Subprotocol}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse != "" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"` + refuse + `"}`))
			return
		}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		if _, _, err := ws.ReadMessage(); err != nil {
			return
		}
		hellos.Add(1)
		ack, _ := tunnel.Frame{Type: tunnel.FrameHelloAck, Payload: []byte(`{"server_time":` + strconv.FormatInt(time.Now().UnixMilli(), 10) + `}`)}.Marshal()
		_ = ws.WriteMessage(websocket.BinaryMessage, ack)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hellos
}

func TestRunConnectsAndDrainsOnShutdown(t *testing.T) {
	broker, hellos := brokerFor(t, "")
	cfg, state, _ := setup(t, broker.URL, "https://a", "")
	ctx, cancel := context.WithCancel(t.Context())
	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"run", "--config", cfg, "--health-addr", "127.0.0.1:0"}, &out, &errOut)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for hellos.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if hellos.Load() == 0 {
		t.Fatalf("the agent never connected: %s %s", out.String(), errOut.String())
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d: %s", code, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	if !strings.Contains(out.String(), `"event":"connected"`) {
		t.Errorf("log = %s", out.String())
	}
	_ = state
}

func TestRunExitCodes(t *testing.T) {
	revoking, _ := brokerFor(t, "agent_revoked")
	cfg, _, _ := setup(t, revoking.URL, "https://a", "")
	if code, _, errOut := runCmd(t, "run", "--config", cfg); code != 3 || !strings.Contains(errOut, "revoked") {
		t.Errorf("revoked: %d %q", code, errOut)
	}
	invalid := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(invalid, []byte("version: 1\nnetwork: net_1\ntargets: [{name: x}]\n"), 0o644)
	if code, _, _ := runCmd(t, "run", "--config", invalid); code != 2 {
		t.Errorf("invalid config: %d", code)
	}
	cfg2, _, _ := setup(t, "https://b", "https://a", "")
	if code, _, errOut := runCmd(t, "run", "--config", cfg2, "--state-dir", t.TempDir()); code != 2 || !strings.Contains(errOut, "not enrolled") {
		t.Errorf("not enrolled: %d %q", code, errOut)
	}
	other := filepath.Join(t.TempDir(), "c.yaml")
	_, state, _ := setup(t, "https://b", "https://a", "")
	_ = os.WriteFile(other, []byte("version: 1\nnetwork: net_other\ntargets: []\n"), 0o644)
	if code, _, errOut := runCmd(t, "run", "--config", other, "--state-dir", state); code != 2 || !strings.Contains(errOut, "net_other") {
		t.Errorf("wrong network: %d %q", code, errOut)
	}
}

func TestRunEnrollsAContainerFromItsEnvironment(t *testing.T) {
	broker, hellos := brokerFor(t, "")
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		jwks := `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k1","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}]}`
		_, _ = w.Write([]byte(`{"agent_id":"nag_c","network_id":"net_c","broker_url":"` + broker.URL + `","gateway_jwks":` + jwks + `}`))
	}))
	defer app.Close()
	dir := t.TempDir()
	cfg, state := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state")
	t.Setenv("ZUANO_CONNECTOR_ENROLL_TOKEN", testToken)
	t.Setenv("ZUANO_CONNECTOR_URL", app.URL)
	ctx, cancel := context.WithCancel(t.Context())
	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"run", "--config", cfg, "--state-dir", state}, &out, &errOut) }()
	deadline := time.Now().Add(10 * time.Second)
	for hellos.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if hellos.Load() == 0 {
		t.Fatalf("no connection after self-enrollment: %s %s", out.String(), errOut.String())
	}
	cancel()
	<-done
	if _, err := os.Stat(filepath.Join(state, tunnel.IdentityFile)); err != nil {
		t.Error(err)
	}
	// A bad token stops the start with a failure.
	t.Setenv("ZUANO_CONNECTOR_ENROLL_TOKEN", "bad")
	if code, _, _ := runCmd(t, "run", "--config", filepath.Join(dir, "c2.yaml"), "--state-dir", filepath.Join(dir, "s2")); code == 0 {
		t.Error("run started with a bad enrollment token")
	}
}

func TestExitFor(t *testing.T) {
	for err, want := range map[error]int{nil: 0, errors.New("x"): 1, tunnel.ErrRevoked: 3, tunnel.ErrNotEnrolled: 2, &tunnel.ErrConfig{Problems: []string{"p"}}: 2} {
		if got := exitFor(err); got != want {
			t.Errorf("exitFor(%v) = %d, want %d", err, got, want)
		}
	}
}
