package txn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCompensate_RejectsSymlinkSwappedAfterApply_NewFile proves compensate
// re-runs the same TOCTOU symlink check Apply does before every write: a
// legitimate operation stages and applies a brand-new file under a
// subdirectory, then an attacker replaces that subdirectory with a symlink
// to an outside directory before the ledger callback fails and triggers
// compensation. Without the fix, compensate's plain os.Remove would follow
// the symlink and delete whatever the attacker's outside directory
// contains; with it, the symlink check aborts compensation instead.
func TestCompensate_RejectsSymlinkSwappedAfterApply_NewFile(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	subdir := filepath.Join(root, "sub")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	outsideDir := t.TempDir()
	canaryFile := filepath.Join(outsideDir, "file.txt")
	if err := os.WriteFile(canaryFile, []byte("attacker's outside content"), 0o644); err != nil {
		t.Fatalf("seed outside canary: %v", err)
	}

	tx, err := Begin(context.Background(), "test write", []string{"skill:demo"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Stage(root, "sub/file.txt", []byte("new content"), 0o644); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := tx.Apply(VerifyNoSymlinks); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Attacker swaps the "sub" path component for a symlink to an outside
	// directory in the window between the successful Apply and the
	// ledger-failure compensation triggered below.
	if err := os.RemoveAll(subdir); err != nil {
		t.Fatalf("remove sub: %v", err)
	}
	if err := os.Symlink(outsideDir, subdir); err != nil {
		t.Fatalf("symlink sub -> outside: %v", err)
	}

	err = tx.Commit(func() error { return errors.New("ledger unavailable") })
	if err == nil {
		t.Fatal("Commit should fail: ledger failed and compensation must be rejected by the symlink check")
	}

	got, rerr := os.ReadFile(canaryFile)
	if rerr != nil {
		t.Fatalf("outside canary file should still exist: %v", rerr)
	}
	if string(got) != "attacker's outside content" {
		t.Fatalf("outside canary content = %q, want untouched", got)
	}

	j, lerr := Load(tx.ID())
	if lerr != nil {
		t.Fatalf("Load: %v", lerr)
	}
	if j.State != StateRecovery {
		t.Fatalf("journal state = %q, want %q (compensation blocked by symlink check)", j.State, StateRecovery)
	}
}

// TestCompensate_RejectsSymlinkSwappedAfterApply_Restore is the
// Existed=true analogue: compensating a replaced file re-writes the
// backed-up prior bytes via atomicWrite. Without the fix this is an
// arbitrary-file-write primitive through the swapped symlink; with it, the
// same TOCTOU check that guards new-file removal also guards the restore
// write.
func TestCompensate_RejectsSymlinkSwappedAfterApply_Restore(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	subdir := filepath.Join(root, "sub")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "existing.txt"), []byte("original"), 0o644); err != nil {
		t.Fatalf("seed existing.txt: %v", err)
	}

	outsideDir := t.TempDir()

	tx, err := Begin(context.Background(), "test write", []string{"skill:demo"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Stage(root, "sub/existing.txt", []byte("new content"), 0o644); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := tx.Apply(VerifyNoSymlinks); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Attacker swaps "sub" for a symlink to an outside directory before
	// compensation runs.
	if err := os.RemoveAll(subdir); err != nil {
		t.Fatalf("remove sub: %v", err)
	}
	if err := os.Symlink(outsideDir, subdir); err != nil {
		t.Fatalf("symlink sub -> outside: %v", err)
	}

	err = tx.Commit(func() error { return errors.New("ledger unavailable") })
	if err == nil {
		t.Fatal("Commit should fail: compensation must be rejected by the symlink check")
	}

	if _, statErr := os.Stat(filepath.Join(outsideDir, "existing.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("compensation must never write backup bytes through the swapped symlink, stat err = %v", statErr)
	}
}

// TestRecover_ReconcilesUnpersistedAppliedFlag simulates the exact crash
// window between atomicWrite landing and the following journal save (which
// records Applied=true) surviving: the destination already has the new
// content, but the on-disk journal still says Applied=false. Recover must
// detect the mismatch (via NewHash) and restore from backup rather than
// reporting Restored while silently leaving the new content in place.
func TestRecover_ReconcilesUnpersistedAppliedFlag(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	dest := filepath.Join(root, "file.txt")
	if err := os.WriteFile(dest, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed original: %v", err)
	}

	tx, err := Begin(context.Background(), "test write", []string{"skill:demo"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	newData := []byte("new content")
	if err := tx.Stage(root, "file.txt", newData, 0o644); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	id := tx.ID()

	// Simulate the write landing without the per-file Applied=true save
	// surviving the crash: the destination has new content, but the
	// journal on disk still records Applied=false.
	if err := atomicWrite(dest, newData, 0o644); err != nil {
		t.Fatalf("simulate landed write: %v", err)
	}
	tx.j.State = StateApplying
	if err := tx.j.save(); err != nil {
		t.Fatalf("save (Applied still false): %v", err)
	}
	tx.locks.Unlock() // simulate the OS releasing the flock on process exit

	j, err := Recover(context.Background(), id)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if j.State != StateRestored {
		t.Fatalf("journal state = %q, want %q", j.State, StateRestored)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "original" {
		t.Fatalf("dest = %q, want restored to %q — Recover must not report success while new content silently remains", got, "original")
	}
}
