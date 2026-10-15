package agentport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport/txn"
)

// TestWrite_PartialFailureLeavesNothingBehind makes the target root
// read-only after staging has already begun conceptually (the directory
// exists, but nothing can be created inside it), so every file's Apply
// step fails. Write must leave no file behind and must never record a
// manifest entry for a write that didn't fully land (changed files with
// no ledger record). The interrupted
// transaction itself must be left in a terminal (Restored) state, not
// silently discarded, so `relay history` can still show it happened.
func TestWrite_PartialFailureLeavesNothingBehind(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(plan.Files) < 2 {
		t.Fatalf("fixture needs at least 2 files to exercise a mid-apply failure, got %v", keysOf(plan.Files))
	}

	if err := os.MkdirAll(plan.TargetPaths, 0o755); err != nil {
		t.Fatalf("mkdir target root: %v", err)
	}
	if err := os.Chmod(plan.TargetPaths, 0o555); err != nil {
		t.Fatalf("chmod target root read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(plan.TargetPaths, 0o755) }) // let TempDir cleanup remove it

	if err := Write(plan); err == nil {
		t.Fatal("Write should have failed against a read-only target root")
	}

	for rel := range plan.Files {
		if _, err := os.Stat(filepath.Join(plan.TargetPaths, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("%s should not exist after a failed write, stat err = %v", rel, err)
		}
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("manifest should have no entries after a failed write, got %d", len(m.Entries))
	}

	journals, err := txn.List()
	if err != nil {
		t.Fatalf("txn.List: %v", err)
	}
	if len(journals) != 1 || journals[0].State != txn.StateRestored {
		t.Fatalf("expected exactly one Restored journal, got %#v", journals)
	}
}

// TestWrite_RefusesWhilePriorJournalPending proves an interrupted journal
// blocks new writes: a manually-injected pending journal for the same artifact
// (standing in for a crashed prior `relay` process) makes a fresh Write
// refuse outright rather than silently proceeding alongside it.
func TestWrite_RefusesWhilePriorJournalPending(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	src, err := NewClaudeAdapter().Load(filepath.Join("testdata", "claude", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := Migrate(src, NewCodexAdapter(), ScopeUser)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A journal file existing on disk in a non-terminal state is what
	// Write's pending-check looks for — it's checked before any lock is
	// even attempted, so this "stale" transaction's own lock deliberately
	// stays held (as a real crashed process's would have, until the OS
	// released it) without affecting the assertion below.
	key := skillLockKey(plan.Target.ID(), plan.Scope, plan.Skill.Name, "")
	stale, err := txn.Begin(context.Background(), "stale write", []string{key})
	if err != nil {
		t.Fatalf("Begin (simulated crashed process): %v", err)
	}
	if err := stale.Stage(plan.TargetPaths, "SKILL.md", []byte("stale"), 0o644); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := stale.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	err = Write(plan)
	if err == nil {
		t.Fatal("Write should refuse while a pending journal exists for this artifact")
	}
	if !errors.Is(err, txn.ErrPending) {
		t.Fatalf("Write error = %v, want it to wrap txn.ErrPending", err)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
