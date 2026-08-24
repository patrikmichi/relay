package agentport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSkillPath_UserScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	skillDir := filepath.Join(home, ".claude", "skills", "git-helper")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: git-helper\ndescription: d\n---\n\nbody\n"),
	})

	got, err := ResolveSkillPath(NewClaudeAdapter(), ScopeUser, "git-helper")
	if err != nil {
		t.Fatalf("ResolveSkillPath: %v", err)
	}
	if got != skillDir {
		t.Fatalf("got %s, want %s", got, skillDir)
	}
}

func TestResolveSkillPath_LegacyCommandFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// No skills/ dir entry — only a legacy flat commands/<name>.md file.
	commandsDir := filepath.Join(home, ".claude", "commands")
	if err := os.MkdirAll(commandsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	flatFile := filepath.Join(commandsDir, "old-command.md")
	if err := os.WriteFile(flatFile, []byte("---\nname: old-command\ndescription: d\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ResolveSkillPath(NewClaudeAdapter(), ScopeUser, "old-command")
	if err != nil {
		t.Fatalf("ResolveSkillPath: %v", err)
	}
	if got != flatFile {
		t.Fatalf("got %s, want %s", got, flatFile)
	}
}

func TestResolveSkillPath_OpencodeFallsBackToClaudeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Only ~/.claude/skills/<name> exists — opencode should still find it
	// via its documented fallback dirs.
	skillDir := filepath.Join(home, ".claude", "skills", "shared-skill")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: shared-skill\ndescription: d\n---\n\nbody\n"),
	})

	got, err := ResolveSkillPath(NewOpencodeAdapter(), ScopeUser, "shared-skill")
	if err != nil {
		t.Fatalf("ResolveSkillPath: %v", err)
	}
	if got != skillDir {
		t.Fatalf("got %s, want %s", got, skillDir)
	}
}

func TestResolveSkillPath_NotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if _, err := ResolveSkillPath(NewClaudeAdapter(), ScopeUser, "does-not-exist"); err == nil {
		t.Fatalf("expected an error for a missing skill")
	}
}

// --- BLOCKER 1: path traversal via unvalidated name ---
//
// ResolveSkillPath (and ResolveOwnSkillPath) must reject a name containing
// path separators or "../" BEFORE doing any filepath.Join/filesystem
// operation — every read caller (migrate --from, scan, diff) and the
// destructive uninstall path all funnel through one of these two resolvers.

func TestResolveSkillPath_RejectsPathTraversal_UserScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A file that would be reached if traversal were allowed: escapes
	// ~/.claude/skills/ up to an arbitrary sibling location.
	outsideDir := filepath.Join(home, "x", ".claude", "skills", "foo")
	writeFiles(t, outsideDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: foo\ndescription: d\n---\n\nbody\n"),
	})

	for _, name := range []string{
		"../../../x/.claude/skills/foo",
		"../foo",
		"foo/../../bar",
		"a/b",
		`a\b`,
	} {
		if _, err := ResolveSkillPath(NewClaudeAdapter(), ScopeUser, name); err == nil {
			t.Fatalf("ResolveSkillPath(%q): expected an error rejecting path traversal, got nil", name)
		}
	}
}

func TestResolveSkillPath_RejectsPathTraversal_ProjectScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, name := range []string{"../escape", "a/b", "../../etc/passwd"} {
		if _, err := ResolveSkillPath(NewClaudeAdapter(), ScopeProject, name); err == nil {
			t.Fatalf("ResolveSkillPath(%q, project): expected an error rejecting path traversal, got nil", name)
		}
	}
}

func TestResolveOwnSkillPath_RejectsPathTraversal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, adapter := range AllAdapters() {
		for _, name := range []string{"../../../etc/passwd", "a/b", "../x"} {
			if _, err := ResolveOwnSkillPath(adapter, ScopeUser, name); err == nil {
				t.Fatalf("%s: ResolveOwnSkillPath(%q): expected an error rejecting path traversal, got nil", adapter.ID(), name)
			}
			if _, err := ResolveOwnSkillPath(adapter, ScopeProject, name); err == nil {
				t.Fatalf("%s: ResolveOwnSkillPath(%q, project): expected an error rejecting path traversal, got nil", adapter.ID(), name)
			}
		}
	}
}

// --- BLOCKER 2: uninstall must never cross into another provider's dir or
// an admin/read-only dir ---

func TestResolveOwnSkillPath_CursorDoesNotFallBackToClaudeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Only exists via Claude's own directory — Cursor documents this as a
	// read-compatibility fallback (see cursorAdapter.UserDirs), but it must
	// never be a destructive-operation ("own") target for Cursor.
	claudeSkillDir := filepath.Join(home, ".claude", "skills", "shared-skill")
	writeFiles(t, claudeSkillDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: shared-skill\ndescription: d\n---\n\nbody\n"),
	})

	// Sanity check: the READ resolver (ResolveSkillPath) does find it via
	// the documented compatibility fallback...
	if _, err := ResolveSkillPath(NewCursorAdapter(), ScopeUser, "shared-skill"); err != nil {
		t.Fatalf("ResolveSkillPath (read path) should still find it via the compat fallback: %v", err)
	}

	// ...but the OWN resolver (used by uninstall) must not.
	if path, err := ResolveOwnSkillPath(NewCursorAdapter(), ScopeUser, "shared-skill"); err == nil {
		t.Fatalf("ResolveOwnSkillPath should not find a Claude-owned skill via Cursor's compat fallback, got %s", path)
	}

	// And the file must still exist — proving nothing would have deleted it.
	if _, err := os.Stat(claudeSkillDir); err != nil {
		t.Fatalf("shared skill should still be on disk: %v", err)
	}
}

func TestResolveOwnSkillPath_CodexNeverResolvesAdminDir(t *testing.T) {
	// codexAdminSkillsDir (/etc/codex/skills) is UserDirs()[1] but must be
	// excluded from Codex's "own" dirs entirely.
	codex := NewCodexAdapter()
	if got := codex.OwnUserDirCount(); got != 1 {
		t.Fatalf("codexAdapter.OwnUserDirCount() = %d, want 1 (admin dir excluded)", got)
	}
	dirs := codex.UserDirs()
	if len(dirs) < 2 || dirs[1] != codexAdminSkillsDir {
		t.Fatalf("expected UserDirs()[1] == %s (the excluded admin dir), got %#v", codexAdminSkillsDir, dirs)
	}
}

func TestResolveOwnSkillPath_ClaudeOwnsBothItsDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Claude's legacy flat commands/<name>.md is genuinely Claude's own
	// (unlike opencode/Cursor's compat-read fallbacks), so ResolveOwnSkillPath
	// must still find it — this is the ResolveOwnSkillPath analogue of
	// TestResolveSkillPath_LegacyCommandFallback.
	commandsDir := filepath.Join(home, ".claude", "commands")
	if err := os.MkdirAll(commandsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	flatFile := filepath.Join(commandsDir, "old-command.md")
	if err := os.WriteFile(flatFile, []byte("---\nname: old-command\ndescription: d\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ResolveOwnSkillPath(NewClaudeAdapter(), ScopeUser, "old-command")
	if err != nil {
		t.Fatalf("ResolveOwnSkillPath: %v", err)
	}
	if got != flatFile {
		t.Fatalf("got %s, want %s", got, flatFile)
	}
}

// --- P1.6: kind-based load-back adapter resolution ---

// TestResolveTargetForEntry_SkillKindResolvesSkillAdapter confirms a
// KindSkill (or default-normalized) manifest entry resolves to the Skill
// Adapter for its provider, not the agent one.
func TestResolveTargetForEntry_SkillKindResolvesSkillAdapter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	target, err := resolveTargetForEntry(ManifestEntry{Provider: ProviderClaude, Kind: KindSkill})
	if err != nil {
		t.Fatalf("resolveTargetForEntry: %v", err)
	}
	if target.ID() != ProviderClaude {
		t.Fatalf("ID() = %s, want %s", target.ID(), ProviderClaude)
	}
	dirs := target.UserDirs()
	want := filepath.Join(".claude", "skills")
	if len(dirs) == 0 || !strings.HasSuffix(dirs[0], want) {
		t.Fatalf("UserDirs() = %v, want a dir ending in %s (the SKILL adapter)", dirs, want)
	}
}

// TestResolveTargetForEntry_AgentKindResolvesAgentAdapter confirms a
// KindAgent manifest entry resolves to the AgentAdapter for its provider —
// a DIFFERENT set of directories than the skill adapter for the same
// provider id, proving kind, not just provider id, drives the resolution.
func TestResolveTargetForEntry_AgentKindResolvesAgentAdapter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	target, err := resolveTargetForEntry(ManifestEntry{Provider: ProviderClaude, Kind: KindAgent})
	if err != nil {
		t.Fatalf("resolveTargetForEntry: %v", err)
	}
	if target.ID() != ProviderClaude {
		t.Fatalf("ID() = %s, want %s", target.ID(), ProviderClaude)
	}
	dirs := target.UserDirs()
	want := filepath.Join(".claude", "agents")
	if len(dirs) == 0 || !strings.HasSuffix(dirs[0], want) {
		t.Fatalf("UserDirs() = %v, want a dir ending in %s (the AGENT adapter)", dirs, want)
	}

	// Sanity: this must NOT be the skill adapter's directory.
	notWant := filepath.Join(".claude", "skills")
	if strings.HasSuffix(dirs[0], notWant) {
		t.Fatalf("UserDirs() = %v resolved to the SKILL adapter's dir for a KindAgent entry", dirs)
	}
}

// TestResolveTargetForEntry_UnknownAgentProviderErrors confirms a KindAgent
// entry for a provider with no agent config (codex/cursor have no
// agent-file primitive — design §3d) errors rather than silently
// falling back to the skill adapter.
func TestResolveTargetForEntry_UnknownAgentProviderErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if _, err := resolveTargetForEntry(ManifestEntry{Provider: ProviderCodex, Kind: KindAgent}); err == nil {
		t.Fatalf("resolveTargetForEntry(codex, KindAgent): expected an error (codex has no agent provider config)")
	}
}

func TestResolveSkillPath_ProjectScopeSearchesParents(t *testing.T) {
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Skill lives at <root>/.claude/skills/git-helper — repo root, two
	// levels above the nested working directory.
	skillDir := filepath.Join(root, ".claude", "skills", "git-helper")
	writeFiles(t, skillDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: git-helper\ndescription: d\n---\n\nbody\n"),
	})

	if err := os.Chdir(nested); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	got, err := ResolveSkillPath(NewClaudeAdapter(), ScopeProject, "git-helper")
	if err != nil {
		t.Fatalf("ResolveSkillPath: %v", err)
	}

	wantSuffix := filepath.Join(".claude", "skills", "git-helper")
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("got %s, want a path ending in %s", got, wantSuffix)
	}
	// Confirm it actually resolved to the real fixture (not just a
	// coincidental name match) by loading it.
	loaded, err := NewClaudeAdapter().Load(got)
	if err != nil {
		t.Fatalf("Load(%s): %v", got, err)
	}
	if loaded.Name != "git-helper" {
		t.Fatalf("loaded.Name = %s, want git-helper", loaded.Name)
	}
}
