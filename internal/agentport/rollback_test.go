package agentport

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRollback_DeletesFilesAndDropsEntry(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := Write(plan); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := os.Stat(filepath.Join(plan.TargetPaths, "SKILL.md")); err != nil {
		t.Fatalf("expected SKILL.md to exist before rollback: %v", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entry, ok := LastEntry(m)
	if !ok {
		t.Fatalf("expected a manifest entry after Write")
	}

	if err := Rollback(entry, false); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if _, err := os.Stat(plan.TargetPaths); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed after rollback, stat err = %v", plan.TargetPaths, err)
	}

	m, err = LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("Entries = %#v, want none after rollback", m.Entries)
	}
}

func TestRollback_RefusesModifiedFileWithoutForce(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := Write(plan); err != nil {
		t.Fatalf("Write: %v", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entry, ok := LastEntry(m)
	if !ok {
		t.Fatalf("expected a manifest entry after Write")
	}

	// Modify one of the written files after the fact.
	skillMdPath := filepath.Join(plan.TargetPaths, "SKILL.md")
	if err := os.WriteFile(skillMdPath, []byte("modified by the user\n"), 0o644); err != nil {
		t.Fatalf("modify %s: %v", skillMdPath, err)
	}

	if err := Rollback(entry, false); err == nil {
		t.Fatalf("expected Rollback to refuse a modified file without --force")
	}

	// Nothing should have been deleted (verify pass runs before delete pass).
	if _, err := os.Stat(skillMdPath); err != nil {
		t.Fatalf("expected the modified file to remain: %v", err)
	}
	scriptPath := filepath.Join(plan.TargetPaths, "scripts", "run.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("expected the untouched resource file to remain too: %v", err)
	}

	// The manifest entry should still be present (rollback aborted).
	m, err = LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("Entries = %#v, want the entry to remain after a refused rollback", m.Entries)
	}

	// With --force, the modified file (and everything else) is deleted and
	// the entry is dropped.
	if err := Rollback(entry, true); err != nil {
		t.Fatalf("Rollback with --force: %v", err)
	}
	if _, err := os.Stat(skillMdPath); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed with --force, stat err = %v", skillMdPath, err)
	}
	m, err = LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("Entries = %#v, want none after forced rollback", m.Entries)
	}
}

// TestRollback_RoutesByKind_AgentEntryDoesNotTouchSkillFiles is the
// Rollback-level proof of P1.6's resolver (extended by the relay-standalone
// design §3e agent-write-path closure): an agent-kind manifest entry and a
// skill-kind manifest entry for the SAME provider+scope+name resolve to
// DIFFERENT target directories, so rolling back the agent entry never
// touches the skill's files, and vice versa.
//
// This now exercises the REAL on-disk flat `<name>.md` convention
// Claude/opencode agent files use: a skill's target dir is its own
// "<dir>/<name>/" subdirectory (TargetDir), while an agent's is the
// provider's shared directory itself (AgentTargetDir) containing a single
// "reviewer.md" — not a per-agent subdirectory. Rollback must therefore
// remove only the single file (never the shared directory, which may still
// hold other agents), leaving the skill's directory and files untouched.
func TestRollback_RoutesByKind_AgentEntryDoesNotTouchSkillFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	skillTarget, err := resolveTargetForEntry(ManifestEntry{Provider: ProviderClaude, Kind: KindSkill})
	if err != nil {
		t.Fatalf("resolveTargetForEntry (skill): %v", err)
	}
	agentTarget, err := resolveTargetForEntry(ManifestEntry{Provider: ProviderClaude, Kind: KindAgent})
	if err != nil {
		t.Fatalf("resolveTargetForEntry (agent): %v", err)
	}

	skillDir, err := TargetDir(skillTarget, ScopeUser, "reviewer")
	if err != nil {
		t.Fatalf("TargetDir (skill): %v", err)
	}
	agentDir, err := AgentTargetDir(agentTarget, ScopeUser)
	if err != nil {
		t.Fatalf("AgentTargetDir (agent): %v", err)
	}
	if skillDir == agentDir {
		t.Fatalf("skill and agent target dirs must differ, both = %s", skillDir)
	}

	writeFiles(t, skillDir, map[string][]byte{"marker.md": []byte("skill file\n")})
	writeFiles(t, agentDir, map[string][]byte{"reviewer.md": []byte("agent file\n")})

	agentEntry := ManifestEntry{
		Name:        "reviewer",
		Kind:        KindAgent,
		Provider:    ProviderClaude,
		Scope:       ScopeUser,
		TargetPaths: HashFiles(map[string][]byte{"reviewer.md": []byte("agent file\n")}),
	}
	if err := RecordEntry(agentEntry); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	recorded, ok := LastEntryFor(m, "reviewer", ProviderClaude, ScopeUser, KindAgent)
	if !ok {
		t.Fatalf("expected the recorded agent entry")
	}

	if err := Rollback(recorded, false); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if _, err := os.Stat(filepath.Join(agentDir, "reviewer.md")); !os.IsNotExist(err) {
		t.Fatalf("expected the agent file %s/reviewer.md to be removed, stat err = %v", agentDir, err)
	}
	if _, err := os.Stat(agentDir); err != nil {
		t.Fatalf("expected the shared agent directory %s to remain (only the one file is this entry's to delete): %v", agentDir, err)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "marker.md")); err != nil {
		t.Fatalf("expected the skill's marker file to remain untouched: %v", err)
	}
}

func TestRollback_MissingFilesAreTreatedAsAlreadyGone(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := Write(plan); err != nil {
		t.Fatalf("Write: %v", err)
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entry, ok := LastEntry(m)
	if !ok {
		t.Fatalf("expected a manifest entry")
	}

	// The user already deleted the whole skill dir by hand.
	if err := os.RemoveAll(plan.TargetPaths); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	if err := Rollback(entry, false); err != nil {
		t.Fatalf("Rollback should succeed when files are already gone: %v", err)
	}

	m, err = LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("Entries = %#v, want none after rollback", m.Entries)
	}
}
