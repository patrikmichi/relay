package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	lockPoll    = 20 * time.Millisecond
	lockTimeout = 10 * time.Second
)

// lockConfig takes an advisory lock that serializes read-modify-write cycles
// of the config file across relay processes. The kernel drops it when the
// holder exits, so a crash never leaves it stuck.
func lockConfig(dir string) (unlock func(), err error) {
	path := filepath.Join(dir, "config.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open config lock: %w", err)
	}
	deadline := time.Now().Add(lockTimeout)
	for {
		err := tryLock(f)
		if err == nil {
			return func() { _ = unlockFile(f); _ = f.Close() }, nil
		}
		if !isWouldBlock(err) || time.Now().After(deadline) {
			_ = f.Close()
			if isWouldBlock(err) {
				return nil, fmt.Errorf("config is locked by another relay process (waited %s)", lockTimeout)
			}
			return nil, fmt.Errorf("lock config: %w", err)
		}
		time.Sleep(lockPoll)
	}
}
