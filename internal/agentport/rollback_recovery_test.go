package agentport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport/txn"
)

// TestRollback_RestoresPriorBytesAndModeFromLinkedTransaction proves the
// actual restore guarantee: rolling back an entry whose transaction
// recorded prior content brings back the ORIGINAL bytes and mode, not
// just a deletion. The "overwrite" this simulates isn't reachable through
// today's CLI (fresh installs still refuse a conflicting destination; an
// explicit replace/adopt verb doesn't exist yet), so this drives the same
// txn primitive Write uses directly, exactly like a future replace
// operation's write would.
func TestRollback_RestoresPriorBytesAndModeFromLinkedTransaction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dir, err := TargetDir(NewClaudeAdapter(), ScopeUser, "demo-skill")
	if err != nil {
		t.Fatalf("TargetDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	skillPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("original content\n"), 0o755); err != nil {
		t.Fatalf("seed original: %v", err)
	}

	tx, err := txn.Begin(context.Background(), "test replace", []string{"test:demo-skill:replace"})
	if err != nil {
		t.Fatalf("txn.Begin: %v", err)
	}
	if err := tx.Stage(dir, "SKILL.md", []byte("new content\n"), 0o644); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := tx.Apply(func(string, string) error { return nil }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	journalID := tx.ID()
	if err := tx.Commit(func() error { return nil }); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := os.ReadFile(skillPath)
	if err != nil || string(got) != "new content\n" {
		t.Fatalf("expected the simulated replace to have landed, got %q, err %v", got, err)
	}

	newHash := sha256.Sum256([]byte("new content\n"))
	entry := ManifestEntry{
		ID:            "manual-replace-1",
		Name:          "demo-skill",
		Kind:          KindSkill,
		Provider:      ProviderClaude,
		Scope:         ScopeUser,
		TargetPaths:   map[string]string{"SKILL.md": hex.EncodeToString(newHash[:])},
		TransactionID: journalID,
	}
	if err := RecordEntry(entry); err != nil {
		t.Fatalf("RecordEntry: %v", err)
	}

	if err := Rollback(entry, false); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	restored, err := os.Stat(skillPath)
	if err != nil {
		t.Fatalf("SKILL.md should still exist (restored, not removed): %v", err)
	}
	if restored.Mode().Perm() != 0o755 {
		t.Fatalf("restored mode = %o, want %o", restored.Mode().Perm(), 0o755)
	}
	gotContent, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read restored SKILL.md: %v", err)
	}
	if string(gotContent) != "original content\n" {
		t.Fatalf("restored content = %q, want %q", gotContent, "original content\n")
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("entry should be dropped after rollback, got %#v", m.Entries)
	}
}

// TestRollback_InterruptedApplyLeavesFilesIntactAndResumable forces one of
// two files to fail during Rollback's own Apply step (by making its
// parent directory unwritable) after the other has already been removed.
// Rollback must compensate the completed removal back to its
// pre-rollback state — leaving the artifact exactly as it was before the
// rollback attempt, not half-rolled-back — and the manifest entry must
// still be present so a corrected retry can complete the job.
func TestRollback_InterruptedApplyLeavesFilesIntactAndResumable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

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

	scriptsDir := filepath.Join(plan.TargetPaths, "scripts")
	if _, err := os.Stat(filepath.Join(scriptsDir, "run.sh")); err != nil {
		t.Fatalf("fixture missing scripts/run.sh: %v", err)
	}
	if err := os.Chmod(scriptsDir, 0o555); err != nil {
		t.Fatalf("chmod scripts read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(scriptsDir, 0o755) })

	if err := Rollback(entry, false); err == nil {
		t.Fatal("Rollback should fail: scripts/run.sh cannot be removed from a read-only directory")
	}

	if _, err := os.Stat(filepath.Join(plan.TargetPaths, "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md should have been restored by compensation, not left removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(scriptsDir, "run.sh")); err != nil {
		t.Fatalf("scripts/run.sh should be untouched (its removal never applied): %v", err)
	}

	m, err = LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("entry should still be present after an interrupted rollback, got %#v", m.Entries)
	}

	journals, err := txn.List()
	if err != nil {
		t.Fatalf("txn.List: %v", err)
	}
	if len(journals) != 2 { // the original install's journal, plus the interrupted rollback's
		t.Fatalf("expected 2 journals (install + interrupted rollback), got %#v", journals)
	}

	// Clear the injected fault and retry — a corrected retry must now
	// complete cleanly, since compensation left everything exactly as the
	// original install wrote it.
	if err := os.Chmod(scriptsDir, 0o755); err != nil {
		t.Fatalf("chmod scripts writable: %v", err)
	}
	if err := Rollback(entry, false); err != nil {
		t.Fatalf("retried Rollback should succeed: %v", err)
	}
	if _, err := os.Stat(plan.TargetPaths); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed after the successful retry, stat err = %v", plan.TargetPaths, err)
	}
}
