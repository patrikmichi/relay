//go:build darwin

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// isInteractiveTerminal reports whether f is attached to an interactive
// terminal, via a TIOCGETA ioctl probe. os.ModeCharDevice alone is
// insufficient — /dev/null is also a character device — so we require the
// termios ioctl to actually succeed.
func isInteractiveTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}
