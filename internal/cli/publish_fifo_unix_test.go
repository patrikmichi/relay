//go:build unix

package cli

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestBuildBundle_FIFO_Skipped verifies that a FIFO (named pipe) in the bundle directory
// is silently skipped and does NOT cause the bundler to block or error.
// Non-regular, non-symlink entries must be filtered before os.ReadFile is
// called.
func TestBuildBundle_FIFO_Skipped(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "SKILL.md")

	// Create a FIFO in the bundle directory.
	fifoPath := filepath.Join(dir, "test.fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("Mkfifo not available: %v", err)
	}

	// buildBundle must complete without hanging and without including the FIFO.
	_, bundleBytes, files, err := buildBundle(dir)
	if err != nil {
		t.Fatalf("buildBundle with FIFO present: unexpected error: %v", err)
	}
	if len(bundleBytes) == 0 {
		t.Error("expected non-empty bundle bytes")
	}

	// The FIFO must NOT appear in the file list.
	for _, f := range files {
		if strings.HasSuffix(f, ".fifo") {
			t.Errorf("FIFO must be skipped, but found in bundle files: %s", f)
		}
	}
}
