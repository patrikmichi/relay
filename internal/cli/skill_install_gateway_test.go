package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport"
	"github.com/patrikmichi/relay/internal/catalog"
)

func TestTranslateGatewayFetchError_MapsEachSentinel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"no-access", catalog.ErrNoAccess, "don't have access"},
		{"not-found", catalog.ErrNotFound, "relay skill search"},
		{"not-downloadable", catalog.ErrNotDownloadable, "not downloadable"},
		{"unauthorized", catalog.ErrUnauthorized, "no gateway configured"},
		{"other", errBoomTest, "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateGatewayFetchError("res_x", tc.err)
			if got == nil || !strings.Contains(got.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got: %v", tc.want, got)
			}
		})
	}
}

var errBoomTest = &testBoomErr{}

type testBoomErr struct{}

func (*testBoomErr) Error() string { return "boom: unrelated error" }

// ---- test-only canonical bundle builder ----
//
// Mirrors internal/catalog's own test bundle builder but lives here (rather
// than being imported) so this package's e2e tests don't need to export
// catalog's test helpers — a real gzipped tarball of a canonical
// Agent-Skills bundle (SKILL.md + a scripts/ resource), exactly the shape
// the gateway's download endpoint is contracted to serve (design spec §4.6).

func buildCanonicalSkillBundle(t *testing.T, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	files := map[string]string{
		"SKILL.md":       "---\nname: " + name + "\ndescription: triage PRs from the catalog\n---\n\nTriage the PR.\n",
		"scripts/run.sh": "#!/bin/sh\necho triage\n",
	}
	for rel, content := range files {
		hdr := &tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", rel, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write body %s: %v", rel, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func sha256HexOfBundle(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newMockCatalogGateway stands up an httptest.Server that serves bundle at
// GET /api/catalog/skills/<id>/download with the real X-Skill-* headers the
// design spec's download endpoint is contracted to set (§4.6) — sha256,
// version, scan verdict. Requires a bearer Authorization header (any
// non-empty value — GATEWAY_API_KEY bearer auth, matching client.New).
func newMockCatalogGateway(t *testing.T, wantID string, bundle []byte, version string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		wantPath := "/api/catalog/skills/" + wantID + "/download"
		if r.URL.Path != wantPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-Skill-Content-Sha256", sha256HexOfBundle(bundle))
		w.Header().Set("X-Skill-Version", version)
		w.Header().Set("X-Skill-Catalog-Id", wantID)
		w.Header().Set("X-Skill-Trust-Score", "92")
		w.Header().Set("X-Skill-Scan-Verdict", "approved")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSkillInstall_CatalogID_RoundTripToClaude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	bundle := buildCanonicalSkillBundle(t, "pr-triage")
	srv := newMockCatalogGateway(t, "res_abc123", bundle, "1.4.2")

	cmd := SkillInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_abc123", "--to", "claude", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, out.String())
	}

	skillMd := filepath.Join(home, ".claude", "skills", "pr-triage", "SKILL.md")
	if _, err := os.Stat(skillMd); err != nil {
		t.Fatalf("expected %s to be written: %v\noutput:\n%s", skillMd, err, out.String())
	}
	runSh := filepath.Join(home, ".claude", "skills", "pr-triage", "scripts", "run.sh")
	if _, err := os.Stat(runSh); err != nil {
		t.Fatalf("expected resource %s to be written: %v", runSh, err)
	}

	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entry, ok := agentport.LastEntryFor(m, "pr-triage", agentport.ProviderClaude, agentport.ScopeUser, agentport.KindSkill)
	if !ok {
		t.Fatalf("expected a manifest entry for pr-triage/claude/user")
	}
	if entry.Provenance.CatalogID != "res_abc123" {
		t.Errorf("expected manifest Provenance.CatalogID res_abc123, got %q", entry.Provenance.CatalogID)
	}
	if entry.Provenance.Version != "1.4.2" {
		t.Errorf("expected manifest Provenance.Version 1.4.2, got %q", entry.Provenance.Version)
	}
	if string(entry.Provenance.SourceProvider) != "gateway" {
		t.Errorf("expected manifest Provenance.SourceProvider gateway, got %q", entry.Provenance.SourceProvider)
	}
}

func TestSkillInstall_CatalogID_FanOutToTwoTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	bundle := buildCanonicalSkillBundle(t, "pr-triage")
	srv := newMockCatalogGateway(t, "res_fanout", bundle, "2.0.0")

	cmd := SkillInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_fanout", "--to", "codex", "--to", "cursor", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, out.String())
	}

	codexPath := filepath.Join(home, ".agents", "skills", "pr-triage", "SKILL.md")
	if _, err := os.Stat(codexPath); err != nil {
		t.Fatalf("expected codex placement %s: %v\noutput:\n%s", codexPath, err, out.String())
	}
	cursorPath := filepath.Join(home, ".cursor", "skills", "pr-triage", "SKILL.md")
	if _, err := os.Stat(cursorPath); err != nil {
		t.Fatalf("expected cursor placement %s: %v\noutput:\n%s", cursorPath, err, out.String())
	}

	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if _, ok := agentport.LastEntryFor(m, "pr-triage", agentport.ProviderCodex, agentport.ScopeUser, agentport.KindSkill); !ok {
		t.Errorf("expected a manifest entry for pr-triage/codex/user")
	}
	if _, ok := agentport.LastEntryFor(m, "pr-triage", agentport.ProviderCursor, agentport.ScopeUser, agentport.KindSkill); !ok {
		t.Errorf("expected a manifest entry for pr-triage/cursor/user")
	}
}

func TestSkillInstall_CatalogID_ChecksumMismatch_NonZeroExitNoWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	bundle := buildCanonicalSkillBundle(t, "pr-triage")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Skill-Content-Sha256", "0000000000000000000000000000000000000000000000000000000000000000")
		w.Header().Set("X-Skill-Scan-Verdict", "approved")
		w.Header().Set("X-Skill-Version", "1.0.0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	}))
	t.Cleanup(srv.Close)

	cmd := SkillInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_bad_checksum", "--to", "claude", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected a non-nil error for a checksum mismatch")
	}

	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "pr-triage")); !os.IsNotExist(err) {
		t.Fatalf("expected nothing written on checksum mismatch, stat err = %v", err)
	}
}

func TestSkillInstall_CatalogID_BadScanVerdict_NonZeroExitNoWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	bundle := buildCanonicalSkillBundle(t, "pr-triage")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Skill-Content-Sha256", sha256HexOfBundle(bundle))
		w.Header().Set("X-Skill-Scan-Verdict", "failed")
		w.Header().Set("X-Skill-Version", "1.0.0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	}))
	t.Cleanup(srv.Close)

	cmd := SkillInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_bad_verdict", "--to", "claude", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected a non-nil error for a failed scan verdict")
	}

	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "pr-triage")); !os.IsNotExist(err) {
		t.Fatalf("expected nothing written on a failed scan verdict, stat err = %v", err)
	}
}

func TestSkillInstall_CatalogID_NoGatewayConfigured_FailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Deliberately no GATEWAY_API_KEY / RELAY_EMAIL / keychain session, and
	// no --gateway-url override — client.Resolve must fail with
	// ErrNotLoggedIn, and runSkillInstall must translate that into the
	// offline degradation message rather than attempting a network call.

	cmd := SkillInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_never_reached"})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected an error with no gateway configured")
	}

	if _, statErr := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(statErr) {
		t.Fatalf("expected zero files written, stat err = %v", statErr)
	}
}

// TestSkillInstall_CatalogID_OfflineFlag_FailsClosed exercises the root
// `--offline` persistent flag's effect on a catalog-id install. --offline
// is now registered on the cobra root (cmd/relay/main.go) rather than on
// SkillInstallCmd itself, so a standalone SkillInstallCmd() — as every test
// in this file constructs, without the root's PersistentPreRunE ever
// running — can't set it via a command-line flag. Instead this simulates
// the root flag's effect directly via SetOffline, mirroring what
// PersistentPreRunE does after parsing --offline.
func TestSkillInstall_CatalogID_OfflineFlag_FailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token") // even with creds available...

	SetOffline(true)
	t.Cleanup(func() { SetOffline(false) })

	cmd := SkillInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_never_reached"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected --offline to refuse a catalog-id install even with credentials configured")
	}
}

func TestSkillInstall_CatalogID_VersionAndChannelForwarded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	bundle := buildCanonicalSkillBundle(t, "pr-triage")
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("X-Skill-Content-Sha256", sha256HexOfBundle(bundle))
		w.Header().Set("X-Skill-Scan-Verdict", "approved")
		w.Header().Set("X-Skill-Version", "1.4.2")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	}))
	t.Cleanup(srv.Close)

	cmd := SkillInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_versioned", "--to", "claude", "--gateway-url", srv.URL, "--version", "1.4.2", "--channel", "beta"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, out.String())
	}
	if gotQuery != "channel=beta&version=1.4.2" {
		t.Errorf("expected version/channel query params forwarded, got %q", gotQuery)
	}
}
