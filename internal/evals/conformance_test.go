package evals

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vectors are the contract with the gateway's implementation; a hand edit
// that is not a fresh copy from a gateway release fails here.
func TestConformanceVectorsMatchTheirRecordedHashes(t *testing.T) {
	dir := filepath.Join("testdata", "conformance")
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		want, name, ok := strings.Cut(line, "  ")
		if !ok {
			t.Fatalf("malformed line %q", line)
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		got := sha256.Sum256(raw)
		if hex.EncodeToString(got[:]) != want {
			t.Errorf("%s changed since its hash was recorded; copy fresh vectors with `make gen-evals-fixtures`", name)
		}
		listed++
	}
	if listed != 2 {
		t.Errorf("%d vector files are hashed, want 2", listed)
	}
}

func TestScaffoldRejectsUnknownKinds(t *testing.T) {
	if _, err := Scaffold("toaster"); err == nil {
		t.Error("an unknown kind")
	}
}
