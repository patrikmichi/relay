//go:build !darwin && !linux

package cli

import "os"

// isInteractiveTerminal always reports false on platforms without a
// dedicated termios probe (Windows, other BSDs) — the safe default is to
// never prompt and behave as non-interactive (matching --strict/dry-run
// semantics rather than risking a hang waiting on stdin).
func isInteractiveTerminal(f *os.File) bool {
	return false
}
