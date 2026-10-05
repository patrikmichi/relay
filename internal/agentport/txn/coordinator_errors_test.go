package txn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testKey = "skill:claude:user:demo:"

func beginTx(t *testing.T, keys ...string) *Tx {
	t.Helper()
	if len(keys) == 0 {
		keys = []string{testKey}
	}
	tx, err := Begin(context.Background(), "test write", keys)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return tx
}

// leavePending stages and persists one new file, then drops the locks as a
// crashed process would, returning the journal id.
func leavePending(t *testing.T, root, rel string, keys ...string) string {
	t.Helper()
	tx := beginTx(t, keys...)
	if err := tx.Stage(root, rel, []byte("new"), 0o644); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	tx.locks.Unlock()
	return tx.ID()
}

func TestBegin_RefusesOverlappingPendingJournal(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	id := leavePending(t, root, "a.txt", "k1", "k2")

	_, err := Begin(context.Background(), "other", []string{"k2"})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("Begin = %v, want ErrPending", err)
	}
	if !strings.Contains(err.Error(), id) {
		t.Fatalf("error %q does not name pending journal %s", err, id)
	}

	tx, err := Begin(context.Background(), "disjoint", []string{"k3"})
	if err != nil {
		t.Fatalf("Begin with disjoint keys: %v", err)
	}
	tx.locks.Unlock()
}

func TestBegin_HomeUnusable(t *testing.T) {
	home := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(home, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if _, err := Begin(context.Background(), "x", []string{testKey}); err == nil {
		t.Fatal("Begin should fail when the transactions dir cannot be created")
	}
	if _, err := AcquireAll(context.Background(), []string{testKey}); err == nil {
		t.Fatal("AcquireAll should fail when the lock dir cannot be created")
	}
	if _, err := Dir("1-aaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("Dir should fail when the transactions dir cannot be created")
	}
	if _, err := RecoverAll(context.Background()); err == nil {
		t.Fatal("RecoverAll should fail when the transactions dir cannot be created")
	}
}

func TestAcquireAll_WaitsForHolderAndHonoursContext(t *testing.T) {
	isolate(t)
	held, err := AcquireAll(context.Background(), []string{"b", "a"})
	if err != nil {
		t.Fatalf("AcquireAll: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*pollInterval)
	defer cancel()
	if _, err := AcquireAll(ctx, []string{"c", "a"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcquireAll while held = %v, want DeadlineExceeded", err)
	}

	// The failed attempt must have released "c" on its way out.
	c, err := AcquireAll(context.Background(), []string{"c"})
	if err != nil {
		t.Fatalf("AcquireAll c after failed attempt: %v", err)
	}
	c.Unlock()

	done := make(chan error, 1)
	go func() {
		ls, err := AcquireAll(context.Background(), []string{"a"})
		if err == nil {
			ls.Unlock()
		}
		done <- err
	}()
	time.Sleep(2 * pollInterval)
	held.Unlock()
	held.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiter: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never acquired the released lock")
	}
}

func TestStage_RejectsUnsupportedDestinations(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "dir"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	tx := beginTx(t)
	defer tx.locks.Unlock()

	for _, rel := range []string{"dir", "link"} {
		if err := tx.Stage(root, rel, []byte("x"), 0o644); err == nil {
			t.Errorf("Stage(%s) should fail", rel)
		}
		if err := tx.StageRemoval(root, rel); err == nil {
			t.Errorf("StageRemoval(%s) should fail", rel)
		}
	}
	if len(tx.j.Files) != 0 {
		t.Fatalf("rejected destinations were recorded: %+v", tx.j.Files)
	}
}

func TestStage_UnreadableParentIsAnError(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	skipWithoutPermissionChecks(t)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	tx := beginTx(t)
	defer tx.locks.Unlock()
	if err := tx.Stage(root, "locked/f", []byte("y"), 0o644); err == nil {
		t.Fatal("Stage should surface a stat permission error")
	}
	if err := tx.StageRemoval(root, "locked/f"); err == nil {
		t.Fatal("StageRemoval should surface a stat permission error")
	}
}

func TestStage_UnreadableExistingFile(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	dest := filepath.Join(root, "f")
	skipWithoutPermissionChecks(t)
	if err := os.WriteFile(dest, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	tx := beginTx(t)
	defer tx.locks.Unlock()
	if err := tx.Stage(root, "f", []byte("y"), 0o644); err == nil {
		t.Fatal("Stage should fail when the existing destination is unreadable")
	}
	if err := tx.StageRemoval(root, "f"); err == nil {
		t.Fatal("StageRemoval should fail when the existing destination is unreadable")
	}
}

func TestStage_TransactionDirGone(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "old"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tx := beginTx(t)
	defer tx.locks.Unlock()
	if err := os.RemoveAll(tx.dir); err != nil {
		t.Fatal(err)
	}
	if err := tx.Stage(root, "new", []byte("y"), 0o644); err == nil {
		t.Fatal("Stage should fail when the staged file cannot be written")
	}
	if err := tx.Stage(root, "old", []byte("y"), 0o644); err == nil {
		t.Fatal("Stage should fail when the backup cannot be written")
	}
	if err := tx.StageRemoval(root, "old"); err == nil {
		t.Fatal("StageRemoval should fail when the backup cannot be written")
	}
}

func TestStageRemoval_MissingDestinationIsNoop(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	tx := beginTx(t)
	if err := tx.StageRemoval(root, "absent.txt"); err != nil {
		t.Fatalf("StageRemoval: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := tx.Apply(noopVerify); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := tx.Commit(func() error { return nil }); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "absent.txt")); !os.IsNotExist(err) {
		t.Fatalf("absent destination appeared: %v", err)
	}
}

func TestApply_VerifyRejectionLeavesDestinationUntouched(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	tx := beginTx(t)
	if err := tx.Stage(root, "f", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	err := tx.Apply(func(string, string) error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("Apply = %v, want wrapped verify error", err)
	}
	if got := tx.Discard(err); !errors.Is(got, refused) {
		t.Fatalf("Discard = %v, want original error", got)
	}
	if _, err := os.Stat(filepath.Join(root, "f")); !os.IsNotExist(err) {
		t.Fatalf("destination written despite rejection: %v", err)
	}
}

func TestApply_MissingStagedContent(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	tx := beginTx(t)
	if err := tx.Stage(root, "f", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(tx.dir, tx.j.Files[0].StagedPath)); err != nil {
		t.Fatal(err)
	}
	err := tx.Apply(noopVerify)
	if err == nil || !strings.Contains(err.Error(), "read staged content") {
		t.Fatalf("Apply = %v, want staged read error", err)
	}
	if err := tx.Discard(err); err == nil {
		t.Fatal("Discard should return the apply error")
	}
}

func TestApply_ParentIsAFile(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	tx := beginTx(t)
	if err := tx.Stage(root, "sub/f", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub"), []byte("blocker"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := tx.Apply(noopVerify)
	if err == nil || !strings.Contains(err.Error(), "create dir") {
		t.Fatalf("Apply = %v, want create dir error", err)
	}
	_ = tx.Discard(err)
	if got := mustRead(t, filepath.Join(root, "sub")); string(got) != "blocker" {
		t.Fatalf("unrelated file changed to %q", got)
	}
}

func TestApply_RemovalOfNonEmptyDirectoryFails(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	tx := beginTx(t)
	if err := tx.StageRemoval(root, "d"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "d", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := tx.Apply(noopVerify)
	if err == nil || !strings.Contains(err.Error(), "remove") {
		t.Fatalf("Apply = %v, want remove error", err)
	}
	_ = tx.Discard(err)
}

func TestApply_JournalUnwritable(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	tx := beginTx(t)
	defer tx.locks.Unlock()
	if err := tx.Stage(root, "f", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	tx.j.ID = "../escape"
	if err := tx.Apply(noopVerify); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("Apply = %v, want ErrInvalidID from journal save", err)
	}
}

func TestDiscard_BeforePersistLeavesNothingBehind(t *testing.T) {
	isolate(t)
	tx := beginTx(t)
	cause := errors.New("preflight failed")
	if err := tx.Discard(cause); err != cause {
		t.Fatalf("Discard = %v, want the cause unchanged", err)
	}
	if _, err := Load(tx.ID()); err == nil {
		t.Fatal("no journal should exist when Persist never ran")
	}
	// Locks must be free again.
	again := beginTx(t)
	again.locks.Unlock()
}

// corruptBackup applies a replacement of an existing file and then tampers
// with its backup, so compensation can't restore it.
func corruptBackup(t *testing.T, root string) *Tx {
	t.Helper()
	dest := filepath.Join(root, "f")
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	tx := beginTx(t)
	if err := tx.Stage(root, "f", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Apply(noopVerify); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tx.dir, tx.j.Files[0].BackupPath), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestDiscard_CompensationFailureNeedsRecovery(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	tx := corruptBackup(t, root)

	err := tx.Discard(errors.New("boom"))
	if !errors.Is(err, ErrPending) || !strings.Contains(err.Error(), tx.ID()) {
		t.Fatalf("Discard = %v, want ErrPending naming %s", err, tx.ID())
	}
	j, lerr := Load(tx.ID())
	if lerr != nil {
		t.Fatal(lerr)
	}
	if j.State != StateRecovery || !strings.Contains(j.Err, "corrupt") {
		t.Fatalf("journal = state %q err %q, want recovery with corrupt-backup reason", j.State, j.Err)
	}

	if _, err := Recover(context.Background(), tx.ID()); err == nil {
		t.Fatal("Recover should keep failing while the backup is corrupt")
	}
	j, _ = Load(tx.ID())
	if j.State != StateRecovery {
		t.Fatalf("state after failed recover = %q, want recovery", j.State)
	}
	if err := Prune(tx.ID()); err == nil {
		t.Fatal("Prune must refuse a journal still in recovery")
	}
}

func TestCommit_LedgerAndCompensationFailure(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	tx := corruptBackup(t, root)

	ledgerErr := errors.New("ledger down")
	err := tx.Commit(func() error { return ledgerErr })
	if !errors.Is(err, ledgerErr) || !strings.Contains(err.Error(), "relay recover "+tx.ID()) {
		t.Fatalf("Commit = %v, want ledger error with recover hint", err)
	}
	j, _ := Load(tx.ID())
	if j.State != StateRecovery {
		t.Fatalf("state = %q, want recovery", j.State)
	}
	if got := mustRead(t, filepath.Join(root, "f")); string(got) != "new" {
		t.Fatalf("destination = %q; an unverifiable backup must not be written", got)
	}
}

func TestRestore_MissingBackup(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	tx := corruptBackup(t, root)
	if err := os.Remove(filepath.Join(tx.dir, tx.j.Files[0].BackupPath)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Discard(errors.New("boom")); !errors.Is(err, ErrPending) {
		t.Fatalf("Discard = %v, want ErrPending", err)
	}
}

func TestRestore_ParentReplacedByFile(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "f"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	tx := beginTx(t)
	if err := tx.Stage(root, "sub/f", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Apply(noopVerify); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tx.Discard(errors.New("boom")); !errors.Is(err, ErrPending) {
		t.Fatalf("Discard = %v, want ErrPending when restore cannot write", err)
	}
}

func TestRecover_TerminalAndMissingJournals(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	tx := beginTx(t)
	if err := tx.Stage(root, "f", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Apply(noopVerify); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	j, err := Recover(context.Background(), tx.ID())
	if err != nil || j.State != StateCommitted {
		t.Fatalf("Recover committed = %v, %v; want unchanged committed", j.State, err)
	}
	if got := mustRead(t, filepath.Join(root, "f")); string(got) != "new" {
		t.Fatalf("Recover touched a committed destination: %q", got)
	}

	if _, err := Recover(context.Background(), "1-0000000000000000"); err == nil {
		t.Fatal("Recover of an unknown id should fail")
	}
}

func TestRecover_WaitsOnLiveHolder(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	id := leavePending(t, root, "f", testKey)

	held, err := AcquireAll(context.Background(), []string{testKey})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*pollInterval)
	defer cancel()
	if _, err := Recover(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Recover = %v, want DeadlineExceeded while lock is held", err)
	}
	j, _ := Load(id)
	if j.State != StatePrepared {
		t.Fatalf("state = %q, Recover must not act without the lock", j.State)
	}
}

func TestRecoverAll_RecoversEveryPendingJournal(t *testing.T) {
	isolate(t)
	root := t.TempDir()

	empty, err := RecoverAll(context.Background())
	if err != nil || len(empty) != 0 {
		t.Fatalf("RecoverAll on empty store = %v, %v", empty, err)
	}

	first := leavePending(t, root, "a", "k1")
	second := leavePending(t, root, "b", "k2")

	tx := beginTx(t, "k3")
	if err := tx.Persist(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	got, err := RecoverAll(context.Background())
	if err != nil {
		t.Fatalf("RecoverAll: %v", err)
	}
	ids := map[string]State{}
	for _, j := range got {
		ids[j.ID] = j.State
	}
	if len(ids) != 2 || ids[first] != StateRestored || ids[second] != StateRestored {
		t.Fatalf("RecoverAll = %v, want both pending journals restored and the committed one skipped", ids)
	}
	if pending, _ := Pending([]string{"k1", "k2", "k3"}); len(pending) != 0 {
		t.Fatalf("still pending after RecoverAll: %+v", pending)
	}
}

func TestRecoverAll_ReportsFailuresAndContinues(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	bad := corruptBackup(t, root)
	_ = bad.Discard(errors.New("boom"))
	good := leavePending(t, t.TempDir(), "g", "other")

	got, err := RecoverAll(context.Background())
	if err == nil || !strings.Contains(err.Error(), bad.ID()) {
		t.Fatalf("RecoverAll err = %v, want failure naming %s", err, bad.ID())
	}
	if len(got) != 1 || got[0].ID != good || got[0].State != StateRestored {
		t.Fatalf("RecoverAll = %+v, want only %s restored", got, good)
	}
}

func TestAtomicWrite_MissingDirectory(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "missing", "f")
	if err := atomicWrite(dest, []byte("x"), 0o644); err == nil {
		t.Fatal("atomicWrite into a missing directory should fail")
	}
}

func TestAtomicWrite_DestinationIsDirectory(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "d")
	if err := os.MkdirAll(filepath.Join(dest, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(dest, []byte("x"), 0o644); err == nil {
		t.Fatal("atomicWrite over a non-empty directory should fail")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".relay-tmp-") {
			t.Fatalf("temp file %s left behind", e.Name())
		}
	}
}

func TestAtomicWrite_AppliesMode(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "f")
	if err := atomicWrite(dest, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}
