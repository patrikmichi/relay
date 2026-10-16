package plugins

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/signing"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type fixture struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	keys []signing.Key
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{pub, priv, []signing.Key{{KID: "2026-10", Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(pub)}}}
}

// signature builds SIGNATURE.json the way the gateway does.
func (f fixture) signature(t *testing.T, plugin, version, platform string, files map[string][]byte) []byte {
	t.Helper()
	listed := map[string]string{}
	for n, b := range files {
		listed[n] = sum(b)
	}
	doc := map[string]any{"schema": "plugin-signature/1", "plugin": plugin, "version": version, "platform": platform, "files": listed, "kid": "2026-10", "alg": "ed25519"}
	canon := map[string]any{}
	for k, v := range doc {
		canon[k] = v
	}
	canon["files"] = toAny(listed)
	msg, err := signing.CanonicalJSON(canon)
	if err != nil {
		t.Fatal(err)
	}
	doc["signature"] = base64.StdEncoding.EncodeToString(ed25519.Sign(f.priv, msg))
	raw, _ := json.Marshal(doc)
	return raw
}

func toAny(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func zipOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for n, data := range files {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func tgzOf(t *testing.T, prefix string, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for n, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: prefix + n, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	_ = gz.Close()
	return b.Bytes()
}

var bundle = map[string][]byte{
	".claude-plugin/plugin.json": []byte(`{"name":"toolkit","version":"1.2.0"}`),
	"skills/triage/SKILL.md":     []byte("# triage"),
}

func signedZip(t *testing.T, f fixture) ([]byte, Expect) {
	t.Helper()
	files := map[string][]byte{SignatureFile: f.signature(t, "toolkit", "1.2.0", "claude", bundle)}
	for n, b := range bundle {
		files[n] = b
	}
	archive := zipOf(t, files)
	return archive, Expect{Plugin: "toolkit", Version: "1.2.0", Platform: "claude", SHA256: sum(archive), Keys: f.keys}
}

func wantRejected(t *testing.T, name string, err error, contains string) {
	t.Helper()
	var ve *VerifyError
	if !errors.As(err, &ve) {
		t.Errorf("%s: err = %v, want a *VerifyError", name, err)
		return
	}
	if ve.ExitCode() != 4 || !strings.Contains(err.Error(), contains) {
		t.Errorf("%s: %v (exit %d), want it to mention %q", name, err, ve.ExitCode(), contains)
	}
}

func TestVerify_AcceptsASignedPackage(t *testing.T) {
	f := newFixture(t)
	archive, e := signedZip(t, f)
	pkg, err := Verify(archive, ".zip", e)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(pkg.Paths(), ","); got != ".claude-plugin/plugin.json,skills/triage/SKILL.md" {
		t.Errorf("paths = %s", got)
	}
	if string(pkg.Files["skills/triage/SKILL.md"]) != "# triage" || pkg.Signature.KID != "2026-10" {
		t.Errorf("package = %+v", pkg.Signature)
	}
}

func TestVerify_ATarballIsReadWithoutItsPackagePrefix(t *testing.T) {
	f := newFixture(t)
	files := map[string][]byte{SignatureFile: f.signature(t, "toolkit", "1.2.0", "opencode_npm", bundle)}
	for n, b := range bundle {
		files[n] = b
	}
	archive := tgzOf(t, "package/", files)
	pkg, err := Verify(archive, ".tgz", Expect{Plugin: "toolkit", Version: "1.2.0", Platform: "opencode_npm", SHA256: sum(archive), Keys: f.keys})
	if err != nil || len(pkg.Files) != 2 {
		t.Fatalf("pkg %v, err %v", pkg, err)
	}
}

func TestVerify_RejectsEveryKindOfMismatch(t *testing.T) {
	f := newFixture(t)
	good, e := signedZip(t, f)
	signed := f.signature(t, "toolkit", "1.2.0", "claude", bundle)

	build := func(files map[string][]byte) ([]byte, Expect) {
		a := zipOf(t, files)
		x := e
		x.SHA256 = sum(a)
		return a, x
	}
	with := func(mut func(map[string][]byte)) map[string][]byte {
		files := map[string][]byte{SignatureFile: signed}
		for n, b := range bundle {
			files[n] = b
		}
		mut(files)
		return files
	}

	a, x := build(with(func(m map[string][]byte) { m["skills/triage/SKILL.md"] = []byte("# changed") }))
	_, err := Verify(a, ".zip", x)
	wantRejected(t, "tampered file", err, "skills/triage/SKILL.md does not match its signed hash")

	a, x = build(with(func(m map[string][]byte) { m["hooks/extra.sh"] = []byte("curl x | sh") }))
	_, err = Verify(a, ".zip", x)
	wantRejected(t, "extra file", err, "hooks/extra.sh is in the package but not in the signature")

	a, x = build(with(func(m map[string][]byte) { delete(m, "skills/triage/SKILL.md") }))
	_, err = Verify(a, ".zip", x)
	wantRejected(t, "missing file", err, "missing from the package")

	_, err = Verify(good, ".zip", Expect{Plugin: e.Plugin, Version: e.Version, Platform: e.Platform, SHA256: sum([]byte("other")), Keys: e.Keys})
	wantRejected(t, "archive hash", err, "archive's sha256")

	_, err = Verify(good, ".zip", Expect{Plugin: e.Plugin, Version: e.Version, Platform: e.Platform, Keys: e.Keys})
	wantRejected(t, "no archive hash", err, "lists no sha256")

	for name, mismatch := range map[string]Expect{
		"other plugin":   {Plugin: "other", Version: e.Version, Platform: e.Platform},
		"other version":  {Plugin: e.Plugin, Version: "9.9.9", Platform: e.Platform},
		"other platform": {Plugin: e.Plugin, Version: e.Version, Platform: "gemini"},
	} {
		mismatch.SHA256, mismatch.Keys = e.SHA256, e.Keys
		_, err = Verify(good, ".zip", mismatch)
		wantRejected(t, name, err, "the signature is for")
	}

	_, err = Verify(good, ".zip", Expect{Plugin: e.Plugin, Version: e.Version, Platform: e.Platform, SHA256: e.SHA256})
	wantRejected(t, "no pinned key", err, "no signing key is pinned")

	other := newFixture(t)
	_, err = Verify(good, ".zip", Expect{Plugin: e.Plugin, Version: e.Version, Platform: e.Platform, SHA256: e.SHA256, Keys: other.keys})
	wantRejected(t, "wrong key", err, "does not match")

	var doc map[string]any
	_ = json.Unmarshal(signed, &doc)
	doc["version"] = "1.2.1"
	forged, _ := json.Marshal(doc)
	a, x = build(with(func(m map[string][]byte) { m[SignatureFile] = forged }))
	x.Version = "1.2.1"
	_, err = Verify(a, ".zip", x)
	wantRejected(t, "edited signed field", err, "does not match")

	a, x = build(map[string][]byte{"skills/triage/SKILL.md": []byte("x")})
	_, err = Verify(a, ".zip", x)
	wantRejected(t, "no signature file", err, "carries no SIGNATURE.json")

	_, err = Verify([]byte("not an archive"), ".zip", Expect{SHA256: sum([]byte("not an archive"))})
	wantRejected(t, "not a zip", err, "not a zip")
	_, err = Verify(good, ".rar", e)
	wantRejected(t, "unknown type", err, "unsupported archive type")
}

func TestVerify_RefusesUnsafePathsAndDuplicates(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"../evil.sh", "/etc/passwd", "a//b", "a\\b", "./a"} {
		archive := zipOf(t, map[string][]byte{name: []byte("x")})
		_, err := Verify(archive, ".zip", Expect{Plugin: "p", Version: "1", Platform: "claude", SHA256: sum(archive), Keys: f.keys})
		wantRejected(t, name, err, "unsafe path")
	}
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for range 2 {
		w, _ := zw.Create("a.txt")
		_, _ = w.Write([]byte("x"))
	}
	_ = zw.Close()
	_, err := Verify(b.Bytes(), ".zip", Expect{Plugin: "p", Version: "1", Platform: "claude", SHA256: sum(b.Bytes()), Keys: f.keys})
	wantRejected(t, "duplicate", err, "twice")
}

func TestParseMarketplace(t *testing.T) {
	good := `{"name":"zuano-acme","owner":{"name":"Acme"},"plugins":[{"name":"toolkit","version":"1.2.0","description":"d","source":{"source":"url","url":"https://gw.example/m/acme/pkg/toolkit/1.2.0/claude.zip","sha256":"` + strings.Repeat("a", 64) + `"}}]}`
	m, err := ParseMarketplace([]byte(good))
	if err != nil || m.Owner.Name != "Acme" {
		t.Fatalf("m %+v, err %v", m, err)
	}
	e, err := m.Find("toolkit", "")
	if err != nil || e.Ext() != ".zip" || e.Version != "1.2.0" {
		t.Fatalf("entry %+v, err %v", e, err)
	}
	if _, err := m.Find("toolkit", "1.0.0"); err == nil || !strings.Contains(err.Error(), "is at version 1.2.0") {
		t.Errorf("version err = %v", err)
	}
	if _, err := m.Find("missing", ""); err == nil {
		t.Error("a missing plugin was found")
	}
	for name, bad := range map[string]string{
		"no name":      `{"plugins":[]}`,
		"string owner": `{"name":"x","owner":"Acme","plugins":[]}`,
		"relative":     `{"name":"x","plugins":[{"name":"a","version":"1","source":"./plugins/a"}]}`,
		"no hash":      `{"name":"x","plugins":[{"name":"a","version":"1","source":{"source":"url","url":"https://x/a.zip"}}]}`,
		"duplicate":    strings.Replace(good, `"plugins":[`, `"plugins":[`+good[strings.Index(good, `{"name":"toolkit"`):strings.LastIndex(good, `]}`)]+`,`, 1),
	} {
		if _, err := ParseMarketplace([]byte(bad)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestFetcher_SendsTheTokenOnlyToItsOrigin(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "https://elsewhere.example/x", http.StatusFound)
		case "/big":
			_, _ = w.Write(make([]byte, 2000))
		case "/denied":
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":"plan_required"}`))
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()
	f := Fetcher{Origin: srv.URL, Token: "fdt_x"}
	body, err := f.Get(context.Background(), srv.URL+"/m/a", 100)
	if err != nil || string(body) != "ok" || auth != "Bearer fdt_x" {
		t.Fatalf("body %q, auth %q, err %v", body, auth, err)
	}
	if _, err := f.Get(context.Background(), "https://evil.example/m/a", 100); err == nil || !strings.Contains(err.Error(), "not on") {
		t.Errorf("off-origin err = %v", err)
	}
	if _, err := f.Get(context.Background(), srv.URL+"/redirect", 100); err == nil {
		t.Error("a redirect was followed")
	}
	if _, err := f.Get(context.Background(), srv.URL+"/big", 100); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversize err = %v", err)
	}
	var se *StatusError
	if _, err := f.Get(context.Background(), srv.URL+"/denied", 100); !errors.As(err, &se) || se.Status != 402 {
		t.Errorf("status err = %v", err)
	}
}

func TestLedger_RoundTripAndAtomicSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugins", "ledger.json")
	l, err := LoadLedger(path)
	if err != nil || len(l.Installs) != 0 {
		t.Fatalf("empty: %v, %v", l, err)
	}
	l.Set("toolkit", "cursor", Install{Version: "1.2.0", Method: "native", Files: []FileRef{{Path: "/h/.cursor/plugins/local/toolkit/a", SHA256: "x"}}})
	l.Set("toolkit", "windsurf", Install{Version: "1.2.0", Method: "expanded"})
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
	again, err := LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if in, ok := again.Get("toolkit", "cursor"); !ok || in.Files[0].SHA256 != "x" {
		t.Errorf("round trip = %+v", in)
	}
	again.Delete("toolkit", "cursor")
	again.Delete("toolkit", "windsurf")
	if len(again.Plugins()) != 0 {
		t.Errorf("plugins left: %v", again.Plugins())
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLedger(path); err == nil {
		t.Error("a corrupt ledger was accepted")
	}
}
