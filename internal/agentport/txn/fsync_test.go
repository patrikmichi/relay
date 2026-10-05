package txn

import "testing"

// TestSave_FsyncsBeforeRename is a smoke test that save() still produces a
// readable, correct journal — the crash-safety half of fsync-before-rename
// can't be exercised without killing the process, but this guards against
// a regression that breaks the write path itself while adding the sync
// call (e.g. syncing the wrong file handle).
func TestSave_FsyncsBeforeRename(t *testing.T) {
	isolate(t)
	j := &Journal{Schema: schemaVersion, ID: "1-aaaaaaaaaaaaaaaa", State: StatePrepared, Op: "test"}
	if err := j.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(j.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != StatePrepared || got.Op != "test" {
		t.Fatalf("loaded journal = %+v, want matching State/Op", got)
	}
}
