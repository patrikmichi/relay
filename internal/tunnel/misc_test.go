package tunnel

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoggerNeverWritesBodiesQueriesOrHeaderValues(t *testing.T) {
	var out bytes.Buffer
	dir := t.TempDir()
	l, err := NewLogger(&out, "json", filepath.Join(dir, "logs", "exchanges.log"), 1, 3, "metadata")
	if err != nil {
		t.Fatal(err)
	}
	l.Event("info", "exchange", map[string]any{
		"target": "erp", "status": 200, "body": "SECRET-BODY", "query": "ssn=1", "headers": map[string]string{"Authorization": "Bearer x"},
		"Authorization": "Bearer y", "value": "v", "url": "https://internal/x", "token": "t", "path": "/invoices",
	})
	for _, banned := range []string{"SECRET-BODY", "ssn=1", "Bearer", "https://internal", `"token"`} {
		if strings.Contains(out.String(), banned) {
			t.Errorf("the log holds %q: %s", banned, out.String())
		}
	}
	if !strings.Contains(out.String(), `"path":"/invoices"`) || !strings.Contains(out.String(), `"target":"erp"`) {
		t.Errorf("log = %s", out.String())
	}
	file, _ := os.ReadFile(filepath.Join(dir, "logs", "exchanges.log"))
	if !bytes.Equal(bytes.TrimSpace(file), bytes.TrimSpace(out.Bytes())) {
		t.Error("the file and stdout differ")
	}
	up := l.TakeUpload()
	if len(up) != 1 || len(l.TakeUpload()) != 0 {
		t.Errorf("upload = %d lines", len(up))
	}
	_ = l.Close()
}

func TestLoggerUploadModesAndTextFormat(t *testing.T) {
	for upload, want := range map[string]int{"off": 0, "errors": 1, "metadata": 2} {
		l, _ := NewLogger(nil, "json", "", 1, 1, upload)
		l.Event("info", "exchange", nil)
		l.Event("warn", "policy_violation", nil)
		if got := len(l.TakeUpload()); got != want {
			t.Errorf("upload=%s kept %d lines, want %d", upload, got, want)
		}
	}
	var out bytes.Buffer
	l, _ := NewLogger(&out, "text", "", 1, 1, "off")
	l.Event("error", "security", map[string]any{"code": "replay", "b": 2})
	if !strings.Contains(out.String(), "error security b=2 code=replay") {
		t.Errorf("text line = %q", out.String())
	}
	// The upload buffer is bounded.
	l, _ = NewLogger(nil, "json", "", 1, 1, "errors")
	for i := 0; i < 250; i++ {
		l.Event("error", "x", map[string]any{"i": i})
	}
	if n := len(l.TakeUpload()); n != maxUploadLines {
		t.Errorf("buffer = %d", n)
	}
	if err := (&Logger{}).Close(); err != nil {
		t.Error(err)
	}
	if _, err := NewLogger(nil, "json", "/proc/nope/x.log", 1, 1, "off"); err == nil {
		t.Error("an unwritable log file was accepted")
	}
}

func TestRotatingFileKeepsTheNewestFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.log")
	f, err := newRotatingFile(p, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err := f.Write([]byte(strings.Repeat(string(rune('a'+i)), 40) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	for _, name := range []string{"x.log", "x.log.1", "x.log.2"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "x.log.3")); err == nil {
		t.Error("a third rotated file was kept")
	}
	cur, _ := os.ReadFile(p)
	if !strings.Contains(string(cur), "l") {
		t.Errorf("the newest line is not in the current file: %q", cur)
	}
	// keep = 0 discards instead of rotating.
	f2, _ := newRotatingFile(filepath.Join(dir, "y.log"), 10, 0)
	for i := 0; i < 3; i++ {
		_, _ = f2.Write([]byte("0123456789abc\n"))
	}
	_ = f2.Close()
	if _, err := os.Stat(filepath.Join(dir, "y.log.1")); err == nil {
		t.Error("a rotated file exists with keep = 0")
	}
}

func TestControlCommands(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	ask := func(cmd string) (*Status, error) {
		server, client := net.Pipe()
		go r.a.handleControl(server)
		defer client.Close()
		return controlExchange(client, cmd)
	}
	if st, err := ask("status"); err != nil || st.AgentID != "nag_1" || st.Paused {
		t.Fatalf("status = %+v %v", st, err)
	}
	if st, err := ask("pause"); err != nil || !st.Paused || !r.a.Paused() {
		t.Errorf("pause = %+v %v", st, err)
	}
	if st, err := ask("resume"); err != nil || st.Paused {
		t.Errorf("resume = %+v %v", st, err)
	}
	for _, cmd := range []string{"rotate", "reload"} {
		if _, err := ask(cmd); err != nil {
			t.Errorf("%s: %v", cmd, err)
		}
	}
	if _, err := ask("explode"); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("unknown command: %v", err)
	}
	// Something that is not JSON is refused.
	server, client := net.Pipe()
	go r.a.handleControl(server)
	_, _ = client.Write([]byte("nonsense\n"))
	line, _ := bufio.NewReader(client).ReadBytes('\n')
	client.Close()
	if !strings.Contains(string(line), "not JSON") {
		t.Errorf("answer to garbage = %s", line)
	}
	long := &Agent{opts: AgentOptions{StateDir: "/" + strings.Repeat("d", 120)}}
	if err := long.ServeControl(t.Context()); err == nil || !strings.Contains(err.Error(), "shorter") {
		t.Errorf("long path: %v", err)
	}
	if _, err := ControlRequest(filepath.Join(t.TempDir(), "none"), "status"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("no agent: %v", err)
	}
}

func TestControlSocket(t *testing.T) {
	if l, err := net.Listen("unix", filepath.Join(shortDir(t), "probe.sock")); err != nil {
		t.Skipf("unix sockets are not available here: %v", err)
	} else {
		_ = l.Close()
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	r := newRig(t, erpTarget(up.URL, ""), nil)
	ctx := t.Context()
	done := make(chan error, 1)
	go func() { done <- r.a.ServeControl(ctx) }()
	sock := filepath.Join(r.a.stateDir(), ControlSocket)
	r.waitFor("the socket", func() bool {
		select {
		case err := <-done:
			t.Fatalf("ServeControl stopped: %v (%s)", err, sock)
		default:
		}
		_, err := os.Stat(sock)
		return err == nil
	})
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(sock); info.Mode().Perm() != 0o600 {
			t.Errorf("socket mode = %o", info.Mode().Perm())
		}
	}
	st, err := ControlRequest(r.a.stateDir(), "status")
	if err != nil || st.AgentID != "nag_1" || st.Paused {
		t.Fatalf("status = %+v %v", st, err)
	}
	if st, err = ControlRequest(r.a.stateDir(), "pause"); err != nil || !st.Paused || !r.a.Paused() {
		t.Errorf("pause = %+v %v", st, err)
	}
	if st, err = ControlRequest(r.a.stateDir(), "resume"); err != nil || st.Paused {
		t.Errorf("resume = %+v %v", st, err)
	}
	for _, cmd := range []string{"rotate", "reload"} {
		if _, err := ControlRequest(r.a.stateDir(), cmd); err != nil {
			t.Errorf("%s: %v", cmd, err)
		}
	}
	if _, err := ControlRequest(r.a.stateDir(), "explode"); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("unknown command: %v", err)
	}
	if _, err := ControlRequest(filepath.Join(t.TempDir(), "none"), "status"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("no agent: %v", err)
	}
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

type manifestServer struct {
	srv      *httptest.Server
	priv     ed25519.PrivateKey
	manifest UpdateManifest
	binary   []byte
	sigBad   bool
	hits     map[string]int
}

func newManifestServer(t *testing.T, version string) *manifestServer {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_ = pub
	m := &manifestServer{priv: priv, binary: []byte("#!/bin/sh\necho new\n"), hits: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/connector/stable/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		m.hits["manifest"]++
		raw, _ := json.Marshal(m.manifest)
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("/connector/stable/manifest.json.sig", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := json.Marshal(m.manifest)
		sig := ed25519.Sign(m.priv, raw)
		if m.sigBad {
			sig[0] ^= 1
		}
		_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(sig)))
	})
	mux.HandleFunc("/artifact", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(m.binary) })
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	sum := sha256.Sum256(m.binary)
	m.manifest = UpdateManifest{Version: version, MinProto: 1, Artifacts: []UpdateArtifact{{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: m.srv.URL + "/artifact", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(m.binary))}}}
	return m
}

func (m *manifestServer) keys() []ed25519.PublicKey {
	return []ed25519.PublicKey{m.priv.Public().(ed25519.PublicKey)}
}

func TestVersionsAndAutoUpdatePolicy(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
		ok   bool
	}{{"1.4.2", "1.4.1", 1, true}, {"v1.4.2", "1.5.0", -1, true}, {"2.0.0", "2.0.0", 0, true}, {"1.4.2-rc1", "1.4.2", 0, true}, {"dev", "1.0.0", 0, false}, {"1.2", "1.2.0", 0, false}} {
		if got, ok := CompareVersions(tc.a, tc.b); got != tc.want || ok != tc.ok {
			t.Errorf("CompareVersions(%q,%q) = %d,%v", tc.a, tc.b, got, ok)
		}
	}
	for _, tc := range []struct{ from, to, class string }{{"1.4.2", "1.4.3", "patch"}, {"1.4.2", "1.5.0", "minor"}, {"1.4.2", "2.0.0", "major"}} {
		if got := BumpClass(tc.from, tc.to); got != tc.class {
			t.Errorf("BumpClass(%s,%s) = %s", tc.from, tc.to, got)
		}
	}
	for _, tc := range []struct {
		local      bool
		net, class string
		want       bool
	}{
		{true, "patch", "patch", true}, {true, "patch", "minor", false}, {true, "minor", "minor", true}, {true, "minor", "major", false},
		{true, "off", "patch", false}, {true, "", "patch", false}, {false, "minor", "patch", false},
	} {
		if got := AutoUpdateAllowed(tc.local, tc.net, tc.class); got != tc.want {
			t.Errorf("AutoUpdateAllowed(%+v) = %v", tc, got)
		}
	}
}

func TestCheckUpdateVerifiesTheManifest(t *testing.T) {
	m := newManifestServer(t, "1.5.0")
	info, err := CheckUpdate(t.Context(), http.DefaultClient, m.srv.URL, "stable", "1.4.2", m.keys(), false)
	if err != nil || info == nil || info.Manifest.Version != "1.5.0" || info.Class != "minor" {
		t.Fatalf("info = %+v, %v", info, err)
	}
	if info, err = CheckUpdate(t.Context(), http.DefaultClient, m.srv.URL, "stable", "1.5.0", m.keys(), false); err != nil || info != nil {
		t.Errorf("same version: %v %v", info, err)
	}
	if info, err = CheckUpdate(t.Context(), http.DefaultClient, m.srv.URL, "stable", "1.6.0", m.keys(), false); err != nil || info != nil {
		t.Errorf("a downgrade without the flag: %v %v", info, err)
	}
	if info, err = CheckUpdate(t.Context(), http.DefaultClient, m.srv.URL, "stable", "1.6.0", m.keys(), true); err != nil || info == nil {
		t.Errorf("a downgrade with the flag: %v %v", info, err)
	}
	if info, err = CheckUpdate(t.Context(), http.DefaultClient, m.srv.URL, "stable", "dev", m.keys(), false); err != nil || info != nil {
		t.Errorf("a dev build is never updated: %v %v", info, err)
	}
	m.sigBad = true
	if _, err = CheckUpdate(t.Context(), http.DefaultClient, m.srv.URL, "stable", "1.4.2", m.keys(), false); err == nil || err.Error() != ErrUpdateSignature.Error() {
		t.Errorf("a bad signature: %v", err)
	}
	m.sigBad = false
	if _, err = CheckUpdate(t.Context(), http.DefaultClient, m.srv.URL, "stable", "1.4.2", nil, false); err == nil {
		t.Error("no keys compiled in")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err = CheckUpdate(t.Context(), http.DefaultClient, m.srv.URL, "stable", "1.4.2", []ed25519.PublicKey{other}, false); err == nil {
		t.Error("a manifest signed by an unknown key")
	}
	m.manifest.MinProto = 9
	if _, err = CheckUpdate(t.Context(), http.DefaultClient, m.srv.URL, "stable", "1.4.2", m.keys(), false); err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Errorf("min_proto: %v", err)
	}
	m.manifest.MinProto = 1
	m.manifest.Artifacts[0].OS = "plan9"
	if _, err = CheckUpdate(t.Context(), http.DefaultClient, m.srv.URL, "stable", "1.4.2", m.keys(), false); err == nil || !strings.Contains(err.Error(), "no build") {
		t.Errorf("no artifact for this platform: %v", err)
	}
	if _, err = CheckUpdate(t.Context(), http.DefaultClient, "http://updates.example.com", "stable", "1.4.2", m.keys(), false); err == nil {
		t.Error("an update fetched over plain http")
	}
	// A raw signature is accepted as well as base64.
	raw, _ := json.Marshal(m.manifest)
	if err := VerifyManifest(raw, ed25519.Sign(m.priv, raw), m.keys()); err != nil {
		t.Errorf("raw signature: %v", err)
	}
	if len(UpdateKeys()) != 0 {
		t.Error("keys compiled into a test build")
	}
	UpdateKeyHex1 = hex.EncodeToString(m.keys()[0])
	UpdateKeyHex2 = "zz"
	defer func() { UpdateKeyHex1, UpdateKeyHex2 = "", "" }()
	if len(UpdateKeys()) != 1 {
		t.Error("UpdateKeys did not read the compiled-in key")
	}
}

func TestDownloadSwapAndRollback(t *testing.T) {
	m := newManifestServer(t, "1.5.0")
	dir := t.TempDir()
	art := m.manifest.Artifacts[0]
	path, err := Download(t.Context(), http.DefaultClient, art, dir)
	if err != nil {
		t.Fatal(err)
	}
	bad := art
	bad.SHA256 = strings.Repeat("0", 64)
	if _, err := Download(t.Context(), http.DefaultClient, bad, dir); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("sha mismatch: %v", err)
	}
	short := art
	short.Size--
	if _, err := Download(t.Context(), http.DefaultClient, short, dir); err == nil {
		t.Error("size mismatch")
	}
	if _, err := Download(t.Context(), http.DefaultClient, UpdateArtifact{URL: "http://x.example/a"}, dir); err == nil {
		t.Error("plain http artifact")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("failed downloads left files: %d entries", len(entries))
	}
	exe := filepath.Join(dir, "zuano-connector")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Swap(exe, path); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); !bytes.Equal(got, m.binary) {
		t.Errorf("exe = %q", got)
	}
	if got, _ := os.ReadFile(exe + PreviousSuffix); string(got) != "old" || !Pending(exe) {
		t.Error("the old binary was not kept or the swap is not pending")
	}
	if err := Rollback(exe); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" || Pending(exe) {
		t.Errorf("after rollback: %q pending %v", got, Pending(exe))
	}
	if err := Rollback(exe); err == nil {
		t.Error("a second rollback with no previous binary")
	}
	// Confirm clears the pending mark.
	path2, _ := Download(t.Context(), http.DefaultClient, art, dir)
	_ = Swap(exe, path2)
	Confirm(exe)
	if Pending(exe) {
		t.Error("still pending after Confirm")
	}
	if err := Swap(filepath.Join(dir, "missing"), path2); err == nil {
		t.Error("swapping a binary that does not exist")
	}
}

func TestInContainer(t *testing.T) {
	t.Setenv("ZUANO_CONNECTOR_CONTAINER", "1")
	if !InContainer() {
		t.Error("the container marker is ignored")
	}
	t.Setenv("ZUANO_CONNECTOR_CONTAINER", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	if !InContainer() {
		t.Error("Kubernetes is not a container")
	}
}

func TestAgentInstallsAVerifiedUpdateAndRestarts(t *testing.T) {
	m := newManifestServer(t, "9.9.9")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	dir := t.TempDir()
	exe := filepath.Join(dir, "zuano-connector")
	_ = os.WriteFile(exe, []byte("old"), 0o755)
	old := Version
	Version = "1.0.0"
	defer func() { Version = old }()
	r := newRig(t, erpTarget(up.URL, ""), func(o *AgentOptions) { o.Executable = exe; o.UpdateKeys = m.keys() })
	writeFile(t, r.cfgPth, strings.Replace(readFileT(t, r.cfgPth), "log: { upload: errors }", "log: { upload: errors }\nupdate: { auto: true, url: "+m.srv.URL+" }", 1))
	r.waitFor("the reload", func() bool { return r.a.config().Update.URL != "" })
	info, err := r.a.CheckForUpdate(t.Context())
	if err != nil || info == nil || info.Class != "major" {
		t.Fatalf("CheckForUpdate = %+v, %v", info, err)
	}
	// The network's setting vetoes a major update.
	r.a.netAutoUpdate.Store("minor")
	r.a.autoUpdateOnce(t.Context())
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Error("a vetoed update was installed")
	}
	// Allowed: installed, and Run returns ErrRestart.
	if err := r.a.installUpdate(t.Context(), info); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-r.done:
		if !errors.Is(err, ErrRestart) {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop for the update")
	}
	if got, _ := os.ReadFile(exe); !bytes.Equal(got, m.binary) || !Pending(exe) {
		t.Error("the new binary is not in place and pending")
	}
	if r.out.has("updated") == nil {
		t.Error("the update was not logged")
	}
}

func TestAutoUpdateIsSkippedWhereItShouldBe(t *testing.T) {
	m := newManifestServer(t, "9.9.9")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	exe := filepath.Join(t.TempDir(), "zuano-connector")
	_ = os.WriteFile(exe, []byte("old"), 0o755)
	old := Version
	Version = "9.9.0"
	defer func() { Version = old }()
	r := newRig(t, erpTarget(up.URL, ""), func(o *AgentOptions) { o.Executable = exe; o.UpdateKeys = m.keys() })
	r.a.netAutoUpdate.Store("patch")
	// Auto off locally.
	r.a.autoUpdateOnce(t.Context())
	// Auto on, but outside the window.
	w := time.Now().Add(6 * time.Hour).UTC()
	writeFile(t, r.cfgPth, strings.Replace(readFileT(t, r.cfgPth), "log: { upload: errors }", "log: { upload: errors }\nupdate: { auto: true, url: "+m.srv.URL+", window: \""+w.Format("15:04")+"-"+w.Add(time.Hour).Format("15:04")+" UTC\" }", 1))
	r.waitFor("the reload", func() bool { return r.a.config().Update.Auto })
	r.a.autoUpdateOnce(t.Context())
	if m.hits["manifest"] != 0 {
		t.Error("the manifest was fetched outside the window")
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Error("an update was installed")
	}
	// A broken manifest is logged, not fatal.
	m.sigBad = true
	writeFile(t, r.cfgPth, strings.Replace(readFileT(t, r.cfgPth), `window: "`+w.Format("15:04")+"-"+w.Add(time.Hour).Format("15:04")+` UTC"`, "channel: stable", 1))
	r.waitFor("the reload", func() bool { return r.a.config().Update.Window == "" })
	r.a.autoUpdateOnce(t.Context())
	if r.out.has("update_check_failed") == nil {
		t.Error("a bad signature was not logged")
	}
}

func TestPendingUpdateIsConfirmedOnceConnected(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	exe := filepath.Join(t.TempDir(), "zuano-connector")
	_ = os.WriteFile(exe, []byte("new"), 0o755)
	_ = os.WriteFile(exe+PreviousSuffix, []byte("old"), 0o755)
	_ = os.WriteFile(exe+PendingSuffix, []byte("x"), 0o600)
	newRig(t, erpTarget(up.URL, ""), func(o *AgentOptions) { o.Executable = exe })
	deadline := time.Now().Add(5 * time.Second)
	for Pending(exe) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if Pending(exe) {
		t.Error("the update was not confirmed after connecting")
	}
}
