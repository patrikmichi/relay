package txn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStageFromRejectsChangedDestination(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	dest := filepath.Join(root, "a.json")
	if err := os.WriteFile(dest, []byte("planned"), 0o644); err != nil {
		t.Fatal(err)
	}

	tx, err := Begin(context.Background(), "test", []string{"conflict:a"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Discard(nil) }()

	if err := os.WriteFile(dest, []byte("edited meanwhile"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = tx.StageFrom(root, "a.json", []byte("new"), 0o644, Before{Data: []byte("planned"), Existed: true})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("StageFrom = %v, want ErrConflict", err)
	}
	if err := tx.StageFrom(root, "a.json", []byte("new"), 0o644, Before{Data: []byte("edited meanwhile"), Existed: true}); err != nil {
		t.Fatalf("unchanged destination rejected: %v", err)
	}
}

func TestStageFromRejectsDestinationCreatedAfterPlanning(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "new.json"), []byte("appeared"), 0o644); err != nil {
		t.Fatal(err)
	}
	tx, err := Begin(context.Background(), "test", []string{"conflict:new"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Discard(nil) }()
	if err := tx.StageFrom(root, "new.json", []byte("x"), 0o644, Before{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("StageFrom = %v, want ErrConflict", err)
	}
	if err := tx.StageRemovalFrom(root, "new.json", Before{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("StageRemovalFrom = %v, want ErrConflict", err)
	}
}

func commitOne(t *testing.T, root, name string) string {
	t.Helper()
	tx, err := Begin(context.Background(), "test", []string{"prune:" + name})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Stage(root, name, []byte(name), 0o644); err != nil {
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
	return tx.ID()
}

func TestPruneFinishedKeepsNewestAndPending(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	var ids []string
	for _, n := range []string{"one", "two", "three"} {
		ids = append(ids, commitOne(t, root, n))
	}

	pending, err := Begin(context.Background(), "pending", []string{"prune:pending"})
	if err != nil {
		t.Fatal(err)
	}
	if err := pending.Stage(root, "p", []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pending.Persist(); err != nil {
		t.Fatal(err)
	}

	n, err := PruneFinished(Retention{MaxAge: time.Hour, Keep: 1}, time.Now())
	if err != nil || n != 2 {
		t.Fatalf("PruneFinished = %d, %v; want 2 removed", n, err)
	}
	if _, err := Load(ids[2]); err != nil {
		t.Fatalf("newest journal was pruned: %v", err)
	}
	if _, err := Load(ids[0]); err == nil {
		t.Fatal("oldest journal survived")
	}
	if _, err := Load(pending.ID()); err != nil {
		t.Fatalf("pending journal was pruned: %v", err)
	}

	n, err = PruneFinished(Retention{MaxAge: time.Hour, Keep: 10}, time.Now().Add(48*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("age prune = %d, %v; want 1 removed", n, err)
	}
	if _, err := Load(pending.ID()); err != nil {
		t.Fatalf("pending journal was pruned by age: %v", err)
	}
}

func TestBackupsAreOwnerOnly(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	dest := filepath.Join(root, "secret.json")
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	tx, err := Begin(context.Background(), "test", []string{"mode:secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Discard(nil) }()
	if err := tx.Stage(root, "secret.json", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, _ := Dir(tx.ID())
	for _, sub := range []string{"backup", "staged"} {
		info, err := os.Stat(filepath.Join(dir, sub, "0000"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", sub, info.Mode().Perm())
		}
	}
}
