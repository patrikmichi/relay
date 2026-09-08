//go:build !unix

package txn

import "os"

// relay's supported release matrix is Linux/macOS (see the remediation
// plan's deferred backlog — Windows is explicitly out of scope). On any
// other platform there is no wired-up native advisory-lock primitive:
// tryLock always succeeds, so the CLI still functions single-process, but
// this provides NO cross-process exclusion here — mirrors
// internal/client/sessionlock's identical fallback.
func tryLock(f *os.File) error    { return nil }
func unlockFile(f *os.File) error { return nil }
func isWouldBlock(err error) bool { return false }
