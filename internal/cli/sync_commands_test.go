package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncCmd_EndToEndThroughResolvedClient(t *testing.T) {
	srv := buildMockServer(t, nil, nil)
	t.Cleanup(srv.Close)
	withAPIKeyGateway(t, srv)
	dir := filepath.Join(t.TempDir(), "market")

	out, err := runCommand(t, SyncCmd(), "--dir", dir)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !strings.Contains(out, "Synced marketplace: acme-corp-plugins") || !strings.Contains(out, "Plugins synced:     2") {
		t.Errorf("unexpected output:\n%s", out)
	}
	assertFileExists(t, filepath.Join(dir, "marketplace.json"))
	assertFileExists(t, filepath.Join(dir, syncStateDir, "searcher-001.json"))
}

func TestSyncCmd_RejectsArgsAndFailsClosed(t *testing.T) {
	if _, err := runCommand(t, SyncCmd(), "extra"); err == nil {
		t.Error("expected positional args to be rejected")
	}
	withNoGateway(t)
	if _, err := runCommand(t, SyncCmd()); err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Errorf("expected offline guidance, got %v", err)
	}
	t.Setenv("RELAY_EMAIL", "")
	t.Setenv("GATEWAY_API_KEY", "")
	if _, err := runCommand(t, SyncCmd(), "--gateway-url", "https://gateway.example.com"); err == nil {
		t.Error("expected a not-logged-in error")
	}
}

// syncServer serves the default mock routes, letting handlers override paths
// by prefix.
func syncServer(t *testing.T, overrides map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	base := buildMockServer(t, nil, nil)
	t.Cleanup(base.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for prefix, h := range overrides {
			if strings.HasPrefix(r.URL.Path, prefix) {
				h(w, r)
				return
			}
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunSync_ErrorPaths(t *testing.T) {
	text := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	cases := []struct {
		name      string
		overrides map[string]http.HandlerFunc
		want      string
	}{
		{"manifest not json", map[string]http.HandlerFunc{"/api/marketplace/manifest": text(200, "{")}, "decode manifest"},
		{"manifest without name", map[string]http.HandlerFunc{"/api/marketplace/manifest": text(200, `{"plugins":[]}`)}, "manifest has no name field"},
		{"plugin server error", map[string]http.HandlerFunc{"/api/marketplace/plugin/": text(500, " exploded ")}, "returned 500: exploded — re-run `relay sync` to retry"},
		{"plugin not json", map[string]http.HandlerFunc{"/api/marketplace/plugin/": text(200, "nope")}, "decode plugin bundle"},
		{"plugin unsafe path", map[string]http.HandlerFunc{"/api/marketplace/plugin/": text(200, `{"files":{"../evil":"x"}}`)}, "unsafe path"},
		{"plugin absolute path", map[string]http.HandlerFunc{"/api/marketplace/plugin/": text(200, `{"files":{"/etc/passwd":"x"}}`)}, "unsafe path"},
		{"settings unauthorized", map[string]http.HandlerFunc{"/api/marketplace/managed-settings": text(401, "")}, "not authenticated"},
		{"settings server error", map[string]http.HandlerFunc{"/api/marketplace/managed-settings": text(503, "later")}, "returned 503: later"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv := syncServer(t, tc.overrides)
			dir := t.TempDir()
			err := runSync(&testDoer{srv: srv}, dir, false, &strings.Builder{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "marketplace.json")); !os.IsNotExist(statErr) {
				t.Error("marketplace.json must not be written after a failed sync")
			}
		})
	}
}

func TestRunSync_PluginWithoutFileMapIsStoredAsPluginJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	body := `{"name":"legacy","version":"1"}`
	srv := syncServer(t, map[string]http.HandlerFunc{
		"/api/marketplace/plugin/": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) },
	})
	dir := t.TempDir()
	if err := runSync(&testDoer{srv: srv}, dir, false, &strings.Builder{}); err != nil {
		t.Fatalf("runSync: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "plugins", "searcher-001", ".claude-plugin", "plugin.json"))
	if err != nil || string(got) != body {
		t.Fatalf("plugin.json = %q (err %v), want raw bundle", got, err)
	}
}

func TestRunSync_DropsOnlyPreviouslyOwnedFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bundle := `{"files":{"a.txt":"a","b.txt":"b"}}`
	srv := syncServer(t, map[string]http.HandlerFunc{
		"/api/marketplace/plugin/": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(bundle)) },
	})
	dir := t.TempDir()
	if err := runSync(&testDoer{srv: srv}, dir, false, &strings.Builder{}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	pluginDir := filepath.Join(dir, "plugins", "searcher-001")
	if err := os.WriteFile(filepath.Join(pluginDir, "mine.txt"), []byte("user"), 0o644); err != nil {
		t.Fatal(err)
	}

	bundle = `{"files":{"a.txt":"a2"}}`
	if err := runSync(&testDoer{srv: srv}, dir, false, &strings.Builder{}); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "b.txt")); !os.IsNotExist(err) {
		t.Error("file dropped from the bundle must be removed")
	}
	if got, _ := os.ReadFile(filepath.Join(pluginDir, "a.txt")); string(got) != "a2" {
		t.Errorf("a.txt = %q, want updated content", got)
	}
	assertFileExists(t, filepath.Join(pluginDir, "mine.txt"))
}

func TestWritePluginBundle_CorruptOwnershipRecordIsAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, syncStateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownedFilesPath(dir, "p"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writePluginBundle(dir, "p", filepath.Join(dir, "plugins", "p"), []byte(`{"files":{"x":"y"}}`))
	if err == nil || !strings.Contains(err.Error(), "parse owned-files record for p") {
		t.Fatalf("got %v", err)
	}

	if err := os.Remove(ownedFilesPath(dir, "p")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ownedFilesPath(dir, "p"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOwnedFiles(dir, "p"); err == nil || !strings.Contains(err.Error(), "read owned-files record") {
		t.Fatalf("unreadable record: got %v", err)
	}
}

func TestSaveOwnedFiles_StateDirBlockedByFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, syncStateDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveOwnedFiles(dir, "p", []string{"a"}); err == nil || !strings.Contains(err.Error(), "create sync state dir") {
		t.Fatalf("got %v", err)
	}
}

func TestWriteFile_Errors(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, "missing", "f.json"), []byte("x")); err == nil || !strings.Contains(err.Error(), "write temp file") {
		t.Errorf("missing parent: got %v", err)
	}
	target := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(target, []byte("x")); err == nil || !strings.Contains(err.Error(), "rename") {
		t.Errorf("rename over non-empty dir: got %v", err)
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file must be cleaned up after a failed rename")
	}
}
