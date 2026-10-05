package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Prompts must never block on stdin that is not a terminal; each platform's
// probe has to reject the null device, pipes and regular files.
func TestIsInteractiveTerminal_RejectsNonConsoleHandles(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = null.Close() })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	file, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })

	for name, f := range map[string]*os.File{"null device": null, "pipe": r, "regular file": file} {
		if isInteractiveTerminal(f) {
			t.Errorf("%s reported as an interactive terminal", name)
		}
	}
}
