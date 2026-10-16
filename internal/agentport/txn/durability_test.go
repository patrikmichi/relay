package txn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

var errCrash = errors.New("injected crash")

func TestCommitCrashIsFinishedForwardNeverBackward(t *testing.T) {
	for _, stage := range []string{"committing", "applied"} {
		t.Run(stage, func(t *testing.T) {
			isolate(t)
			var applied []string
			RegisterFinalizer("test-ledger", func(raw json.RawMessage) error {
				var v string
				if err := json.Unmarshal(raw, &v); err != nil {
					return err
				}
				if !slices.Contains(applied, v) { // idempotent, like a real ledger update
					applied = append(applied, v)
				}
				return nil
			})
			root := t.TempDir()
			tx, err := Begin(context.Background(), "write", []string{"k:commit-" + stage})
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Stage(root, "f.txt", []byte("new"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := tx.Persist(); err != nil {
				t.Fatal(err)
			}
			if err := tx.Apply(noopVerify); err != nil {
				t.Fatal(err)
			}

			restore := SetCommitHook(func(s string) error {
				if s == stage {
					return errCrash
				}
				return nil
			})
			err = tx.CommitWith("test-ledger", "entry-1", func() error {
				applied = append(applied, "entry-1")
				return nil
			})
			restore()
			if !errors.Is(err, errCrash) {
				t.Fatalf("CommitWith = %v, want the injected crash", err)
			}
			if j, _ := Load(tx.ID()); j.State != StateCommitting {
				t.Fatalf("journal state = %q, want committing", j.State)
			}
			if got := mustRead(t, filepath.Join(root, "f.txt")); string(got) != "new" {
				t.Fatalf("a crash during commit changed the file: %q", got)
			}
			if _, err := Begin(context.Background(), "next", []string{"k:commit-" + stage}); !errors.Is(err, ErrPending) {
				t.Fatalf("a committing journal must block a new run, got %v", err)
			}

			j, err := Recover(context.Background(), tx.ID())
			if err != nil || j.State != StateCommitted {
				t.Fatalf("Recover = %+v, %v; want committed", j, err)
			}
			if got := mustRead(t, filepath.Join(root, "f.txt")); string(got) != "new" {
				t.Errorf("recovery rolled the file back: %q", got)
			}
			if len(applied) != 1 || applied[0] != "entry-1" {
				t.Errorf("ledger updates = %v, want exactly one entry-1", applied)
			}
		})
	}
}

func TestRecoverKeepsACommittingJournalWhoseFinalizerIsMissing(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	tx, err := Begin(context.Background(), "write", []string{"k:nofinalizer"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Stage(root, "f.txt", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tx.Persist(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Apply(noopVerify); err != nil {
		t.Fatal(err)
	}
	restore := SetCommitHook(func(string) error { return errCrash })
	_ = tx.CommitWith("unknown-kind", "x", func() error { return nil })
	restore()

	if _, err := Recover(context.Background(), tx.ID()); err == nil {
		t.Fatal("recovery without a finalizer must not claim success")
	}
	if got := mustRead(t, filepath.Join(root, "f.txt")); string(got) != "new" {
		t.Errorf("the file was rolled back although the ledger may have recorded it: %q", got)
	}
	if j, _ := Load(tx.ID()); j.State != StateCommitting {
		t.Errorf("state = %q, want committing so a build that has the finalizer can finish it", j.State)
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
