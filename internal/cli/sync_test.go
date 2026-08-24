package cli

// White-box tests for sync.go.  These live in the same package so they can
// access the unexported helpers (syncCmdWithDoer, runSync, pluginIDFromSource,
// stalePluginDirs, etc.) directly.
//
// All tests use an httptest.Server — no real gateway is ever contacted.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- httptest adapter ----

// testDoer wraps an *httptest.Server and satisfies syncDoer.
// N8: records ALL request paths so tests can assert routing (e.g. dry-run
// must never issue a /plugin/ fetch).
type testDoer struct {
	srv   *httptest.Server
	paths []string // all paths requested so far
}

func (td *testDoer) Get(path string) (*http.Response, error) {
	td.paths = append(td.paths, path)
	return http.Get(td.srv.URL + path)
}

// lastPath returns the most recently requested path (for backward compat).
func (td *testDoer) lastPath() string {
	if len(td.paths) == 0 {
		return ""
	}
	return td.paths[len(td.paths)-1]
}

// ---- fixtures ----

var testManifest = marketplaceManifest{
	Name:  "acme-corp-plugins",
	Owner: "acme-corp",
	Plugins: []manifestPlugin{
		{Name: "searcher", Source: "./plugins/searcher-001", Version: "1.2.3"},
		{Name: "formatter", Source: "./plugins/fmt-002", Version: "0.9.0"},
	},
}

func marshalJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// buildMockServer returns an httptest.Server that serves:
//
//	GET /api/marketplace/manifest              → testManifest (or customManifest if non-nil)
//	GET /api/marketplace/plugin/<id>/<rev>     → materialisable plugin file bundle
//	GET /api/marketplace/managed-settings      → {"extraKnownMarketplaces":{},"enabledPlugins":[]}
//
// Callers can override response codes per-path via the overrides map.
func buildMockServer(t *testing.T, manifestOverride interface{}, statusOverrides map[string]int) *httptest.Server {
	t.Helper()
	mf := manifestOverride
	if mf == nil {
		mf = testManifest
	}
	rawMf := marshalJSON(t, mf)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if code, ok := statusOverrides[path]; ok {
			w.WriteHeader(code)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		switch {
		case path == "/api/marketplace/manifest":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(rawMf)

		case strings.HasPrefix(path, "/api/marketplace/plugin/"):
			// Extract /<id>/<rev> from /api/marketplace/plugin/<id>/<rev>
			parts := strings.TrimPrefix(path, "/api/marketplace/plugin/")
			bundle := map[string]map[string]string{
				"files": {
					".claude-plugin/plugin.json": `{"name":"` + parts + `","version":"mock"}`,
					".mcp.json":                  `{"mcpServers":{}}`,
				},
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(marshalJSON(t, bundle))

		case strings.HasPrefix(path, "/api/marketplace/managed-settings"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"extraKnownMarketplaces":{},"enabledPlugins":[]}`))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv
}

// ---- tests ----

// TestRunSync_HappyPath verifies that a successful sync creates the expected files.
func TestRunSync_HappyPath(t *testing.T) {
	srv := buildMockServer(t, nil, nil)
	defer srv.Close()

	dir := t.TempDir()
	doer := &testDoer{srv: srv}

	if err := runSync(doer, dir, false); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	// marketplace.json
	mfPath := filepath.Join(dir, "marketplace.json")
	assertFileExists(t, mfPath)
	raw, _ := os.ReadFile(mfPath)
	var got marketplaceManifest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse marketplace.json: %v", err)
	}
	if got.Name != testManifest.Name {
		t.Errorf("manifest name: got %q, want %q", got.Name, testManifest.Name)
	}

	// plugin files
	for _, p := range testManifest.Plugins {
		id, _ := safePluginID(p.Source)
		pluginJSON := filepath.Join(dir, "plugins", id, ".claude-plugin", "plugin.json")
		assertFileExists(t, pluginJSON)
		assertFileExists(t, filepath.Join(dir, "plugins", id, ".mcp.json"))
	}

	// managed-settings.fragment.json
	msPath := filepath.Join(dir, "managed-settings.fragment.json")
	assertFileExists(t, msPath)
}

// TestRunSync_DryRun verifies that --dry-run writes nothing and issues no /plugin/ fetches.
func TestRunSync_DryRun(t *testing.T) {
	srv := buildMockServer(t, nil, nil)
	defer srv.Close()

	dir := t.TempDir()
	doer := &testDoer{srv: srv}

	if err := runSync(doer, dir, true); err != nil {
		t.Fatalf("runSync dry-run: %v", err)
	}

	// marketplace.json must NOT be written
	mfPath := filepath.Join(dir, "marketplace.json")
	if _, err := os.Stat(mfPath); !os.IsNotExist(err) {
		t.Errorf("dry-run: marketplace.json should not exist, but it does (or stat error: %v)", err)
	}

	// plugin dirs must NOT be written
	for _, p := range testManifest.Plugins {
		id, _ := safePluginID(p.Source)
		pluginDir := filepath.Join(dir, "plugins", id)
		if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
			t.Errorf("dry-run: plugin dir %s should not exist", id)
		}
	}

	// managed-settings must NOT be written
	msPath := filepath.Join(dir, "managed-settings.fragment.json")
	if _, err := os.Stat(msPath); !os.IsNotExist(err) {
		t.Errorf("dry-run: managed-settings.fragment.json should not exist")
	}

	// N8: verify no /plugin/ fetch was issued during dry-run.
	for _, p := range doer.paths {
		if strings.HasPrefix(p, "/api/marketplace/plugin/") {
			t.Errorf("dry-run must not fetch plugins, but got request to %s", p)
		}
	}
}

// TestRunSync_Revocation verifies that a plugin dir absent from the new manifest is removed.
func TestRunSync_Revocation(t *testing.T) {
	srv := buildMockServer(t, nil, nil)
	defer srv.Close()

	dir := t.TempDir()

	// Pre-create a stale plugin dir (not in the manifest).
	staleDir := filepath.Join(dir, "plugins", "old-revoked-plugin")
	if err := os.MkdirAll(staleDir, 0o700); err != nil {
		t.Fatalf("create stale dir: %v", err)
	}

	// Also pre-create a dir that IS in the manifest — it should survive.
	existingID, _ := safePluginID(testManifest.Plugins[0].Source)
	existingDir := filepath.Join(dir, "plugins", existingID)
	if err := os.MkdirAll(existingDir, 0o700); err != nil {
		t.Fatalf("create existing dir: %v", err)
	}

	doer := &testDoer{srv: srv}
	if err := runSync(doer, dir, false); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	// Stale dir must be removed.
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Errorf("stale plugin dir should have been removed")
	}

	// Manifest plugin dir must still exist.
	for _, p := range testManifest.Plugins {
		id, _ := safePluginID(p.Source)
		pluginDir := filepath.Join(dir, "plugins", id)
		if _, err := os.Stat(pluginDir); err != nil {
			t.Errorf("plugin dir %s should exist after sync: %v", id, err)
		}
	}
}

// TestRunSync_DryRun_RevocationReported verifies dry-run reports stale dirs without deleting.
func TestRunSync_DryRun_RevocationReported(t *testing.T) {
	srv := buildMockServer(t, nil, nil)
	defer srv.Close()

	dir := t.TempDir()

	// Pre-create a stale plugin dir.
	staleDir := filepath.Join(dir, "plugins", "old-revoked-plugin")
	if err := os.MkdirAll(staleDir, 0o700); err != nil {
		t.Fatalf("create stale dir: %v", err)
	}

	doer := &testDoer{srv: srv}
	if err := runSync(doer, dir, true); err != nil {
		t.Fatalf("runSync dry-run: %v", err)
	}

	// Stale dir must NOT be removed by dry-run.
	if _, err := os.Stat(staleDir); err != nil {
		t.Errorf("dry-run must NOT remove stale plugin dir: %v", err)
	}
}

// TestRunSync_401_Manifest verifies that a 401 on manifest fetch returns an actionable error.
func TestRunSync_401_Manifest(t *testing.T) {
	srv := buildMockServer(t, nil, map[string]int{
		"/api/marketplace/manifest": http.StatusUnauthorized,
	})
	defer srv.Close()

	dir := t.TempDir()
	doer := &testDoer{srv: srv}

	err := runSync(doer, dir, false)
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "relay login") {
		t.Errorf("401 error should mention `relay login`, got: %v", err)
	}
}

// TestRunSync_NonOK_Manifest verifies that a non-200 manifest response returns a wrapped error.
func TestRunSync_NonOK_Manifest(t *testing.T) {
	srv := buildMockServer(t, nil, map[string]int{
		"/api/marketplace/manifest": http.StatusServiceUnavailable,
	})
	defer srv.Close()

	dir := t.TempDir()
	doer := &testDoer{srv: srv}

	err := runSync(doer, dir, false)
	if err == nil {
		t.Fatal("expected error on 503, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention status 503, got: %v", err)
	}
}

// TestRunSync_OrgNamespacing verifies the default dir is namespaced by manifest name.
func TestRunSync_OrgNamespacing(t *testing.T) {
	mfA := marketplaceManifest{
		Name:    "org-alpha",
		Plugins: []manifestPlugin{},
	}
	mfB := marketplaceManifest{
		Name:    "org-beta",
		Plugins: []manifestPlugin{},
	}

	dirA, err := resolveLocalDir("", mfA.Name)
	if err != nil {
		t.Fatalf("resolveLocalDir A: %v", err)
	}
	dirB, err := resolveLocalDir("", mfB.Name)
	if err != nil {
		t.Fatalf("resolveLocalDir B: %v", err)
	}

	if dirA == dirB {
		t.Errorf("two orgs should not share the same local dir: both resolve to %q", dirA)
	}
	if !strings.HasSuffix(dirA, "org-alpha") {
		t.Errorf("org-alpha dir should end with 'org-alpha', got %q", dirA)
	}
	if !strings.HasSuffix(dirB, "org-beta") {
		t.Errorf("org-beta dir should end with 'org-beta', got %q", dirB)
	}
}

// TestResolveSyncDoer_NoToken verifies that missing RELAY_EMAIL (and no
// persisted login / GATEWAY_API_KEY) returns an actionable error.
func TestResolveSyncDoer_NoToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from any real ~/.config/relay/config.json
	t.Setenv("RELAY_EMAIL", "")
	t.Setenv("GATEWAY_API_KEY", "")

	_, err := resolveSyncDoer("")
	if err == nil {
		t.Fatal("expected error when RELAY_EMAIL is unset, got nil")
	}
	if !strings.Contains(err.Error(), "relay login") {
		t.Errorf("error should mention `relay login`, got: %v", err)
	}
}

// TestPluginIDFromSource verifies source-path → plugin-ID extraction.
func TestPluginIDFromSource(t *testing.T) {
	cases := []struct {
		source string
		want   string
	}{
		{"./plugins/searcher-001", "searcher-001"},
		{"plugins/fmt-002", "fmt-002"},
		{"./plugins/my-plugin", "my-plugin"},
	}
	for _, tc := range cases {
		got := pluginIDFromSource(tc.source)
		if got != tc.want {
			t.Errorf("pluginIDFromSource(%q): got %q, want %q", tc.source, got, tc.want)
		}
	}
}

// TestStalePluginDirs verifies that only dirs absent from the manifest are flagged.
func TestStalePluginDirs(t *testing.T) {
	dir := t.TempDir()

	// Create plugins dir with two entries: one in manifest, one not.
	_ = os.MkdirAll(filepath.Join(dir, "plugins", "searcher-001"), 0o700)
	_ = os.MkdirAll(filepath.Join(dir, "plugins", "old-plugin"), 0o700)

	manifest := marketplaceManifest{
		Name: "test",
		Plugins: []manifestPlugin{
			{Source: "./plugins/searcher-001", Version: "1.0"},
		},
	}

	stale, err := stalePluginDirs(dir, manifest)
	if err != nil {
		t.Fatalf("stalePluginDirs: %v", err)
	}
	if len(stale) != 1 || stale[0] != "old-plugin" {
		t.Errorf("stale dirs: got %v, want [old-plugin]", stale)
	}
}

// TestStalePluginDirs_NoPluginsDir verifies no error when plugins dir doesn't exist yet.
func TestStalePluginDirs_NoPluginsDir(t *testing.T) {
	dir := t.TempDir()
	manifest := marketplaceManifest{Name: "test", Plugins: nil}

	stale, err := stalePluginDirs(dir, manifest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("expected no stale dirs, got %v", stale)
	}
}

// TestRunSync_EmptyManifest verifies a manifest with zero plugins doesn't fail.
func TestRunSync_EmptyManifest(t *testing.T) {
	emptyMf := marketplaceManifest{Name: "empty-org", Plugins: []manifestPlugin{}}
	srv := buildMockServer(t, emptyMf, nil)
	defer srv.Close()

	dir := t.TempDir()
	doer := &testDoer{srv: srv}

	if err := runSync(doer, dir, false); err != nil {
		t.Fatalf("runSync empty manifest: %v", err)
	}

	assertFileExists(t, filepath.Join(dir, "marketplace.json"))
	assertFileExists(t, filepath.Join(dir, "managed-settings.fragment.json"))
}

// ---- Security tests (T1–T4) ----

// T1: Manifest with a path-traversal plugin source must be rejected and must write NO files.
func TestRunSync_PathTraversal_PluginSource(t *testing.T) {
	traversalCases := []struct {
		name   string
		source string
	}{
		{"dotdot_ssh", "./plugins/../../.ssh/authorized_keys"},
		{"dotdot_etc", "./plugins/../../../etc/passwd"},
		{"slash_in_id", "./plugins/a/b"},
		{"backslash_in_id", `./plugins/a\b`},
		{"dotdot_segment", "./plugins/foo..bar/../evil"},
	}

	for _, tc := range traversalCases {
		t.Run(tc.name, func(t *testing.T) {
			maliciousManifest := marketplaceManifest{
				Name: "safe-org",
				Plugins: []manifestPlugin{
					{Name: "evil", Source: tc.source, Version: "1.0"},
				},
			}
			srv := buildMockServer(t, maliciousManifest, nil)
			defer srv.Close()

			dir := t.TempDir()
			doer := &testDoer{srv: srv}

			err := runSync(doer, dir, false)
			if err == nil {
				t.Fatalf("source %q: expected rejection error, got nil", tc.source)
			}

			// No files should have been written (marketplace.json would be written
			// before plugins are processed — verify the *plugins* dir is absent).
			pluginsDir := filepath.Join(dir, "plugins")
			if _, statErr := os.Stat(pluginsDir); !os.IsNotExist(statErr) {
				t.Errorf("source %q: plugins dir must not exist after rejection", tc.source)
			}
		})
	}
}

// T2: Manifest with a path-unsafe Name must be rejected before any file is written.
func TestRunSync_PathTraversal_ManifestName(t *testing.T) {
	nameCases := []struct {
		name   string
		mfName string
	}{
		{"dotdot_relative", "../../evil"},
		{"slash_component", "foo/bar"},
		{"backslash", `foo\bar`},
	}

	for _, tc := range nameCases {
		t.Run(tc.name, func(t *testing.T) {
			maliciousManifest := marketplaceManifest{
				Name:    tc.mfName,
				Plugins: []manifestPlugin{},
			}
			srv := buildMockServer(t, maliciousManifest, nil)
			defer srv.Close()

			dir := t.TempDir()
			doer := &testDoer{srv: srv}

			err := runSync(doer, dir, false)
			if err == nil {
				t.Fatalf("manifest name %q: expected rejection error, got nil", tc.mfName)
			}

			// The custom --dir path was supplied, so resolveLocalDir won't use the
			// manifest name — but resolveLocalDir IS called with the name when
			// customDir is "". Test the validation directly too.
			_, resolveErr := resolveLocalDir("", tc.mfName)
			if resolveErr == nil {
				t.Errorf("resolveLocalDir with unsafe name %q should fail", tc.mfName)
			}
		})
	}
}

// T3: Plugin endpoint returns 401 → error mentions `relay login`.
func TestRunSync_401_Plugin(t *testing.T) {
	// We need a manifest with one plugin and the /plugin/ endpoint to return 401.
	// url.PathEscape("searcher-001") == "searcher-001" and "1.2.3" == "1.2.3",
	// so the path the client will request is exactly:
	//   /api/marketplace/plugin/searcher-001/1.2.3
	srv := buildMockServer(t, nil, map[string]int{
		"/api/marketplace/plugin/searcher-001/1.2.3": http.StatusUnauthorized,
	})
	defer srv.Close()

	dir := t.TempDir()
	doer := &testDoer{srv: srv}

	err := runSync(doer, dir, false)
	if err == nil {
		t.Fatal("expected error on plugin 401, got nil")
	}
	if !strings.Contains(err.Error(), "relay login") {
		t.Errorf("plugin 401 error should mention `relay login`, got: %v", err)
	}
}

// T4: safePluginID rejects empty or path-unsafe IDs (unit-level).
func TestSafePluginID(t *testing.T) {
	cases := []struct {
		source  string
		wantErr bool
		wantID  string
	}{
		// Valid
		{"./plugins/searcher-001", false, "searcher-001"},
		{"./plugins/fmt-002", false, "fmt-002"},
		{"plugins/my-plugin", false, "my-plugin"},
		// Invalid
		{"./plugins/../../.ssh/authorized_keys", true, ""},
		{"./plugins/a/b", true, ""},
		{`./plugins/a\b`, true, ""},
		{"./plugins/", true, ""},   // empty after stripping
		{"./plugins/..", true, ""}, // dotdot only
		{"./plugins/foo..bar", true, ""},
	}

	for _, tc := range cases {
		id, err := safePluginID(tc.source)
		if tc.wantErr {
			if err == nil {
				t.Errorf("safePluginID(%q): expected error, got id %q", tc.source, id)
			}
		} else {
			if err != nil {
				t.Errorf("safePluginID(%q): unexpected error: %v", tc.source, err)
			}
			if id != tc.wantID {
				t.Errorf("safePluginID(%q): got %q, want %q", tc.source, id, tc.wantID)
			}
		}
	}
}

// ---- helpers ----

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file to exist: %s (%v)", path, err)
	}
}
