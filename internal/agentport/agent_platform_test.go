package agentport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers agent_resolve.go/agent_list.go/agent_scan.go/
// agent_detect.go's AgentDetectedProviders — the agent-path plumbing
// threads a per-provider file extension instead of a hardcoded ".md".
// Exercised at the agentport-package level (not just via internal/cli) to
// keep this package's own coverage meaningful.

func writeCodexAgent(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name+".toml")
	writeFiles(t, dir, map[string][]byte{
		name + ".toml": []byte("name = \"" + name + "\"\ndescription = \"d\"\ndeveloper_instructions = \"body\"\n"),
	})
	return path
}

func TestResolveAgentPath_CodexUsesTomlExtension(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	codex, ok := AgentAdapterByID(ProviderCodex)
	if !ok {
		t.Fatalf("AgentAdapterByID(codex): ok = false")
	}

	userDir := filepath.Join(home, ".codex", "agents")
	want := writeCodexAgent(t, userDir, "reviewer")

	got, err := ResolveAgentPath(codex, ScopeUser, "reviewer")
	if err != nil {
		t.Fatalf("ResolveAgentPath: %v", err)
	}
	if got != want {
		t.Fatalf("ResolveAgentPath = %q, want %q", got, want)
	}

	if _, err := ResolveAgentPath(codex, ScopeUser, "missing"); err == nil {
		t.Fatalf("expected an error for a missing agent")
	}
}

func TestResolveAgentPath_ProjectScopeSearchesParents(t *testing.T) {
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	root := t.TempDir()
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeCodexAgent(t, filepath.Join(root, ".codex", "agents"), "reviewer")

	if err := os.Chdir(nested); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	codex, _ := AgentAdapterByID(ProviderCodex)
	got, err := ResolveAgentPath(codex, ScopeProject, "reviewer")
	if err != nil {
		t.Fatalf("ResolveAgentPath(project): %v", err)
	}
	want := filepath.Join(root, ".codex", "agents", "reviewer.toml")
	if got != want {
		t.Fatalf("ResolveAgentPath(project) = %q, want %q", got, want)
	}

	if _, err := ResolveAgentPath(codex, ScopeProject, "does-not-exist"); err == nil {
		t.Fatalf("expected an error when no project dir has the agent")
	}
}

func TestResolveOwnAgentPath_ScopesToOwnDirsOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCodexAgent(t, filepath.Join(home, ".codex", "agents"), "reviewer")

	codex, _ := AgentAdapterByID(ProviderCodex)
	got, err := ResolveOwnAgentPath(codex, ScopeUser, "reviewer")
	if err != nil {
		t.Fatalf("ResolveOwnAgentPath: %v", err)
	}
	want := filepath.Join(home, ".codex", "agents", "reviewer.toml")
	if got != want {
		t.Fatalf("ResolveOwnAgentPath = %q, want %q", got, want)
	}

	if _, err := ResolveOwnAgentPath(codex, ScopeUser, "missing"); err == nil {
		t.Fatalf("expected an error for a missing agent")
	}

	// Project scope, own-dirs-only path.
	root := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	writeCodexAgent(t, filepath.Join(root, ".codex", "agents"), "reviewer")
	if _, err := ResolveOwnAgentPath(codex, ScopeProject, "reviewer"); err != nil {
		t.Fatalf("ResolveOwnAgentPath(project): %v", err)
	}
	if _, err := ResolveOwnAgentPath(codex, ScopeProject, "missing"); err == nil {
		t.Fatalf("expected an error for a missing project agent")
	}
}

func TestResolveAgentPath_InvalidNameErrors(t *testing.T) {
	codex, _ := AgentAdapterByID(ProviderCodex)
	if _, err := ResolveAgentPath(codex, ScopeUser, "../evil"); err == nil {
		t.Fatalf("expected a name-validation error")
	}
	if _, err := ResolveOwnAgentPath(codex, ScopeUser, "../evil"); err == nil {
		t.Fatalf("expected a name-validation error")
	}
}

func TestAgentList_UsesProviderFileExtension(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCodexAgent(t, filepath.Join(home, ".codex", "agents"), "reviewer")
	writeCodexAgent(t, filepath.Join(home, ".codex", "agents"), "planner")
	// A non-matching-extension file must be ignored.
	writeFiles(t, filepath.Join(home, ".codex", "agents"), map[string][]byte{"notes.txt": []byte("x")})

	codex, _ := AgentAdapterByID(ProviderCodex)
	refs, err := AgentList(codex, ScopeUser)
	if err != nil {
		t.Fatalf("AgentList: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("AgentList = %#v, want 2 entries", refs)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
		if !strings.HasSuffix(r.Path, ".toml") {
			t.Errorf("ref %q path = %q, want a .toml suffix", r.Name, r.Path)
		}
	}
	if !names["reviewer"] || !names["planner"] {
		t.Errorf("names = %#v, want reviewer and planner", names)
	}
}

// TestAgentList_SkipsMissingDirAndDedupsByPriority exercises opencode's two
// own user dirs (plural "agents" dirs[0], legacy singular "agent"
// fallback — the C0 opencode fix): a name present in BOTH dirs is reported
// once (dirs[0]'s copy wins), and a dir that doesn't exist on disk at all
// is silently skipped rather than erroring.
func TestAgentList_SkipsMissingDirAndDedupsByPriority(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	opencode, ok := AgentAdapterByID(ProviderOpencode)
	if !ok {
		t.Fatalf("AgentAdapterByID(opencode): ok = false")
	}
	dirs := opencode.UserDirs()
	if len(dirs) != 2 {
		t.Fatalf("UserDirs() = %#v, want 2 entries (plural + legacy singular)", dirs)
	}

	// Only dirs[0] (plural) exists on disk — dirs[1] (singular) is
	// entirely absent, exercising the !dirExists(d) skip branch.
	writeFiles(t, dirs[0], map[string][]byte{
		"reviewer.md": []byte("---\ndescription: d\n---\n\nbody\n"),
	})

	refs, err := AgentList(opencode, ScopeUser)
	if err != nil {
		t.Fatalf("AgentList: %v", err)
	}
	if len(refs) != 1 || refs[0].Name != "reviewer" {
		t.Fatalf("AgentList = %#v, want exactly one [reviewer] (missing dir skipped)", refs)
	}

	// Now populate BOTH dirs with the same name — dirs[0]'s copy wins
	// (dedup by first-seen priority order).
	if err := os.MkdirAll(dirs[1], 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFiles(t, dirs[1], map[string][]byte{
		"reviewer.md": []byte("---\ndescription: legacy copy\n---\n\nbody\n"),
	})

	refs2, err := AgentList(opencode, ScopeUser)
	if err != nil {
		t.Fatalf("AgentList: %v", err)
	}
	if len(refs2) != 1 {
		t.Fatalf("AgentList = %#v, want exactly one deduped entry", refs2)
	}
	if !strings.HasPrefix(refs2[0].Path, dirs[0]) {
		t.Fatalf("Path = %q, want the dirs[0] (priority) copy to win", refs2[0].Path)
	}
}

func TestAgentList_ProjectScope(t *testing.T) {
	root := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	writeCodexAgent(t, filepath.Join(root, ".codex", "agents"), "reviewer")

	codex, _ := AgentAdapterByID(ProviderCodex)
	refs, err := AgentList(codex, ScopeProject)
	if err != nil {
		t.Fatalf("AgentList(project): %v", err)
	}
	if len(refs) != 1 || refs[0].Name != "reviewer" {
		t.Fatalf("AgentList(project) = %#v, want [reviewer]", refs)
	}
}

func TestAgentScan_UsesSourceProviderFileExtension(t *testing.T) {
	ag := &Agent{
		Name:        "reviewer",
		Description: "Reviews things thoroughly and carefully for quality issues.",
		Body:        strings.Repeat("safe instructions. ", 10),
		Provenance:  Provenance{SourceProvider: ProviderCodex},
	}
	result := AgentScan(ag)
	if result.Score <= 0 {
		t.Fatalf("Score = %d, want > 0 for a clean agent", result.Score)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("Findings = %#v, want none", result.Findings)
	}
}

func TestAgentScan_UnknownProviderFallsBackToMdExtension(t *testing.T) {
	// No Provenance set at all: AgentScan must not panic and must still
	// score/scan the body using the ".md" fallback label.
	ag := &Agent{Name: "reviewer", Description: "d", Body: "curl | sh"}
	result := AgentScan(ag)
	if len(result.Findings) == 0 {
		t.Fatalf("expected a dangerous-pattern finding for a curl-pipe-shell body")
	}
	if result.Findings[0].File != "reviewer.md" {
		t.Errorf("Findings[0].File = %q, want reviewer.md (fallback extension)", result.Findings[0].File)
	}
}

func TestAgentDetectedProviders_OnlyReturnsInstalledProviders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Nothing installed yet.
	if got := AgentDetectedProviders(); len(got) != 0 {
		t.Fatalf("AgentDetectedProviders() = %#v before anything is installed, want none", got)
	}

	if err := os.MkdirAll(filepath.Join(home, ".codex", "agents"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := AgentDetectedProviders()
	if len(got) != 1 || got[0].ID() != ProviderCodex {
		t.Fatalf("AgentDetectedProviders() = %#v, want exactly [codex]", got)
	}
}
