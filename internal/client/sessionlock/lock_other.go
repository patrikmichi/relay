//go:build !unix

package sessionlock

import "os"

func tryLock(f *os.File) error    { return nil }
func unlockFile(f *os.File) error { return nil }
func isWouldBlock(err error) bool { return false }
