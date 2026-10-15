package txn

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func transactionsDir(t *testing.T) string {
	t.Helper()
	base, err := baseDir()
	if err != nil {
		t.Fatal(err)
	}
	return base
}

func TestList_SortsAndKeepsUnreadableJournalsVisible(t *testing.T) {
	isolate(t)
	base := transactionsDir(t)

	older := &Journal{Schema: schemaVersion, ID: "2-bbbbbbbbbbbbbbbb", Op: "older", State: StateCommitted, CreatedAt: time.Unix(100, 0)}
	newer := &Journal{Schema: schemaVersion, ID: "1-aaaaaaaaaaaaaaaa", Op: "newer", State: StatePrepared, CreatedAt: time.Unix(200, 0)}
	for _, j := range []*Journal{newer, older} {
		if err := j.save(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "3-cccccccccccccccc.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "dir.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d journals, want 3: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].Op, "(unreadable:") || got[0].ID != "3-cccccccccccccccc" {
		t.Fatalf("first entry = %+v, want the unreadable journal (zero CreatedAt sorts first)", got[0])
	}
	if got[1].Op != "older" || got[2].Op != "newer" {
		t.Fatalf("order = %q, %q; want older then newer", got[1].Op, got[2].Op)
	}
}

func TestList_TransactionsDirUnreadable(t *testing.T) {
	isolate(t)
	base := transactionsDir(t)
	skipWithoutPermissionChecks(t)
	if err := os.Chmod(base, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o700) })
	if _, err := List(); err == nil {
		t.Fatal("List should fail when the transactions dir is unreadable")
	}
	if _, err := Pending([]string{"k"}); err == nil {
		t.Fatal("Pending should propagate the List error")
	}
}

func TestPending_IgnoresUnreadableAndTerminalJournals(t *testing.T) {
	isolate(t)
	base := transactionsDir(t)
	for _, j := range []*Journal{
		{Schema: schemaVersion, ID: "1-aaaaaaaaaaaaaaaa", LockKeys: []string{"k"}, State: StateCommitted},
		{Schema: schemaVersion, ID: "2-aaaaaaaaaaaaaaaa", LockKeys: []string{"k"}, State: StateRestored},
		{Schema: schemaVersion, ID: "3-aaaaaaaaaaaaaaaa", LockKeys: []string{"other"}, State: StateApplying},
		{Schema: schemaVersion, ID: "4-aaaaaaaaaaaaaaaa", LockKeys: []string{"x", "k"}, State: StateRecovery},
	} {
		if err := j.save(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "5-aaaaaaaaaaaaaaaa.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Pending([]string{"k"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "4-aaaaaaaaaaaaaaaa" {
		t.Fatalf("Pending = %+v, want only the recovery journal sharing key k", got)
	}
}

func TestLoad_MissingAndCorrupt(t *testing.T) {
	isolate(t)
	if _, err := Load("1-aaaaaaaaaaaaaaaa"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load missing = %v, want ErrNotExist", err)
	}
	if err := os.WriteFile(filepath.Join(transactionsDir(t), "1-aaaaaaaaaaaaaaaa.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("1-aaaaaaaaaaaaaaaa"); err == nil || !strings.Contains(err.Error(), "parse journal") {
		t.Fatalf("Load corrupt = %v, want parse error", err)
	}
	if err := Prune("1-aaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("Prune should refuse a journal it cannot parse")
	}
}

func TestPrune_RemovesJournalAndTransactionData(t *testing.T) {
	isolate(t)
	tx := beginTx(t)
	if err := tx.Persist(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tx.dir); err != nil {
		t.Fatalf("transaction dir missing before prune: %v", err)
	}
	if err := Prune(tx.ID()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(tx.dir); !os.IsNotExist(err) {
		t.Fatalf("transaction dir survived prune: %v", err)
	}
	if _, err := Load(tx.ID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal survived prune: %v", err)
	}
}

func TestSave_UnwritableDirectory(t *testing.T) {
	isolate(t)
	base := transactionsDir(t)
	skipWithoutPermissionChecks(t)
	if err := os.Chmod(base, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o700) })
	j := &Journal{Schema: schemaVersion, ID: "1-aaaaaaaaaaaaaaaa"}
	if err := j.save(); err == nil {
		t.Fatal("save should fail when the transactions dir is read-only")
	}
}

func TestSave_RenameOntoDirectoryCleansTemp(t *testing.T) {
	isolate(t)
	base := transactionsDir(t)
	id := "1-aaaaaaaaaaaaaaaa"
	if err := os.MkdirAll(filepath.Join(base, id+".json", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	j := &Journal{Schema: schemaVersion, ID: id}
	if err := j.save(); err == nil || !strings.Contains(err.Error(), "rename journal") {
		t.Fatalf("save = %v, want rename error", err)
	}
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp journal %s left behind", e.Name())
		}
	}
}

func TestWriteFileDurable_MissingDirectory(t *testing.T) {
	if err := writeFileDurable(filepath.Join(t.TempDir(), "missing", "f"), []byte("x"), 0o600); err == nil {
		t.Fatal("writeFileDurable into a missing directory should fail")
	}
}

func TestBaseDir_RequiresHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("home", "")
	if _, err := baseDir(); err == nil {
		t.Fatal("baseDir should fail without a home directory")
	}
	if _, err := lockPath("k"); err == nil {
		t.Fatal("lockPath should fail without a home directory")
	}
}

func TestVerifyNoSymlinks_ComponentsAndRoot(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	if err := VerifyNoSymlinks(root, "real/new/file"); err != nil {
		t.Fatalf("nonexistent components under a real dir: %v", err)
	}
	if err := VerifyNoSymlinks(root, ""); err != nil {
		t.Fatalf("root itself: %v", err)
	}
	if err := VerifyNoSymlinks(root, "link/file"); !errors.Is(err, ErrDestinationSymlink) {
		t.Fatalf("symlinked component = %v, want ErrDestinationSymlink", err)
	}
	if err := VerifyNoSymlinks(filepath.Join(root, "link"), "file"); !errors.Is(err, ErrDestinationSymlink) {
		t.Fatalf("symlinked root = %v, want ErrDestinationSymlink", err)
	}
	if err := VerifyNoSymlinks(filepath.Join(root, "absent"), "file"); err != nil {
		t.Fatalf("missing root: %v", err)
	}
}

func TestVerifyNoSymlinks_StatErrors(t *testing.T) {
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	skipWithoutPermissionChecks(t)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if err := VerifyNoSymlinks(root, "locked/inner/f"); err == nil || errors.Is(err, ErrDestinationSymlink) {
		t.Fatalf("unreadable component = %v, want a stat error", err)
	}
	if err := VerifyNoSymlinks(filepath.Join(locked, "inner"), "f"); err == nil || errors.Is(err, ErrDestinationSymlink) {
		t.Fatalf("unreadable root = %v, want a stat error", err)
	}
}

// skipWithoutPermissionChecks skips when file modes are not enforced: root
// bypasses them and Windows ignores Unix permission bits.
func skipWithoutPermissionChecks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file permissions are not enforced for this user or OS")
	}
}
