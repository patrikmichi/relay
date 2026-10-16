//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// isInteractiveTerminal reports whether f is a console, via GetConsoleMode,
// which fails for pipes, files and NUL.
func isInteractiveTerminal(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}
