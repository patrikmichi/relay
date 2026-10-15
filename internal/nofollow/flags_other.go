//go:build !unix

// Package nofollow holds the open(2) flags relay uses to read a file without
// following a final symlink or blocking on a FIFO.
package nofollow

import "os"

// ReadFlags is plain read-only where the OS has no O_NOFOLLOW. Callers
// Lstat the path first and check the opened file is regular.
const ReadFlags = os.O_RDONLY
