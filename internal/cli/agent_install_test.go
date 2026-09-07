package cli

// White-box tests for agent_install.go — mirrors
// skill_install_gateway_test.go's shape for the Agent-IR install path.
// All network tests use httptest.Server; no live gateway is ever contacted.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport"
)

// buildCanonicalAgentBundle builds a real gzipped tarball containing a
// single root-level "<name>.md" Claude-shape agent file — the shape
// catalog.FetchAgent's bundle-shape assumption expects (agent_source.go).
func buildCanonicalAgentBundle(t *testing.T, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	content := "---\nname: " + name + "\ndescription: reviews pull requests from the catalog\nmodel: sonnet\n---\n\nYou review PRs.\n"
	hdr := &tar.Header{Name: name + ".md", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// newMockAgentCatalogGateway stands up an httptest.Server serving the
// generalized resources download endpoint
// (GET /api/catalog/resources/<id>/download) with the real
// X-Resource-Content-Sha256 (plus legacy X-Skill-Content-Sha256 alias)
// headers the D2 gateway route is contracted to set. Requires a bearer
// Authorization header (any non-empty value — GATEWAY_API_KEY bearer auth).
func newMockAgentCatalogGateway(t *testing.T, wantID string, bundle []byte, version string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		wantPath := "/api/catalog/resources/" + wantID + "/download"
		if r.URL.Path != wantPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-Resource-Content-Sha256", sha256HexOfBundle(bundle))
		w.Header().Set("X-Skill-Content-Sha256", sha256HexOfBundle(bundle))
		w.Header().Set("X-Skill-Version", version)
		w.Header().Set("X-Skill-Catalog-Id", wantID)
		w.Header().Set("X-Skill-Trust-Score", "88")
		w.Header().Set("X-Skill-Scan-Verdict", "approved")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAgentInstall_CatalogID_RoundTripToClaude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	bundle := buildCanonicalAgentBundle(t, "pr-reviewer")
	srv := newMockAgentCatalogGateway(t, "res_agent123", bundle, "1.2.0")

	cmd := AgentInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_agent123", "--to", "claude", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, out.String())
	}

	agentMd := filepath.Join(home, ".claude", "agents", "pr-reviewer.md")
	if _, err := os.Stat(agentMd); err != nil {
		t.Fatalf("expected %s to be written: %v\noutput:\n%s", agentMd, err, out.String())
	}

	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entry, ok := agentport.LastEntryFor(m, "pr-reviewer", agentport.ProviderClaude, agentport.ScopeUser, agentport.KindAgent)
	if !ok {
		t.Fatalf("expected a Kind: agent manifest entry for pr-reviewer/claude/user")
	}
	if entry.Provenance.CatalogID != "res_agent123" {
		t.Errorf("expected manifest Provenance.CatalogID res_agent123, got %q", entry.Provenance.CatalogID)
	}
	if entry.Provenance.Version != "1.2.0" {
		t.Errorf("expected manifest Provenance.Version 1.2.0, got %q", entry.Provenance.Version)
	}
	if string(entry.Provenance.SourceProvider) != "gateway" {
		t.Errorf("expected manifest Provenance.SourceProvider gateway, got %q", entry.Provenance.SourceProvider)
	}
}

func TestAgentInstall_CatalogID_FanOutToTwoTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	bundle := buildCanonicalAgentBundle(t, "pr-reviewer")
	srv := newMockAgentCatalogGateway(t, "res_fanout_agent", bundle, "2.0.0")

	cmd := AgentInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_fanout_agent", "--to", "claude", "--to", "opencode", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, out.String())
	}

	claudePath := filepath.Join(home, ".claude", "agents", "pr-reviewer.md")
	if _, err := os.Stat(claudePath); err != nil {
		t.Fatalf("expected claude placement %s: %v\noutput:\n%s", claudePath, err, out.String())
	}
	opencodePath := filepath.Join(home, ".config", "opencode", "agents", "pr-reviewer.md")
	if _, err := os.Stat(opencodePath); err != nil {
		t.Fatalf("expected opencode placement %s: %v\noutput:\n%s", opencodePath, err, out.String())
	}

	m, err := agentport.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if _, ok := agentport.LastEntryFor(m, "pr-reviewer", agentport.ProviderClaude, agentport.ScopeUser, agentport.KindAgent); !ok {
		t.Errorf("expected a manifest entry for pr-reviewer/claude/user")
	}
	if _, ok := agentport.LastEntryFor(m, "pr-reviewer", agentport.ProviderOpencode, agentport.ScopeUser, agentport.KindAgent); !ok {
		t.Errorf("expected a manifest entry for pr-reviewer/opencode/user")
	}
}

func TestAgentInstall_CatalogID_ChecksumMismatch_NonZeroExitNoWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	bundle := buildCanonicalAgentBundle(t, "pr-reviewer")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resource-Content-Sha256", "0000000000000000000000000000000000000000000000000000000000000000")
		w.Header().Set("X-Skill-Scan-Verdict", "approved")
		w.Header().Set("X-Skill-Version", "1.0.0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	}))
	t.Cleanup(srv.Close)

	cmd := AgentInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_bad_checksum", "--to", "claude", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected a non-nil error for a checksum mismatch")
	}

	if _, err := os.Stat(filepath.Join(home, ".claude", "agents", "pr-reviewer.md")); !os.IsNotExist(err) {
		t.Fatalf("expected nothing written on checksum mismatch, stat err = %v", err)
	}
}

// TestAgentInstall_NoGatewayConfigured_FailsClosed exercises the offline
// fail-closed exit criterion required by the task: an empty-HOME build with
// no gateway configured must print offlineGuidance and write nothing.
func TestAgentInstall_NoGatewayConfigured_FailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Deliberately no GATEWAY_API_KEY / RELAY_EMAIL / keychain session, and
	// no --gateway-url override.

	cmd := AgentInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_never_reached"})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected an error with no gateway configured")
	}
	if err.Error() != offlineGuidance {
		t.Fatalf("expected the exact offlineGuidance message, got: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(statErr) {
		t.Fatalf("expected zero files written, stat err = %v", statErr)
	}
}

// TestAgentInstall_OfflineFlag_FailsClosed mirrors
// TestSkillInstall_CatalogID_OfflineFlag_FailsClosed for `relay agent
// install`.
func TestAgentInstall_OfflineFlag_FailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token") // even with creds available...

	SetOffline(true)
	t.Cleanup(func() { SetOffline(false) })

	cmd := AgentInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_never_reached"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected --offline to refuse a catalog-id agent install even with credentials configured")
	}
}

func TestAgentInstall_UnknownToProviderErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	bundle := buildCanonicalAgentBundle(t, "pr-reviewer")
	srv := newMockAgentCatalogGateway(t, "res_agent_unknown_to", bundle, "1.0.0")

	cmd := AgentInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_agent_unknown_to", "--to", "cline", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for --to cline (a skill-only provider with no agent adapter)")
	}
}

// TestAgentInstall_ToOmitted_NoProvidersDetectedErrors exercises
// resolveAgentInstallTargets' "no agent providers detected" branch: an
// isolated, empty HOME has no ~/.claude, ~/.config/opencode, etc.
// directories, so AgentDetectedProviders() returns nothing when --to is
// omitted.
func TestAgentInstall_ToOmitted_NoProvidersDetectedErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GATEWAY_API_KEY", "test-bearer-token")

	bundle := buildCanonicalAgentBundle(t, "pr-reviewer")
	srv := newMockAgentCatalogGateway(t, "res_agent_no_targets", bundle, "1.0.0")

	cmd := AgentInstallCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"res_agent_no_targets", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error when --to is omitted and no agent providers are detected")
	}
}
