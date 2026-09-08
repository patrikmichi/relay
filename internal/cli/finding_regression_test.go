package cli

// Regression tests derived from the 2026-09-07 deep review's reproduction
// probes (vault/reviews/relay/2026-09-07/evidence/cli_probes_test.go).
// Those probes deliberately asserted the CURRENT (buggy) behavior; these
// tests invert the assertions to the SAFE behavior the finding demands.
// They are expected to FAIL against baseline 931aac4 and must turn green
// in the same commit/PR that lands each finding's fix — do not weaken
// these assertions to obtain a green baseline.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/client"
)

// TestR09_SyncBundleUpdateRemovesStaleFiles reproduces R09: writing a new
// plugin bundle version does not reconcile the previous file set, so a
// file removed in the new version survives on disk. Safe behavior: the
// owned file set after an update must match the new bundle exactly.
func TestR09_SyncBundleUpdateRemovesStaleFiles(t *testing.T) {
	dir := t.TempDir()
	if err := writePluginBundle(dir, []byte(`{"files":{"old.md":"old","keep.md":"one"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := writePluginBundle(dir, []byte(`{"files":{"keep.md":"two"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.md")); err == nil {
		t.Fatal("R09: old.md removed from the new bundle still exists after update")
	}
}

// TestR05_SyncRejectsSymlinkEscape reproduces R05: a symlinked
// intermediate directory inside the plugin directory lets sync write
// outside the plugin root. Safe behavior: the write must fail (or be
// contained) rather than mutate a file outside the plugin directory.
func TestR05_SyncRejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "nested")); err != nil {
		t.Fatal(err)
	}
	_ = writePluginBundle(dir, []byte(`{"files":{"nested/sentinel":"changed"}}`))
	if raw, err := os.ReadFile(filepath.Join(outside, "sentinel")); err == nil {
		t.Fatalf("R05: sync wrote outside the plugin directory through a symlinked component: %s", raw)
	}
}

// TestR18_ManifestSelectsSkillOverReadme reproduces R18: buildBundle picks
// the first root .md file in walk order, and README.md sorts before
// SKILL.md. Safe behavior: the canonical manifest must be selected by
// artifact kind, not walk order.
func TestR18_ManifestSelectsSkillOverReadme(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("README"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ntype: skill\nname: demo\n---\nSkill"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, _, _, err := buildBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(manifest)) == "README" {
		t.Fatalf("R18: README.md selected as manifest instead of SKILL.md")
	}
}

// TestR15_CallToolFailsOnIsError reproduces R15: callTool decodes a
// JSON-RPC top-level error but not result.isError, so a valid MCP tool
// failure is reported as success. Safe behavior: isError must produce a
// non-nil error.
func TestR15_CallToolFailsOnIsError(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"tool failed"}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()

	err := callTool(client.New(srv.URL, "synthetic"), "demo", "test", nil)
	if err == nil {
		t.Fatal("R15: expected a non-nil error for an MCP result.isError=true envelope")
	}
}

// TestR15_CallToolRejectsUnrelatedJSON reproduces the second half of R15:
// an HTTP 200 body that isn't a JSON-RPC envelope at all is currently
// swallowed as a successful empty result via the raw-output fallback.
// Safe behavior: an envelope that matches neither the result nor error
// shape for a JSON-RPC 2.0 response should not be silently accepted as
// success by this typed call path.
func TestR15_CallToolRejectsUnrelatedJSON(t *testing.T) {
	body := `{"unexpected":"not an RPC response"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()

	err := callTool(client.New(srv.URL, "synthetic"), "demo", "test", nil)
	if err == nil {
		t.Fatal("R15: expected an error for a response missing both result and error fields")
	}
}

// TestR16_CallToolPreservesAllContentBlocks reproduces R16: callTool only
// prints Content[0].Text, discarding subsequent blocks. Safe behavior:
// every content block must appear in the rendered output.
func TestR16_CallToolPreservesAllContentBlocks(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()

	stdout := captureStdout(t, func() {
		if err := callTool(client.New(srv.URL, "synthetic"), "demo", "test", nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stdout, "first") || !strings.Contains(stdout, "second") {
		t.Fatalf("R16: expected both content blocks in output, got: %q", stdout)
	}
}

// TestR29_PublishExcludesDotEnvByDefault reproduces R29: buildBundle walks
// all regular files including dotfiles, so a `.env` alongside SKILL.md is
// packaged for upload. Safe behavior: secret-shaped filenames must be
// excluded by default without an explicit inclusion opt-in.
// TestR04_AgentMigrateRefusesSecurityFieldLoss reproduces R04's exact
// cli-probes.jsonl observation: `relay agent migrate --strict` on a Claude
// agent carrying disallowedTools/permissionMode/hooks exited 0 and printed
// "fidelity: no loss — all fields preserved" while writing only
// description/body. Safe behavior: the command must refuse (nonzero exit,
// named reason) and must not write the target file — with or without
// --strict, since a security-relevant loss is never bypassable by a flag.
func TestR04_AgentMigrateRefusesSecurityFieldLoss(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := "---\nname: reviewer\ndescription: Review only\ndisallowedTools: [Bash, Write]\npermissionMode: plan\nhooks: {}\n---\nReview only.\n"
	if err := os.WriteFile(filepath.Join(dir, "reviewer.md"), []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--to", "opencode"})
	err := cmd.Execute()
	out := buf.String()

	if err == nil {
		t.Fatalf("R04: expected agent migrate to refuse a migration that loses disallowedTools/permissionMode/hooks, got exit 0. output:\n%s", out)
	}
	if strings.Contains(out, "no loss") {
		t.Errorf("R04: fidelity report must never claim \"no loss\" when security fields vanish: %s", out)
	}

	opencodePath := filepath.Join(home, ".config", "opencode", "agent", "reviewer.md")
	if _, statErr := os.Stat(opencodePath); !os.IsNotExist(statErr) {
		t.Fatalf("R04: expected nothing written when a security-relevant field would be lost, stat err = %v", statErr)
	}
}

// TestR22_AgentMigrateRefusesDeniedToolsLoss reproduces R22: migrating an
// opencode agent with an explicitly-denied tool onto claude currently
// reports only a "degraded"-style note and proceeds to write, so the
// migrated agent silently loses its restriction. Safe behavior: the command
// refuses outright, naming the reason, and writes nothing.
func TestR22_AgentMigrateRefusesDeniedToolsLoss(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "opencode", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := "---\ndescription: Reviews code.\ntools:\n  read: true\n  write: false\n---\n\nReview the diff.\n"
	if err := os.WriteFile(filepath.Join(dir, "reviewer.md"), []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "opencode", "--to", "claude"})
	err := cmd.Execute()

	if err == nil {
		t.Fatalf("R22: expected agent migrate to refuse dropping an explicitly-denied tool's restriction, got exit 0. output:\n%s", buf.String())
	}
	claudePath := filepath.Join(home, ".claude", "agents", "reviewer.md")
	if _, statErr := os.Stat(claudePath); !os.IsNotExist(statErr) {
		t.Fatalf("R22: expected nothing written when denied-tool semantics would be lost, stat err = %v", statErr)
	}
}

func TestR29_PublishExcludesDotEnvByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ntype: skill\n---\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SYNTHETIC_ONLY=example"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, files, err := buildBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == ".env" || strings.HasSuffix(f, "/.env") {
			t.Fatalf("R29: .env included in outgoing publish bundle by default: %v", files)
		}
	}
}
