package txn

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecover_RestoresARemovalWhoseAppliedFlagWasNeverSaved(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	dest := filepath.Join(root, "f.txt")
	if err := os.WriteFile(dest, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	tx, err := Begin(context.Background(), "remove", []string{"k:removal"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.StageRemoval(root, "f.txt"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Apply(noopVerify); err != nil {
		t.Fatal(err)
	}
	// The process died after the file was removed and before the journal said so.
	tx.j.Files[0].Applied = false
	if err := tx.j.save(); err != nil {
		t.Fatal(err)
	}
	tx.Abandon()

	if _, err := Recover(context.Background(), tx.ID()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := mustRead(t, dest); string(got) != "original" {
		t.Fatalf("interrupted removal not restored: %q", got)
	}
}

func TestListMovesACorruptJournalAside(t *testing.T) {
	isolate(t)
	base, err := baseDir()
	if err != nil {
		t.Fatal(err)
	}
	id := "1700000000000000000-0123456789abcdef"
	if err := os.WriteFile(filepath.Join(base, id+".json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := List()
	if err != nil || len(first) != 1 || !strings.Contains(first[0].Op, "moved aside") {
		t.Fatalf("first List = %+v, %v", first, err)
	}
	if _, err := os.Stat(filepath.Join(base, id+".json.corrupt")); err != nil {
		t.Errorf("corrupt journal not kept for inspection: %v", err)
	}
	if again, _ := List(); len(again) != 0 {
		t.Errorf("a quarantined journal is still listed: %+v", again)
	}
}

func TestPruneFinishedRemovesOrphanedTransactionData(t *testing.T) {
	isolate(t)
	base, err := baseDir()
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(base, "1700000000000000000-aaaaaaaaaaaaaaaa")
	fresh := filepath.Join(base, "1700000000000000001-bbbbbbbbbbbbbbbb")
	tmp := filepath.Join(base, "journal-123.json.tmp")
	for _, d := range []string{stale, fresh} {
		if err := os.MkdirAll(filepath.Join(d, "backup"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(tmp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	for _, p := range []string{stale, tmp} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := PruneFinished(DefaultRetention, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("an old directory with no journal survived")
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Error("an old temp journal survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a directory that may belong to a running transaction was removed")
	}
}
