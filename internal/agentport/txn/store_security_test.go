package txn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestValidateID_RejectsTraversalAndUnexpectedShapes locks the id format to
// exactly what newID() produces — anything else (path traversal, absolute
// paths, empty, or merely malformed) must be rejected before it reaches a
// filesystem path.
func TestValidateID_RejectsTraversalAndUnexpectedShapes(t *testing.T) {
	bad := []string{
		"../../../../etc/passwd",
		"../escape",
		"/etc/passwd",
		"foo/../../bar",
		"",
		"12345",                  // missing hex suffix
		"12345-zzzzzzzzzzzzzzzz", // suffix not hex
		"12345-abcd",             // hex suffix too short
	}
	for _, id := range bad {
		if err := validateID(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("validateID(%q) = %v, want ErrInvalidID", id, err)
		}
	}
}

func TestValidateID_AcceptsNewIDFormat(t *testing.T) {
	id, err := newID()
	if err != nil {
		t.Fatalf("newID: %v", err)
	}
	if err := validateID(id); err != nil {
		t.Fatalf("validateID(%q) = %v, want nil", id, err)
	}
}

// TestLoad_RejectsTraversalID proves a crafted id can never make Load read
// outside the transactions directory.
func TestLoad_RejectsTraversalID(t *testing.T) {
	isolate(t)
	if _, err := Load("../../../../etc/passwd"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("Load traversal id = %v, want ErrInvalidID", err)
	}
}

// TestDir_RejectsTraversalID mirrors TestLoad_RejectsTraversalID for Dir.
func TestDir_RejectsTraversalID(t *testing.T) {
	isolate(t)
	if _, err := Dir("../../../../etc"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("Dir traversal id = %v, want ErrInvalidID", err)
	}
}

// TestPrune_RejectsTraversalID proves a crafted id can never make Prune's
// os.RemoveAll delete something outside the transactions directory, even
// with a canary file planted exactly where the traversal would land if
// validation were skipped.
func TestPrune_RejectsTraversalID(t *testing.T) {
	isolate(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	// base is <home>/.config/relay/transactions (3 components below home);
	// "../../../canary" walks all three back off, landing on <home>/canary
	// — exactly where an unvalidated Join(base, id) would resolve.
	canary := filepath.Join(home, "canary")
	if err := os.MkdirAll(canary, 0o700); err != nil {
		t.Fatalf("mkdir canary: %v", err)
	}
	canaryFile := filepath.Join(canary, "important.txt")
	if err := os.WriteFile(canaryFile, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("seed canary file: %v", err)
	}

	if err := Prune("../../../canary"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("Prune traversal id = %v, want ErrInvalidID", err)
	}

	if _, err := os.Stat(canaryFile); err != nil {
		t.Fatalf("canary file should survive a rejected traversal id: %v", err)
	}
}

// TestRecover_RejectsTraversalID proves the same for Recover — the more
// dangerous primitive, since compensate can write attacker-controlled
// backup bytes, not just delete.
func TestRecover_RejectsTraversalID(t *testing.T) {
	isolate(t)
	if _, err := Recover(context.Background(), "../../../../etc/passwd"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("Recover traversal id = %v, want ErrInvalidID", err)
	}
}
