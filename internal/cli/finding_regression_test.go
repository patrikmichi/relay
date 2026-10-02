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

	"github.com/patrikmichi/relay/internal/agentport"
	"github.com/patrikmichi/relay/internal/client"
)

// R09: writing a new bundle version doesn't reconcile the previous file
// set, so a file removed upstream survives on disk.
func TestR09_SyncBundleUpdateRemovesStaleFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // plugin writes go through the transaction engine, which locks/journals under HOME
	localDir := t.TempDir()
	pluginDir := filepath.Join(localDir, "plugins", "demo")
	if err := writePluginBundle(localDir, "demo", pluginDir, []byte(`{"files":{"old.md":"old","keep.md":"one"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := writePluginBundle(localDir, "demo", pluginDir, []byte(`{"files":{"keep.md":"two"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "old.md")); err == nil {
		t.Fatal("R09: old.md removed from the new bundle still exists after update")
	}
}

// R05: a symlinked intermediate directory inside the plugin directory
// lets sync write outside the plugin root.
func TestR05_SyncRejectsSymlinkEscape(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // plugin writes go through the transaction engine, which locks/journals under HOME
	localDir := t.TempDir()
	pluginDir := filepath.Join(localDir, "plugins", "demo")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(pluginDir, "nested")); err != nil {
		t.Fatal(err)
	}
	_ = writePluginBundle(localDir, "demo", pluginDir, []byte(`{"files":{"nested/sentinel":"changed"}}`))
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

// R17: skillPublishCmd/agentPublishCmd change Use/Short only and never set
// an alias default type, so an ordinary provider file with no
// marketplace-specific `type` frontmatter fails type detection through the
// skill/agent parent commands even though the command name itself declares
// the intended kind.
func TestR17_SkillPublishDefaultsTypeForOrdinaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	// Ordinary skill file: name/description only, no `type` field.
	src := "---\nname: demo\ndescription: An ordinary skill.\n---\nBody.\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runPublish(nil, path, publishOpts{
		aliasKind: agentport.KindSkill,
		channel:   "stable",
		dryRun:    true,
	})
	if err != nil {
		t.Fatalf("R17: ordinary skill file published via the skill alias should default to type=skill, got error: %v", err)
	}
}

// R17 (continued): the agent alias must supply the same kind of default.
func TestR17_AgentPublishDefaultsTypeForOrdinaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.md")
	src := "---\nname: demo\ndescription: An ordinary agent.\n---\nBody.\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runPublish(nil, path, publishOpts{
		aliasKind: agentport.KindAgent,
		channel:   "stable",
		dryRun:    true,
	})
	if err != nil {
		t.Fatalf("R17: ordinary agent file published via the agent alias should default to type=agent, got error: %v", err)
	}
}

// R17 (continued): "The agent alias can also inherit a contradictory type
// from the document." A document whose own frontmatter names a different
// kind than the alias it was published through must fail loudly rather than
// silently pick one.
func TestR17_PublishRejectsContradictoryAliasType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	src := "---\nname: demo\ntype: agent\ndescription: Declares a different kind than the alias.\n---\nBody.\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runPublish(nil, path, publishOpts{
		aliasKind: agentport.KindSkill,
		channel:   "stable",
		dryRun:    true,
	})
	if err == nil {
		t.Fatal("R17: expected an error when frontmatter type contradicts the skill/agent alias, got nil")
	}
	if !strings.Contains(err.Error(), "contradictory") {
		t.Errorf("R17: error should name the contradiction explicitly, got: %v", err)
	}
}

// R19: publishCmdWithDoer's RunE resolves the gateway and authenticated
// client unconditionally, before ever reaching the --dry-run branch inside
// runPublish — so `relay skill publish --dry-run` fails without credentials
// even though the intended work is entirely local packaging/validation.
func TestR19_DryRunSucceedsWithoutCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from any real ~/.config/relay/config.json or keychain
	t.Setenv("RELAY_EMAIL", "")
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("GATEWAY_URL", "")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ntype: skill\nname: demo\n---\nBody.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := skillPublishCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{dir, "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("R19: offline dry-run must succeed without any credentials, got: %v", err)
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

	err := callTool(client.New(srv.URL, "synthetic"), "demo", "test", nil, false)
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

	err := callTool(client.New(srv.URL, "synthetic"), "demo", "test", nil, false)
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
		if err := callTool(client.New(srv.URL, "synthetic"), "demo", "test", nil, false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stdout, "first") || !strings.Contains(stdout, "second") {
		t.Fatalf("R16: expected both content blocks in output, got: %q", stdout)
	}
}

// R16 (continued): a non-text content block (image/resource/etc.) has no
// preserved output representation — callTool must render it as raw JSON
// rather than silently dropping it (renderContentBlock's non-text fallback).
func TestR16_CallToolPreservesNonTextContentBlock(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"before"},{"type":"image","data":"base64stuff","mimeType":"image/png"},{"type":"text","text":"after"}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()

	stdout := captureStdout(t, func() {
		if err := callTool(client.New(srv.URL, "synthetic"), "demo", "test", nil, false); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"before", "after", `"type": "image"`, "base64stuff"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("R16: expected output to contain %q, got: %q", want, stdout)
		}
	}
}

// --json bypasses human rendering and prints the full raw envelope,
// but still fails (nonzero exit) on a tool-level error — scripts must be
// able to trust the exit code without parsing stdout.
func TestCallTool_JSONOutputPrintsRawEnvelopeAndPreservesErrorExitStatus(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"tool failed"}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()

	var callErr error
	stdout := captureStdout(t, func() {
		callErr = callTool(client.New(srv.URL, "synthetic"), "demo", "test", nil, true)
	})
	if callErr == nil {
		t.Fatal("--json must not suppress the nonzero exit status for a tool-level failure")
	}
	if strings.TrimSpace(stdout) != body {
		t.Fatalf("--json must print the unmodified raw response body, got: %q", stdout)
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

// R29: secretExclusionReason's original pattern set only caught .env and
// bare OpenSSH id_* key basenames — a bundled server.pem, .npmrc, or .netrc
// shipped by default with no exclusion and no --include override needed,
// because it was never screened in the first place.
func TestR29_SecretExclusionCoversCommonCredentialFilenames(t *testing.T) {
	excluded := []string{
		"server.pem", "client.key", "cert.p12", "bundle.pfx",
		".npmrc", ".netrc", ".pgpass", "credentials", "credentials.json",
	}
	for _, name := range excluded {
		if secretExclusionReason(name) == "" {
			t.Errorf("R29: expected %q to be excluded by default, got no reason", name)
		}
	}

	// Ordinary files with superficially similar names must not be swept up.
	safe := []string{"README.md", "keymap.json", "keys.md"}
	for _, name := range safe {
		if reason := secretExclusionReason(name); reason != "" {
			t.Errorf("R29: expected %q to NOT be excluded, got reason %q", name, reason)
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
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_API_KEY", "test-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	cmd.SetArgs([]string{"google-workspace", "--scope", "gmail_list_messages", "--gateway-url", srv.URL})
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
