package txn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// isolate points HOME at a fresh t.TempDir() so every test operates only
// under its own throwaway directory — never a real user's
// ~/.config/relay/transactions.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// noopVerify is the injected symlink-check function tests use when they
// don't care about containment — production callers pass
// agentport.verifyPathHasNoSymlinks instead.
func noopVerify(string, string) error { return nil }

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func TestApply_HappyPath(t *testing.T) {
	isolate(t)
	root := t.TempDir()

	tx, err := Begin(context.Background(), "test write", []string{"skill:claude:user:demo:"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Stage(root, "SKILL.md", []byte("new content"), 0o644); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := tx.Apply(noopVerify); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	ledgerCalled := false
	if err := tx.Commit(func() error { ledgerCalled = true; return nil }); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !ledgerCalled {
		t.Fatal("Commit did not run the ledger callback")
	}

	got := mustRead(t, filepath.Join(root, "SKILL.md"))
	if string(got) != "new content" {
		t.Fatalf("destination content = %q, want %q", got, "new content")
	}

	j, err := Load(tx.ID())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if j.State != StateCommitted {
		t.Fatalf("journal state = %q, want %q", j.State, StateCommitted)
	}
}

// TestRecover_CrashAfterPersistBeforeApply simulates a process that died
// right after the durable journal was written but before any destination
// write was attempted — recovery must leave the (nonexistent) destination
// untouched and mark the journal Restored, never Committed.
func TestRecover_CrashAfterPersistBeforeApply(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	dest := filepath.Join(root, "SKILL.md")

	tx, err := Begin(context.Background(), "test write", []string{"skill:claude:user:demo:"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Stage(root, "SKILL.md", []byte("new content"), 0o644); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	id := tx.ID()

	// Simulate the process dying here: the kernel releases the flock the
	// instant the process exits, without anything else running.
	tx.locks.Unlock()

	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("destination should not exist before recovery, stat err = %v", err)
	}

	j, err := Recover(context.Background(), id)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if j.State != StateRestored {
		t.Fatalf("journal state after recover = %q, want %q", j.State, StateRestored)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("destination should still not exist after recovery, stat err = %v", err)
	}
}

// TestDiscard_MidApplyFailureRestoresAppliedFile stages two files — one
// replacing a pre-existing file, one brand new — then forces Apply to
// fail on the second file after the first has already landed. Discard
// must restore the first file's original bytes and leave the second
// destination absent, exactly as if the write never happened.
func TestDiscard_MidApplyFailureRestoresAppliedFile(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	existingPath := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(existingPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	tx, err := Begin(context.Background(), "test write", []string{"skill:claude:user:demo:"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Stage(root, "existing.txt", []byte("replaced"), 0o644); err != nil {
		t.Fatalf("Stage existing: %v", err)
	}
	if err := tx.Stage(root, "new.txt", []byte("brand new"), 0o644); err != nil {
		t.Fatalf("Stage new: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	failOn := "new.txt"
	verify := func(root, rel string) error {
		if rel == failOn {
			return errors.New("injected symlink-check failure")
		}
		return nil
	}

	applyErr := tx.Apply(verify)
	if applyErr == nil {
		t.Fatal("Apply should have failed on new.txt")
	}

	if err := tx.Discard(applyErr); !errors.Is(err, applyErr) {
		t.Fatalf("Discard returned %v, want it to wrap %v", err, applyErr)
	}

	got := mustRead(t, existingPath)
	if string(got) != "original" {
		t.Fatalf("existing.txt = %q, want restored %q", got, "original")
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt should never have been created, stat err = %v", err)
	}

	j, err := Load(tx.ID())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if j.State != StateRestored {
		t.Fatalf("journal state = %q, want %q", j.State, StateRestored)
	}
}

// TestRecover_AfterProcessDeathMidApply is Discard's scenario but without
// ever calling Discard — the process is simulated dying between the
// partial Apply and any cleanup, exactly like a killed `relay` process.
// A fresh Recover call (as `relay recover` would issue) must still
// compensate correctly.
func TestRecover_AfterProcessDeathMidApply(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	existingPath := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(existingPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	tx, err := Begin(context.Background(), "test write", []string{"skill:claude:user:demo:"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Stage(root, "existing.txt", []byte("replaced"), 0o644); err != nil {
		t.Fatalf("Stage existing: %v", err)
	}
	if err := tx.Stage(root, "new.txt", []byte("brand new"), 0o644); err != nil {
		t.Fatalf("Stage new: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	id := tx.ID()

	verify := func(root, rel string) error {
		if rel == "new.txt" {
			return errors.New("injected failure")
		}
		return nil
	}
	_ = tx.Apply(verify) // first file lands, second fails — process "dies" right here
	tx.locks.Unlock()    // simulate the OS releasing the flock on process exit

	// A concurrent/next invocation must notice the pending journal rather
	// than proceeding as if nothing happened.
	if _, err := Begin(context.Background(), "second write", []string{"skill:claude:user:demo:"}); !errors.Is(err, ErrPending) {
		t.Fatalf("Begin during pending recovery = %v, want ErrPending", err)
	}

	j, err := Recover(context.Background(), id)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if j.State != StateRestored {
		t.Fatalf("journal state = %q, want %q", j.State, StateRestored)
	}
	got := mustRead(t, existingPath)
	if string(got) != "original" {
		t.Fatalf("existing.txt = %q, want restored %q", got, "original")
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt should not exist after recovery, stat err = %v", err)
	}

	// Now that recovery is terminal, a fresh operation on the same key
	// must be allowed to proceed.
	tx2, err := Begin(context.Background(), "third write", []string{"skill:claude:user:demo:"})
	if err != nil {
		t.Fatalf("Begin after recovery: %v", err)
	}
	tx2.locks.Unlock()
}

// TestCommit_LedgerFailureRestoresNewFile proves the ledger update is
// part of the same recoverable unit as the file writes: if it fails after
// every file already landed, Commit must compensate them away rather
// than leaving a written-but-unrecorded artifact.
func TestCommit_LedgerFailureRestoresNewFile(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	dest := filepath.Join(root, "SKILL.md")

	tx, err := Begin(context.Background(), "test write", []string{"skill:claude:user:demo:"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Stage(root, "SKILL.md", []byte("new content"), 0o644); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := tx.Apply(noopVerify); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("file should exist after Apply: %v", err)
	}

	ledgerErr := errors.New("ledger unavailable")
	err = tx.Commit(func() error { return ledgerErr })
	if err == nil {
		t.Fatal("Commit should have failed")
	}

	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("file should have been removed by compensation, stat err = %v", statErr)
	}

	j, loadErr := Load(tx.ID())
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if j.State != StateRestored {
		t.Fatalf("journal state = %q, want %q", j.State, StateRestored)
	}
}

func TestPrune_RefusesNonTerminalJournal(t *testing.T) {
	isolate(t)
	root := t.TempDir()

	tx, err := Begin(context.Background(), "test write", []string{"skill:claude:user:demo:"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Stage(root, "SKILL.md", []byte("x"), 0o644); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	id := tx.ID()
	tx.locks.Unlock() // leave it pending, as if crashed right after Persist

	if err := Prune(id); err == nil {
		t.Fatal("Prune should refuse a Prepared (non-terminal) journal")
	}

	if _, err := Recover(context.Background(), id); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := Prune(id); err != nil {
		t.Fatalf("Prune after recovery should succeed: %v", err)
	}
	if _, err := Load(id); err == nil {
		t.Fatal("journal should be gone after Prune")
	}
}

func TestLoad_RefusesFutureSchema(t *testing.T) {
	isolate(t)
	id := "1700000000000000000-aaaaaaaaaaaaaaaa"
	j := &Journal{Schema: schemaVersion + 1, ID: id, State: StatePrepared}
	if err := j.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := Load(id); !errors.Is(err, ErrFutureSchema) {
		t.Fatalf("Load future schema = %v, want ErrFutureSchema", err)
	}
}

// TestApply_RemovalMidwayFailure_DiscardRestores mirrors
// TestDiscard_MidApplyFailureRestoresAppliedFile but for StageRemoval —
// Rollback's mechanism. Two files are staged for removal in a fixed
// order; the second's verify is forced to fail after the first has
// already been removed. Discard must bring the first file back exactly
// as it was.
func TestApply_RemovalMidwayFailure_DiscardRestores(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	firstPath := filepath.Join(root, "first.txt")
	secondPath := filepath.Join(root, "second.txt")
	if err := os.WriteFile(firstPath, []byte("keep me safe"), 0o644); err != nil {
		t.Fatalf("seed first.txt: %v", err)
	}
	if err := os.WriteFile(secondPath, []byte("also here"), 0o644); err != nil {
		t.Fatalf("seed second.txt: %v", err)
	}

	tx, err := Begin(context.Background(), "test rollback", []string{"rollback:demo"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.StageRemoval(root, "first.txt"); err != nil {
		t.Fatalf("StageRemoval first: %v", err)
	}
	if err := tx.StageRemoval(root, "second.txt"); err != nil {
		t.Fatalf("StageRemoval second: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	verify := func(_, rel string) error {
		if rel == "second.txt" {
			return errors.New("injected failure")
		}
		return nil
	}
	applyErr := tx.Apply(verify)
	if applyErr == nil {
		t.Fatal("Apply should have failed on second.txt")
	}
	if _, err := os.Stat(firstPath); !os.IsNotExist(err) {
		t.Fatalf("first.txt should have been removed by Apply before the failure, stat err = %v", err)
	}

	if err := tx.Discard(applyErr); !errors.Is(err, applyErr) {
		t.Fatalf("Discard = %v, want it to wrap %v", err, applyErr)
	}

	got := mustRead(t, firstPath)
	if string(got) != "keep me safe" {
		t.Fatalf("first.txt = %q, want restored %q", got, "keep me safe")
	}
	got = mustRead(t, secondPath)
	if string(got) != "also here" {
		t.Fatalf("second.txt = %q, want untouched %q", got, "also here")
	}

	j, err := Load(tx.ID())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if j.State != StateRestored {
		t.Fatalf("journal state = %q, want %q", j.State, StateRestored)
	}
}

// TestRecover_AfterProcessDeathMidRemoval is
// TestRecover_AfterProcessDeathMidApply's removal analogue — the process
// dies after the first file's removal lands but before Discard/Commit
// ever runs. A fresh Recover call must still restore it.
func TestRecover_AfterProcessDeathMidRemoval(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	firstPath := filepath.Join(root, "first.txt")
	secondPath := filepath.Join(root, "second.txt")
	if err := os.WriteFile(firstPath, []byte("keep me safe"), 0o644); err != nil {
		t.Fatalf("seed first.txt: %v", err)
	}
	if err := os.WriteFile(secondPath, []byte("also here"), 0o644); err != nil {
		t.Fatalf("seed second.txt: %v", err)
	}

	tx, err := Begin(context.Background(), "test rollback", []string{"rollback:demo"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.StageRemoval(root, "first.txt"); err != nil {
		t.Fatalf("StageRemoval first: %v", err)
	}
	if err := tx.StageRemoval(root, "second.txt"); err != nil {
		t.Fatalf("StageRemoval second: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	id := tx.ID()

	verify := func(_, rel string) error {
		if rel == "second.txt" {
			return errors.New("injected failure")
		}
		return nil
	}
	_ = tx.Apply(verify) // first.txt removed, second.txt fails — process "dies" right here
	tx.locks.Unlock()    // simulate the OS releasing the flock on process exit

	j, err := Recover(context.Background(), id)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if j.State != StateRestored {
		t.Fatalf("journal state = %q, want %q", j.State, StateRestored)
	}
	got := mustRead(t, firstPath)
	if string(got) != "keep me safe" {
		t.Fatalf("first.txt = %q, want restored %q", got, "keep me safe")
	}
	got = mustRead(t, secondPath)
	if string(got) != "also here" {
		t.Fatalf("second.txt = %q, want untouched %q", got, "also here")
	}
}

func TestBegin_AcquiresLocksInDeterministicOrder(t *testing.T) {
	isolate(t)
	// Two overlapping requests submitted with keys in opposite order must
	// still serialize consistently rather than deadlocking — Begin sorts
	// before acquiring.
	tx, err := Begin(context.Background(), "multi", []string{"b-key", "a-key"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.locks.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	if _, err := Begin(ctx, "conflict", []string{"a-key", "c-key"}); err == nil {
		t.Fatal("overlapping Begin should have blocked/failed while a-key is held")
	}
}
