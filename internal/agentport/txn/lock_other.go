//go:build !unix && !windows

package txn

import "os"

// Platforms without a wired-up advisory lock get no cross-process
// exclusion: tryLock always succeeds, so relay still works single-process.
func tryLock(f *os.File) error    { return nil }
func unlockFile(f *os.File) error { return nil }
func isWouldBlock(err error) bool { return false }
