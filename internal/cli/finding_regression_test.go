package cli

// Named regression tests inverting the 2026-09-07 review's bug-confirming
// probes (evidence/cli_probes_test.go) into the safe behavior each finding
// demands. Expected to fail until the fix lands in the same commit — do
// not weaken these assertions.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/client"
)

// R09: writing a new bundle version doesn't reconcile the previous file
// set, so a file removed upstream survives on disk.
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

// R05: a symlinked intermediate directory inside the plugin directory
// lets sync write outside the plugin root.
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

// R18: buildBundle picks the first root .md file in walk order, and
// README.md sorts before SKILL.md.
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

// R15: callTool decodes a JSON-RPC top-level error but not
// result.isError, so a valid MCP tool failure reports success.
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

// R15 (continued): a 200 body matching neither the result nor error shape
// falls through to the raw-output success path.
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

// R16: callTool only prints Content[0].Text, discarding later blocks.
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

	opencodePath := filepath.Join(home, ".config", "opencode", "agents", "reviewer.md")
	if _, statErr := os.Stat(opencodePath); !os.IsNotExist(statErr) {
		t.Fatalf("R04: expected nothing written when a security-relevant field would be lost, stat err = %v", statErr)
	}
}

// TestR04_AgentMigrateRefusesNonInteractiveLossWithoutAcceptLoss reproduces
// R04's noninteractive-consent gap named in the review's Fix guidance:
// "noninteractive calls currently proceed automatically unless --strict
// detects a known dropped field." A test binary has no attached terminal,
// so a migration with ordinary (non-security) dropped fields and neither
// --strict nor --accept-loss must refuse rather than silently writing —
// the absence of a terminal is never treated as consent.
func TestR04_AgentMigrateRefusesNonInteractiveLossWithoutAcceptLoss(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "opencode", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// opencode's Temperature/Mode fields have no claude equivalent —
	// ordinary (non-security) LossDropped items, not Security-flagged.
	src := "---\ndescription: reviews code\ntemperature: 0.2\nmode: subagent\n---\nReview.\n"
	if err := os.WriteFile(filepath.Join(dir, "reviewer.md"), []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := AgentMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "opencode", "--to", "claude"})
	err := cmd.Execute()

	if err == nil {
		t.Fatalf("R04: expected refusal without --accept-loss when running noninteractively, output:\n%s", buf.String())
	}
	claudePath := filepath.Join(home, ".claude", "agents", "reviewer.md")
	if _, statErr := os.Stat(claudePath); !os.IsNotExist(statErr) {
		t.Fatalf("R04: expected nothing written when noninteractive loss is refused, stat err = %v", statErr)
	}

	cmd2 := AgentMigrateCmd()
	var buf2 bytes.Buffer
	cmd2.SetOut(&buf2)
	cmd2.SetArgs([]string{"reviewer", "--from", "opencode", "--to", "claude", "--accept-loss"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("R04: --accept-loss should let ordinary non-security loss proceed noninteractively: %v, output:\n%s", err, buf2.String())
	}
	if _, statErr := os.Stat(claudePath); statErr != nil {
		t.Fatalf("R04: expected the file written after --accept-loss, stat err = %v", statErr)
	}
}

// TestR04_SkillMigrateRefusesNonInteractiveLossWithoutAcceptLoss covers
// applyMigrationToTargets — shared by `skill migrate` and `skill install` —
// which had no test exercising this refusal path despite the equivalent
// agent-side function being covered above. A test binary has no attached
// terminal, so migrating a skill with an ordinary (non-security) dropped
// field and neither --strict nor --accept-loss must refuse rather than
// silently writing.
func TestR04_SkillMigrateRefusesNonInteractiveLossWithoutAcceptLoss(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "skills", "reviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// codex has no `license` equivalent — an ordinary (non-security)
	// LossDropped item, not Security-flagged.
	src := "---\nname: reviewer\ndescription: reviews code\nlicense: MIT\n---\nReview.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := SkillMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"reviewer", "--from", "claude", "--to", "codex"})
	err := cmd.Execute()

	if err == nil {
		t.Fatalf("R04: expected refusal without --accept-loss when running noninteractively, output:\n%s", buf.String())
	}
	codexPath := filepath.Join(home, ".agents", "skills", "reviewer", "SKILL.md")
	if _, statErr := os.Stat(codexPath); !os.IsNotExist(statErr) {
		t.Fatalf("R04: expected nothing written when noninteractive loss is refused, stat err = %v", statErr)
	}

	cmd2 := SkillMigrateCmd()
	var buf2 bytes.Buffer
	cmd2.SetOut(&buf2)
	cmd2.SetArgs([]string{"reviewer", "--from", "claude", "--to", "codex", "--accept-loss"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("R04: --accept-loss should let ordinary non-security loss proceed noninteractively: %v, output:\n%s", err, buf2.String())
	}
	if _, statErr := os.Stat(codexPath); statErr != nil {
		t.Fatalf("R04: expected the file written after --accept-loss, stat err = %v", statErr)
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
	dir := filepath.Join(home, ".config", "opencode", "agents")
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

// R29: buildBundle walks all regular files including dotfiles, so a
// `.env` next to SKILL.md gets packaged for upload.
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

// TestR14_AuthorizeReturnsExplicitUnsupportedError reproduces R14:
// `relay authorize <service>` used to run the generic device-code login
// flow and then unconditionally print "Authorized <service>", even though
// it never requested or verified any service-specific scope. Safe
// behavior: until the real per-service grant flow exists, the command
// must fail with a named, explicit error and must never print any success
// claim for the service.
func TestR14_AuthorizeReturnsExplicitUnsupportedError(t *testing.T) {
	cmd := AuthorizeCmd()
	cmd.SetArgs([]string{"google-workspace"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("R14: authorize must fail until per-service authorization is implemented, got nil error")
	}
	if !errors.Is(err, ErrServiceAuthorizationUnsupported) {
		t.Fatalf("R14: expected ErrServiceAuthorizationUnsupported, got: %v", err)
	}

	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, "Authorized") {
		t.Fatalf("R14: output must never claim the service was authorized: %q", combined)
	}
}
