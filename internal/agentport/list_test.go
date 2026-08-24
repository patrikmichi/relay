package agentport

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func names(refs []SkillRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

func TestList_UserScopeDirectChildren(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeFiles(t, filepath.Join(home, ".claude", "skills", "alpha"), map[string][]byte{
		"SKILL.md": []byte("---\nname: alpha\ndescription: d\n---\n\nbody\n"),
	})
	writeFiles(t, filepath.Join(home, ".claude", "skills", "beta"), map[string][]byte{
		"SKILL.md": []byte("---\nname: beta\ndescription: d\n---\n\nbody\n"),
	})

	refs, err := List(NewClaudeAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := names(refs); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("names = %v, want [alpha beta]", got)
	}
	for _, r := range refs {
		if r.Provider != ProviderClaude || r.Scope != ScopeUser {
			t.Errorf("unexpected ref: %#v", r)
		}
	}
}

func TestList_IncludesLegacyFlatCommandFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	commandsDir := filepath.Join(home, ".claude", "commands")
	if err := os.MkdirAll(commandsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(commandsDir, "old-command.md"), []byte("---\nname: old-command\ndescription: d\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	refs, err := List(NewClaudeAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := names(refs); len(got) != 1 || got[0] != "old-command" {
		t.Fatalf("names = %v, want [old-command]", got)
	}
}

func TestList_NoneFoundReturnsEmptyNotError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	refs, err := List(NewClaudeAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %#v, want none", refs)
	}
}

func TestList_DuplicateNamesResolvedByPriorityOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// opencode's UserDirs() priority: .config/opencode/skills, then
	// .claude/skills, then .agents/skills. Put "shared" in both the
	// primary opencode dir and the claude fallback dir with different
	// descriptions — List should report only the higher-priority one.
	writeFiles(t, filepath.Join(home, ".config", "opencode", "skills", "shared"), map[string][]byte{
		"SKILL.md": []byte("---\nname: shared\ndescription: primary\n---\n\nbody\n"),
	})
	writeFiles(t, filepath.Join(home, ".claude", "skills", "shared"), map[string][]byte{
		"SKILL.md": []byte("---\nname: shared\ndescription: fallback\n---\n\nbody\n"),
	})

	refs, err := List(NewOpencodeAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %#v, want exactly 1 (deduplicated)", refs)
	}
	wantPath := filepath.Join(home, ".config", "opencode", "skills", "shared")
	if refs[0].Path != wantPath {
		t.Fatalf("Path = %s, want %s (primary dir should win)", refs[0].Path, wantPath)
	}
}

func TestList_CursorRecursiveProjectScopeDiscovery(t *testing.T) {
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := t.TempDir()
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	// Nested foo/bar/<name>/SKILL.md under .cursor/skills/ — identity is
	// the SKILL.md's own directory name ("nested-skill"), not "foo" or
	// "bar".
	nestedDir := filepath.Join(root, ".cursor", "skills", "foo", "bar", "nested-skill")
	writeFiles(t, nestedDir, map[string][]byte{
		"SKILL.md": []byte("---\nname: nested-skill\ndescription: deeply nested\n---\n\nbody\n"),
	})

	refs, err := List(NewCursorAdapter(), ScopeProject)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := names(refs); len(got) != 1 || got[0] != "nested-skill" {
		t.Fatalf("names = %v, want [nested-skill]", got)
	}

	// List() resolves ScopeProject paths against the physical cwd (as
	// filepath.Abs/os.Getwd would resolve it, not the possibly-symlinked
	// path t.TempDir() returned — e.g. /tmp is a symlink to /private/tmp on
	// macOS), same convention as TestMigrate_ProjectScopeUsesCwdRelativeDir.
	physicalCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	wantPath := filepath.Join(physicalCwd, ".cursor", "skills", "foo", "bar", "nested-skill")
	if refs[0].Path != wantPath {
		t.Fatalf("Path = %s, want %s", refs[0].Path, wantPath)
	}
}

func TestList_NonCursorProjectScopeIsNotRecursive(t *testing.T) {
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := t.TempDir()
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	// A nested skill under Claude's project dir should NOT be discovered —
	// Claude's discovery only looks at direct children.
	writeFiles(t, filepath.Join(root, ".claude", "skills", "foo", "bar", "nested-skill"), map[string][]byte{
		"SKILL.md": []byte("---\nname: nested-skill\ndescription: d\n---\n\nbody\n"),
	})

	refs, err := List(NewClaudeAdapter(), ScopeProject)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %#v, want none (Claude project scope is not recursive)", refs)
	}
}
