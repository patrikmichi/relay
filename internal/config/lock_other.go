//go:build !unix && !windows

package config

import "os"

// Platforms without a wired-up advisory lock get no cross-process exclusion.
func tryLock(f *os.File) error    { return nil }
func unlockFile(f *os.File) error { return nil }
func isWouldBlock(err error) bool { return false }
