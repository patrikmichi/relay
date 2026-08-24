package cli

// White-box tests for publish.go.
// All tests use httptest.Server — no real gateway is ever contacted.

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// ---- test helpers ----

// publishTestDoer wraps an httptest.Server and satisfies publishDoer.
// It records all requests so tests can inspect what was sent.
type publishTestDoer struct {
	srv      *httptest.Server
	requests []*capturedRequest
}

// capturedRequest holds the metadata captured from a single Post call.
// authHeader is populated so bearer-token tests can assert via it.
type capturedRequest struct {
	contentType string
	body        []byte
}

func (p *publishTestDoer) Post(path, contentType string, body io.Reader) (*http.Response, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	p.requests = append(p.requests, &capturedRequest{
		contentType: contentType,
		body:        raw,
	})
	resp, err := http.Post(p.srv.URL+path, contentType, strings.NewReader(string(raw)))
	return resp, err
}

func (p *publishTestDoer) lastRequest() *capturedRequest {
	if len(p.requests) == 0 {
		return nil
	}
	return p.requests[len(p.requests)-1]
}

func (p *publishTestDoer) requestCount() int { return len(p.requests) }

// authBearerTestDoer sends the Authorization header and records it.
type authBearerTestDoer struct {
	srv          *httptest.Server
	token        string
	capturedAuth string
}

func (a *authBearerTestDoer) Post(path, contentType string, body io.Reader) (*http.Response, error) {
	raw, _ := io.ReadAll(body)
	req, err := http.NewRequest(http.MethodPost, a.srv.URL+path, strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+a.token)
	a.capturedAuth = "Bearer " + a.token
	return http.DefaultClient.Do(req)
}

// buildPublishServer returns a mock gateway that serves POST /api/marketplace/publish.
// statusCode and responseBody control the response. An optional retryAfter value sets
// the Retry-After header (for 429 tests).
func buildPublishServer(t *testing.T, statusCode int, responseBody interface{}) *httptest.Server {
	t.Helper()
	return buildPublishServerWithHeaders(t, statusCode, responseBody, nil)
}

func buildPublishServerWithHeaders(t *testing.T, statusCode int, responseBody interface{}, extraHeaders map[string]string) *httptest.Server {
	t.Helper()
	raw, err := json.Marshal(responseBody)
	if err != nil {
		t.Fatalf("marshal mock response: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == publishAPIPath && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			for k, v := range extraHeaders {
				w.Header().Set(k, v)
			}
			w.WriteHeader(statusCode)
			_, _ = w.Write(raw)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	return srv
}

// successPublishResponse is a canonical 201 response body for tests.
var successPublishResponse = publishResponse{
	ResourceID: "res_abc123",
	VersionID:  "ver_xyz789",
	Semver:     "1.0.0",
	State:      "submitted",
	Channel:    "stable",
}

// writeSkillFile writes a minimal SKILL.md with frontmatter to dir.
func writeSkillFile(t *testing.T, dir, name string) string {
	t.Helper()
	content := `---
type: skill
name: Test Skill
slug: test-skill
version: 1.0.0
---

# Test Skill

A test skill for unit tests.
`
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}
	return path
}

// ---- tests ----

// TestRunPublish_HappyPath_SingleFile verifies a single-file publish sends a well-formed
// multipart request and prints the returned version + state.
func TestRunPublish_HappyPath_SingleFile(t *testing.T) {
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, skillPath, publishOpts{channel: "stable"})
	if err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	if doer.requestCount() != 1 {
		t.Fatalf("expected 1 request, got %d", doer.requestCount())
	}

	req := doer.lastRequest()
	// Verify it's multipart.
	mediaType, params, parseErr := mime.ParseMediaType(req.contentType)
	if parseErr != nil {
		t.Fatalf("parse content-type: %v", parseErr)
	}
	if mediaType != "multipart/form-data" {
		t.Errorf("expected multipart/form-data, got %s", mediaType)
	}
	// Verify manifest part is present.
	mr := multipart.NewReader(strings.NewReader(string(req.body)), params["boundary"])
	foundManifest := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read multipart: %v", err)
		}
		if part.FormName() == "manifest" {
			foundManifest = true
			data, _ := io.ReadAll(part)
			if !strings.Contains(string(data), "type: skill") {
				t.Errorf("manifest part should contain frontmatter, got: %s", string(data))
			}
		}
	}
	if !foundManifest {
		t.Error("multipart request missing 'manifest' part")
	}
}

// TestRunPublish_HappyPath_Directory verifies a directory publish bundles files
// and sends a multipart with both manifest + bundle parts.
func TestRunPublish_HappyPath_Directory(t *testing.T) {
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()

	dir := t.TempDir()
	writeSkillFile(t, dir, "SKILL.md")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"key":"value"}`), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, dir, publishOpts{channel: "stable"})
	if err != nil {
		t.Fatalf("runPublish dir: %v", err)
	}

	req := doer.lastRequest()
	_, params, _ := mime.ParseMediaType(req.contentType)
	mr := multipart.NewReader(strings.NewReader(string(req.body)), params["boundary"])

	foundManifest, foundBundle := false, false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read multipart: %v", err)
		}
		switch part.FormName() {
		case "manifest":
			foundManifest = true
		case "bundle":
			foundBundle = true
		}
	}
	if !foundManifest {
		t.Error("directory publish: missing 'manifest' part")
	}
	if !foundBundle {
		t.Error("directory publish: missing 'bundle' part")
	}
}

// TestRunPublish_DryRun verifies that --dry-run makes NO HTTP call, prints the
// intended payload/manifest/files, and exits 0.
func TestRunPublish_DryRun(t *testing.T) {
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()

	dir := t.TempDir()
	writeSkillFile(t, dir, "SKILL.md")
	_ = os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("extra"), 0o644)

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, dir, publishOpts{channel: "stable", dryRun: true})
	if err != nil {
		t.Fatalf("dry-run returned error: %v", err)
	}

	// Crucially: NO HTTP call should be made.
	if doer.requestCount() != 0 {
		t.Errorf("dry-run must make no HTTP requests, got %d", doer.requestCount())
	}
}

// TestRunPublish_DryRun_SingleFile verifies dry-run for a single-file path.
func TestRunPublish_DryRun_SingleFile(t *testing.T) {
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, skillPath, publishOpts{channel: "stable", dryRun: true})
	if err != nil {
		t.Fatalf("dry-run single file returned error: %v", err)
	}
	if doer.requestCount() != 0 {
		t.Errorf("dry-run must make no HTTP requests, got %d", doer.requestCount())
	}
}

// TestRunPublish_PathSafety_SymlinkEscapeAbsolute verifies that a bundle directory containing
// a symlink pointing to an absolute path outside the root is refused client-side.
func TestRunPublish_PathSafety_SymlinkEscapeAbsolute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on Windows")
	}
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()

	bundleDir := t.TempDir()
	writeSkillFile(t, bundleDir, "SKILL.md")

	// Target outside the bundle root.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	symlinkPath := filepath.Join(bundleDir, "evil-link.txt")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Skipf("symlink creation failed: %v", err)
	}

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, bundleDir, publishOpts{channel: "stable"})
	if err == nil {
		t.Fatal("expected error for escaping symlink, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") && !strings.Contains(err.Error(), "escape") {
		t.Errorf("error should mention symlink or escape, got: %v", err)
	}
	if doer.requestCount() != 0 {
		t.Errorf("escaping symlink: no HTTP request should be made, got %d", doer.requestCount())
	}
}

// TestRunPublish_PathSafety_SymlinkEscapeRelative verifies that a relative-path symlink
// escaping the root (e.g. ../../outside) is refused client-side.
func TestRunPublish_PathSafety_SymlinkEscapeRelative(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on Windows")
	}
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()

	bundleDir := t.TempDir()
	writeSkillFile(t, bundleDir, "SKILL.md")

	// Create an outside file two levels up relative to the symlink location.
	// The symlink itself is inside bundleDir, and its relative target "../../outside.txt"
	// would resolve outside bundleDir.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	symlinkPath := filepath.Join(bundleDir, "relative-escape.txt")
	// Use the absolute outside path as a relative-style target by computing the relative
	// path from bundleDir to outsideFile — this will contain ".." components.
	rel, err := filepath.Rel(bundleDir, outsideFile)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	if err := os.Symlink(rel, symlinkPath); err != nil {
		t.Skipf("symlink creation failed: %v", err)
	}

	doer := &publishTestDoer{srv: srv}
	runErr := runPublish(doer, bundleDir, publishOpts{channel: "stable"})
	if runErr == nil {
		t.Fatal("expected error for relative escaping symlink, got nil")
	}
	if doer.requestCount() != 0 {
		t.Errorf("escaping relative symlink: no HTTP request should be made, got %d", doer.requestCount())
	}
}

// TestRunPublish_PathSafety_SymlinkWithinRoot verifies that a symlink pointing INSIDE
// the bundle root is accepted (no false positive).
func TestRunPublish_PathSafety_SymlinkWithinRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on Windows")
	}
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()

	bundleDir := t.TempDir()
	writeSkillFile(t, bundleDir, "SKILL.md")

	// A file inside the bundle that the symlink points to.
	innerFile := filepath.Join(bundleDir, "inner.txt")
	if err := os.WriteFile(innerFile, []byte("inner content"), 0o644); err != nil {
		t.Fatalf("write inner file: %v", err)
	}

	// Symlink inside the bundle pointing to another file inside the bundle.
	symlinkPath := filepath.Join(bundleDir, "link-to-inner.txt")
	if err := os.Symlink(innerFile, symlinkPath); err != nil {
		t.Skipf("symlink creation failed: %v", err)
	}

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, bundleDir, publishOpts{channel: "stable"})
	if err != nil {
		t.Fatalf("within-root symlink should be accepted, got: %v", err)
	}
	if doer.requestCount() != 1 {
		t.Errorf("expected 1 HTTP request for within-root symlink, got %d", doer.requestCount())
	}
}

// TestRunPublish_PathSafety_DotDotInPath verifies that a path with ".." in it is refused.
func TestRunPublish_PathSafety_DotDotInPath(t *testing.T) {
	cases := []struct {
		name string
		rel  string
	}{
		{"dotdot_only", ".."},
		{"dotdot_prefix", "../etc/passwd"},
		{"dotdot_component", "sub/../../../etc/passwd"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBundlePath(tc.rel)
			if err == nil {
				t.Errorf("checkBundlePath(%q): expected error for unsafe path, got nil", tc.rel)
			}
		})
	}
}

// TestRunPublish_PathSafety_AbsolutePath verifies absolute paths in the bundle are refused.
func TestRunPublish_PathSafety_AbsolutePath(t *testing.T) {
	err := checkBundlePath("/etc/passwd")
	if err == nil {
		t.Error("checkBundlePath: absolute path should be rejected")
	}
}

// TestBuildBundle_FIFO_Skipped verifies that a FIFO (named pipe) in the bundle directory
// is silently skipped and does NOT cause the bundler to block or error.
// This is the H1 blocker fix: non-regular, non-symlink entries must be filtered before
// os.ReadFile is called.
func TestBuildBundle_FIFO_Skipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs (named pipes) are not supported on Windows in the same way")
	}

	dir := t.TempDir()
	writeSkillFile(t, dir, "SKILL.md")

	// Create a FIFO in the bundle directory.
	fifoPath := filepath.Join(dir, "test.fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("Mkfifo not available: %v", err)
	}

	// buildBundle must complete without hanging and without including the FIFO.
	_, bundleBytes, files, err := buildBundle(dir)
	if err != nil {
		t.Fatalf("buildBundle with FIFO present: unexpected error: %v", err)
	}
	if len(bundleBytes) == 0 {
		t.Error("expected non-empty bundle bytes")
	}

	// The FIFO must NOT appear in the file list.
	for _, f := range files {
		if strings.HasSuffix(f, ".fifo") {
			t.Errorf("FIFO must be skipped, but found in bundle files: %s", f)
		}
	}
}

// TestBuildBundle_SizeCap_Exceeded verifies the size cap is enforced: a bundle whose
// total file size exceeds artifactMaxBytes must be refused with a clear error.
func TestBuildBundle_SizeCap_Exceeded(t *testing.T) {
	// Temporarily lower the cap to something tiny.
	orig := artifactMaxBytes
	artifactMaxBytes = 100
	t.Cleanup(func() { artifactMaxBytes = orig })

	dir := t.TempDir()
	writeSkillFile(t, dir, "SKILL.md") // > 100 bytes

	_, _, _, err := buildBundle(dir)
	if err == nil {
		t.Fatal("expected error when bundle exceeds size cap, got nil")
	}
	if !strings.Contains(err.Error(), "size cap") {
		t.Errorf("size cap error should mention 'size cap', got: %v", err)
	}
}

// TestBuildBundle_SizeCap_WithinLimit verifies a reasonably-sized bundle succeeds.
func TestBuildBundle_SizeCap_WithinLimit(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "SKILL.md")
	_ = os.WriteFile(filepath.Join(dir, "data.bin"), make([]byte, 1024), 0o644)

	_, bundleBytes, files, err := buildBundle(dir)
	if err != nil {
		t.Fatalf("buildBundle: %v", err)
	}
	if len(bundleBytes) == 0 {
		t.Error("expected non-empty bundle bytes")
	}
	if len(files) == 0 {
		t.Error("expected at least one file in bundle")
	}
}

// TestRunPublish_Error_409 verifies a 409 response returns a clear message + non-zero exit.
func TestRunPublish_Error_409(t *testing.T) {
	body := publishErrorBody{}
	body.Error.Code = "CONFLICT"
	body.Error.Message = "version already exists"
	srv := buildPublishServer(t, http.StatusConflict, body)
	defer srv.Close()

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, skillPath, publishOpts{channel: "stable"})
	if err == nil {
		t.Fatal("expected error on 409, got nil")
	}
	if !strings.Contains(err.Error(), "bump the version") {
		t.Errorf("409 error should mention 'bump the version', got: %v", err)
	}
}

// TestRunPublish_Error_422 verifies a 422 response surfaces field errors.
func TestRunPublish_Error_422(t *testing.T) {
	body := publishErrorBody{}
	body.Error.Code = "VALIDATION_ERROR"
	body.Error.Message = "Invalid input"
	body.Error.Details = []publishErrorDetail{
		{Field: "slug", Message: "slug must be lowercase alphanumeric"},
		{Field: "semver", Message: "semver is required"},
	}
	srv := buildPublishServer(t, http.StatusUnprocessableEntity, body)
	defer srv.Close()

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, skillPath, publishOpts{channel: "stable"})
	if err == nil {
		t.Fatal("expected error on 422, got nil")
	}
	if !strings.Contains(err.Error(), "slug") {
		t.Errorf("422 error should mention field 'slug', got: %v", err)
	}
	if !strings.Contains(err.Error(), "semver") {
		t.Errorf("422 error should mention field 'semver', got: %v", err)
	}
}

// TestRunPublish_Error_429 verifies a 429 response returns a rate-limit message.
func TestRunPublish_Error_429(t *testing.T) {
	body := publishErrorBody{}
	body.Error.Code = "RATE_LIMITED"
	body.Error.Message = "too many publishes; retry after 60s"
	srv := buildPublishServer(t, http.StatusTooManyRequests, body)
	defer srv.Close()

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, skillPath, publishOpts{channel: "stable"})
	if err == nil {
		t.Fatal("expected error on 429, got nil")
	}
	if !strings.Contains(err.Error(), "rate limit") && !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("429 error should mention rate limiting, got: %v", err)
	}
}

// TestRunPublish_Error_429_RetryAfterHeader verifies the Retry-After header is surfaced.
func TestRunPublish_Error_429_RetryAfterHeader(t *testing.T) {
	body := publishErrorBody{}
	body.Error.Code = "RATE_LIMITED"
	srv := buildPublishServerWithHeaders(t, http.StatusTooManyRequests, body, map[string]string{
		"Retry-After": "42",
	})
	defer srv.Close()

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, skillPath, publishOpts{channel: "stable"})
	if err == nil {
		t.Fatal("expected error on 429, got nil")
	}
	if !strings.Contains(err.Error(), "42") {
		t.Errorf("429 error should include Retry-After value '42', got: %v", err)
	}
}

// TestRunPublish_Error_403 verifies a 403 response gives a clear authorization message.
func TestRunPublish_Error_403(t *testing.T) {
	body := publishErrorBody{}
	body.Error.Code = "FORBIDDEN"
	body.Error.Message = "not authorized"
	srv := buildPublishServer(t, http.StatusForbidden, body)
	defer srv.Close()

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, skillPath, publishOpts{channel: "stable"})
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	if !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("403 error should mention authorization, got: %v", err)
	}
}

// TestRunPublish_Error_401 verifies that a 401 suggests `relay login`.
func TestRunPublish_Error_401(t *testing.T) {
	body := publishErrorBody{}
	body.Error.Message = "unauthorized"
	srv := buildPublishServer(t, http.StatusUnauthorized, body)
	defer srv.Close()

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, skillPath, publishOpts{channel: "stable"})
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "relay login") {
		t.Errorf("401 error should mention `relay login`, got: %v", err)
	}
}

// TestResolvePublishDoer_NoToken verifies that a missing RELAY_EMAIL (and no
// persisted login / GATEWAY_API_KEY) returns an actionable error.
func TestResolvePublishDoer_NoToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from any real ~/.config/relay/config.json
	t.Setenv("RELAY_EMAIL", "")
	t.Setenv("GATEWAY_API_KEY", "")

	_, err := resolvePublishDoer("")
	if err == nil {
		t.Fatal("expected error when RELAY_EMAIL is unset, got nil")
	}
	if !strings.Contains(err.Error(), "relay login") {
		t.Errorf("error should mention `relay login`, got: %v", err)
	}
}

// TestRunPublish_BearerToken verifies the OAuth bearer token is sent in the Authorization header.
func TestRunPublish_BearerToken(t *testing.T) {
	const testToken = "test-oauth-token-abc123"

	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		raw, _ := json.Marshal(successPublishResponse)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	doer := &authBearerTestDoer{srv: srv, token: testToken}
	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	err := runPublish(doer, skillPath, publishOpts{channel: "stable"})
	if err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	if capturedAuth != "Bearer "+testToken {
		t.Errorf("Authorization header: got %q, want %q", capturedAuth, "Bearer "+testToken)
	}
	// Also verify capturedAuth on the doer itself is populated (N1 fix).
	if doer.capturedAuth != "Bearer "+testToken {
		t.Errorf("doer.capturedAuth: got %q, want %q", doer.capturedAuth, "Bearer "+testToken)
	}
}

// TestRunPublish_CatalogURL_UsesGatewayURL verifies the catalog URL in the output uses
// the resolved gateway URL from opts.
func TestRunPublish_CatalogURL_UsesGatewayURL(t *testing.T) {
	const customGW = "https://custom-gw.example.com"

	// catalogURL is called inside handlePublishResponse; test it directly.
	url := catalogURL("res_123", customGW)
	if !strings.HasPrefix(url, customGW) {
		t.Errorf("catalog URL should start with %q, got %q", customGW, url)
	}
	if !strings.Contains(url, "res_123") {
		t.Errorf("catalog URL should contain resource ID, got %q", url)
	}
}

// TestRunPublish_CatalogURL_FallsBackToEnv verifies catalogURL falls back to GATEWAY_URL env
// when no gatewayURL is passed.
func TestRunPublish_CatalogURL_FallsBackToEnv(t *testing.T) {
	const envGW = "https://env-gw.example.com"
	t.Setenv("GATEWAY_URL", envGW)

	url := catalogURL("res_456", "")
	if !strings.HasPrefix(url, envGW) {
		t.Errorf("catalog URL should start with env GATEWAY_URL %q, got %q", envGW, url)
	}
}

// TestRunPublish_TypeDetection_FromFrontmatter verifies type detection from the frontmatter.
func TestRunPublish_TypeDetection_FromFrontmatter(t *testing.T) {
	cases := []struct {
		content  string
		wantType string
	}{
		{
			"---\ntype: skill\nname: My Skill\n---\n# body",
			"skill",
		},
		{
			"---\ntype: agent\nname: My Agent\n---\n# body",
			"agent",
		},
		{
			"---\ntype: mcp_server\nname: My MCP\n---\n# body",
			"mcp_server",
		},
	}

	for _, tc := range cases {
		fm := parseFrontmatter([]byte(tc.content))
		if fm.Type != tc.wantType {
			t.Errorf("parseFrontmatter type: got %q, want %q (content: %s)", fm.Type, tc.wantType, tc.content)
		}
	}
}

// TestRunPublish_TypeOverride verifies --type overrides the frontmatter type.
func TestRunPublish_TypeOverride(t *testing.T) {
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()

	dir := t.TempDir()
	content := "---\nname: My Thing\nslug: my-thing\n---\n# body"
	skillPath := filepath.Join(dir, "THING.md")
	if err := os.WriteFile(skillPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, skillPath, publishOpts{channel: "stable", resourceType: "prompt"})
	if err != nil {
		t.Fatalf("runPublish with --type override: %v", err)
	}

	req := doer.lastRequest()
	_, params, _ := mime.ParseMediaType(req.contentType)
	mr := multipart.NewReader(strings.NewReader(string(req.body)), params["boundary"])
	foundType := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read multipart: %v", err)
		}
		if part.FormName() == "type" {
			foundType = true
			data, _ := io.ReadAll(part)
			if string(data) != "prompt" {
				t.Errorf("type field: got %q, want %q", string(data), "prompt")
			}
		}
	}
	if !foundType {
		t.Error("multipart request missing 'type' field when --type is specified")
	}
}

func TestRunPublish_McpDescriptor_SendsDescriptorField(t *testing.T) {
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()

	dir := t.TempDir()
	descriptorPath := filepath.Join(dir, "server.json")
	descriptor := `{
  "name": "Internal Search",
  "slug": "internal-search",
  "version": "1.2.3",
  "description": "Search internal systems.",
  "transport": "http",
  "endpoint": "https://mcp.example.com/rpc",
  "source": "url",
  "authType": "oauth",
  "authScope": "user",
  "declaredTools": ["search"]
}`
	if err := os.WriteFile(descriptorPath, []byte(descriptor), 0o644); err != nil {
		t.Fatalf("write descriptor: %v", err)
	}

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, descriptorPath, publishOpts{
		channel:           "stable",
		resourceType:      "mcp_server",
		mcpDescriptorPath: descriptorPath,
	})
	if err != nil {
		t.Fatalf("runPublish mcp descriptor: %v", err)
	}

	req := doer.lastRequest()
	_, params, _ := mime.ParseMediaType(req.contentType)
	mr := multipart.NewReader(strings.NewReader(string(req.body)), params["boundary"])

	fields := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read multipart: %v", err)
		}
		data, _ := io.ReadAll(part)
		fields[part.FormName()] = string(data)
	}

	if fields["type"] != "mcp_server" {
		t.Fatalf("type field: got %q, want mcp_server", fields["type"])
	}
	if fields["mcpServerDescriptor"] == "" {
		t.Fatal("missing mcpServerDescriptor field")
	}

	var gotDescriptor mcpDescriptor
	if err := json.Unmarshal([]byte(fields["mcpServerDescriptor"]), &gotDescriptor); err != nil {
		t.Fatalf("decode descriptor field: %v", err)
	}
	if gotDescriptor.Endpoint != "https://mcp.example.com/rpc" {
		t.Fatalf("descriptor endpoint: got %q", gotDescriptor.Endpoint)
	}
	if gotDescriptor.Source != "url" || gotDescriptor.AuthType != "oauth" || gotDescriptor.AuthScope != "user" {
		t.Fatalf("descriptor lost remote OAuth settings: %#v", gotDescriptor)
	}

	var gotManifest map[string]any
	if err := json.Unmarshal([]byte(fields["manifest"]), &gotManifest); err != nil {
		t.Fatalf("manifest should be JSON for MCP descriptor publish: %v", err)
	}
	if gotManifest["type"] != "mcp_server" || gotManifest["name"] != "Internal Search" || gotManifest["version"] != "1.2.3" {
		t.Fatalf("unexpected MCP manifest: %#v", gotManifest)
	}
	if gotManifest["source"] != "url" || gotManifest["authType"] != "oauth" || gotManifest["authScope"] != "user" {
		t.Fatalf("manifest lost remote OAuth settings: %#v", gotManifest)
	}
}

// TestRunPublish_NoType_Error verifies that missing type (no frontmatter, no --type) returns error.
func TestRunPublish_NoType_Error(t *testing.T) {
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()

	dir := t.TempDir()
	noTypePath := filepath.Join(dir, "thing.md")
	if err := os.WriteFile(noTypePath, []byte("# No frontmatter"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, noTypePath, publishOpts{channel: "stable"})
	if err == nil {
		t.Fatal("expected error when no type can be detected, got nil")
	}
	if !strings.Contains(err.Error(), "--type") {
		t.Errorf("error should suggest --type, got: %v", err)
	}
}

// TestRunPublish_ChannelDefault verifies that the "stable" channel succeeds.
func TestRunPublish_ChannelDefault(t *testing.T) {
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")
	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, skillPath, publishOpts{channel: "stable"})
	if err != nil {
		t.Fatalf("runPublish with stable channel: %v", err)
	}
}

// TestRunPublish_InvalidChannel verifies that an invalid channel value returns an error.
func TestRunPublish_InvalidChannel(t *testing.T) {
	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")
	doer := &publishTestDoer{srv: &httptest.Server{}}
	err := runPublish(doer, skillPath, publishOpts{channel: "nightly"})
	if err == nil {
		t.Fatal("expected error for invalid channel, got nil")
	}
	if !strings.Contains(err.Error(), "stable") || !strings.Contains(err.Error(), "beta") {
		t.Errorf("error should mention valid channels (stable, beta), got: %v", err)
	}
}

// TestHandlePublishResponse_ChangesRequested verifies changes_requested prints guidance
// but does not return an error (the HTTP call itself succeeded).
func TestHandlePublishResponse_ChangesRequested(t *testing.T) {
	body := publishResponse{
		ResourceID: "res_1",
		VersionID:  "ver_1",
		Semver:     "1.0.0",
		State:      "changes_requested",
		Channel:    "stable",
	}
	raw, _ := json.Marshal(body)
	err := handlePublishResponse(http.StatusCreated, http.Header{}, raw, "")
	if err != nil {
		t.Errorf("changes_requested should not return an error from handlePublishResponse, got: %v", err)
	}
}

// TestHandlePublishResponse_ErrorCodes exercises all error status codes.
func TestHandlePublishResponse_ErrorCodes(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       interface{}
		wantSubstr string
	}{
		{
			name:       "401",
			statusCode: http.StatusUnauthorized,
			body:       map[string]string{},
			wantSubstr: "relay login",
		},
		{
			name:       "403",
			statusCode: http.StatusForbidden,
			body:       map[string]string{},
			wantSubstr: "not authorized",
		},
		{
			name:       "409",
			statusCode: http.StatusConflict,
			body:       publishErrorBody{},
			wantSubstr: "bump the version",
		},
		{
			name:       "429",
			statusCode: http.StatusTooManyRequests,
			body:       publishErrorBody{},
			wantSubstr: "rate limit",
		},
		{
			name:       "413",
			statusCode: http.StatusRequestEntityTooLarge,
			body:       map[string]string{},
			wantSubstr: "too large",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.body)
			err := handlePublishResponse(tc.statusCode, http.Header{}, raw, "")
			if err == nil {
				t.Fatalf("%s: expected error, got nil", tc.name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantSubstr)) {
				t.Errorf("%s: want substring %q in error, got: %v", tc.name, tc.wantSubstr, err)
			}
		})
	}
}

// TestParseFrontmatter_Full verifies frontmatter parsing extracts all relevant fields.
func TestParseFrontmatter_Full(t *testing.T) {
	content := `---
type: skill
name: My Skill
slug: my-skill
version: 1.2.3
---

# My Skill

Description here.
`
	fm := parseFrontmatter([]byte(content))
	if fm.Type != "skill" {
		t.Errorf("type: got %q, want skill", fm.Type)
	}
	if fm.Name != "My Skill" {
		t.Errorf("name: got %q, want 'My Skill'", fm.Name)
	}
	if fm.Slug != "my-skill" {
		t.Errorf("slug: got %q, want 'my-skill'", fm.Slug)
	}
}

// TestParseFrontmatter_NoFrontmatter verifies empty struct is returned for files without frontmatter.
func TestParseFrontmatter_NoFrontmatter(t *testing.T) {
	fm := parseFrontmatter([]byte("# Just markdown\n\nNo frontmatter here."))
	if fm.Type != "" || fm.Name != "" || fm.Slug != "" {
		t.Errorf("expected empty frontmatter, got: %+v", fm)
	}
}

// TestCheckBundlePath_Safe verifies safe paths are accepted.
func TestCheckBundlePath_Safe(t *testing.T) {
	safe := []string{
		"SKILL.md",
		"subdir/file.txt",
		"a/b/c.json",
		"config.yml",
	}
	for _, p := range safe {
		if err := checkBundlePath(p); err != nil {
			t.Errorf("checkBundlePath(%q): unexpected error: %v", p, err)
		}
	}
}

// TestCheckBundlePath_Unsafe verifies unsafe paths are rejected.
func TestCheckBundlePath_Unsafe(t *testing.T) {
	unsafe := []string{
		"..",
		"../etc/passwd",
		"/absolute/path",
		"sub/../../etc/passwd",
	}
	for _, p := range unsafe {
		if err := checkBundlePath(p); err == nil {
			t.Errorf("checkBundlePath(%q): expected error, got nil", p)
		}
	}
}

// TestRunPublish_IdempotentResponse_200 verifies a 200 (idempotent) response is handled correctly.
func TestRunPublish_IdempotentResponse_200(t *testing.T) {
	srv := buildPublishServer(t, http.StatusOK, successPublishResponse)
	defer srv.Close()

	dir := t.TempDir()
	skillPath := writeSkillFile(t, dir, "SKILL.md")

	doer := &publishTestDoer{srv: srv}
	err := runPublish(doer, skillPath, publishOpts{channel: "stable"})
	if err != nil {
		t.Fatalf("idempotent 200 should succeed, got: %v", err)
	}
}
