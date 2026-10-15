package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport/txn"
)

// committedJournal records a finished write of name into root.
func committedJournal(t *testing.T, root, name string) string {
	t.Helper()
	tx, err := txn.Begin(context.Background(), "write "+name, []string{"test:" + name})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Stage(root, name, []byte("content"), 0o644); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := tx.Apply(func(string, string) error { return nil }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := tx.Commit(func() error { return nil }); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return tx.ID()
}

// interruptedJournal leaves a prepared journal on disk, as if the process
// died right after Persist, and returns its id.
func interruptedJournal(t *testing.T, root, name string) string {
	t.Helper()
	tx, err := txn.Begin(context.Background(), "write "+name, []string{"test:" + name})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Stage(root, name, []byte("content"), 0o644); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	dir, err := txn.Dir(tx.ID())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := os.ReadFile(dir + ".json")
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Discard(errors.New("simulated crash"))
	if err := os.WriteFile(dir+".json", prepared, 0o600); err != nil {
		t.Fatal(err)
	}
	return tx.ID()
}

func TestHistory_EmptyStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out, err := runCommand(t, HistoryCmd())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if strings.TrimSpace(out) != "no recorded transactions" {
		t.Errorf("unexpected output %q", out)
	}
}

func TestHistory_ListsJournalsAndPrunesFinishedOnes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	done := committedJournal(t, root, "a.md")
	pending := interruptedJournal(t, root, "b.md")

	out, err := runCommand(t, HistoryCmd())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if !strings.HasPrefix(out, "ID") || !strings.Contains(out, "STATE") {
		t.Errorf("missing table header:\n%s", out)
	}
	for _, want := range []string{done, "committed", "write a.md", pending, "prepared", "write b.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("history output missing %q:\n%s", want, out)
		}
	}

	if _, err := runCommand(t, HistoryCmd(), "prune", pending); err == nil || !strings.Contains(err.Error(), "refusing to prune") {
		t.Errorf("pruning a pending journal must be refused, got %v", err)
	}

	out, err = runCommand(t, HistoryCmd(), "prune", done)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if strings.TrimSpace(out) != "pruned "+done {
		t.Errorf("unexpected prune output %q", out)
	}
	if _, err := txn.Load(done); err == nil {
		t.Error("pruned journal is still loadable")
	}
	if got, _ := os.ReadFile(filepath.Join(root, "a.md")); string(got) != "content" {
		t.Error("pruning must not touch the committed destination")
	}
}

func TestHistoryPrune_RejectsInvalidIDAndArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := runCommand(t, HistoryCmd(), "prune", "../escape"); !errors.Is(err, txn.ErrInvalidID) {
		t.Errorf("expected ErrInvalidID, got %v", err)
	}
	if _, err := runCommand(t, HistoryCmd(), "prune"); err == nil {
		t.Error("expected an argument-count error")
	}
}

func TestRecover_NothingPending(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	committedJournal(t, t.TempDir(), "a.md")

	out, err := runCommand(t, RecoverCmd())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if strings.TrimSpace(out) != "no pending transactions" {
		t.Errorf("unexpected output %q", out)
	}
}

func TestRecover_AllRestoresEveryPendingJournal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	first := interruptedJournal(t, root, "a.md")
	second := interruptedJournal(t, root, "b.md")

	out, err := runCommand(t, RecoverCmd())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	for _, id := range []string{first, second} {
		if !strings.Contains(out, id+": restored (write ") {
			t.Errorf("output missing restored line for %s:\n%s", id, out)
		}
		j, err := txn.Load(id)
		if err != nil || j.State != txn.StateRestored {
			t.Errorf("journal %s state = %v (err %v), want restored", id, j, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "a.md")); !os.IsNotExist(err) {
		t.Error("recovery must not create the never-applied destination")
	}
}

func TestRecover_SingleJournalByID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	target := interruptedJournal(t, root, "a.md")
	other := interruptedJournal(t, root, "b.md")

	out, err := runCommand(t, RecoverCmd(), target)
	if err != nil {
		t.Fatalf("recover %s: %v", target, err)
	}
	if strings.TrimSpace(out) != target+": restored (write a.md)" {
		t.Errorf("unexpected output %q", out)
	}
	if j, _ := txn.Load(other); j == nil || j.State != txn.StatePrepared {
		t.Errorf("recovering one id must leave the other pending, got %+v", j)
	}
}

func TestRecover_RejectsInvalidIDAndExtraArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := runCommand(t, RecoverCmd(), "../../etc"); !errors.Is(err, txn.ErrInvalidID) {
		t.Errorf("expected ErrInvalidID, got %v", err)
	}
	if _, err := runCommand(t, RecoverCmd(), "a", "b"); err == nil {
		t.Error("expected an argument-count error")
	}
}
