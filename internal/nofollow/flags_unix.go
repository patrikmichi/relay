//go:build unix

// Package nofollow holds the open(2) flags relay uses to read a file without
// following a final symlink or blocking on a FIFO.
package nofollow

import (
	"os"
	"syscall"
)

// ReadFlags opens read-only, refusing a symlink at the final path element
// and never blocking on a FIFO.
const ReadFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
