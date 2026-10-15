// Package sessionlock provides a cross-process advisory file lock scoped to
// one (gateway, account) pair — used by internal/client to serialize OAuth
// refresh-token rotation across independently invoked `relay` processes.
//
// Liveness comes from the kernel's flock(2) semantics, never from
// inspecting a lock file's age or contents: an advisory lock is released
// the instant its holding file descriptor closes — on a clean Unlock, on
// the holding process exiting, or on the holding process crashing — so a
// held lock always means a live holder. There is no staleness heuristic
// and nothing here ever "steals" a lock based on how old it looks.
package sessionlock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// pollInterval is how often Acquire retries a non-blocking lock attempt
// while another holder has it.
const pollInterval = 20 * time.Millisecond

// Lock is a held advisory lock. Call Unlock to release it.
type Lock struct {
	f *os.File
}

// Acquire blocks until the lock for (gatewayOrigin, account) is held or ctx
// is done, whichever happens first. Each call opens its own file
// descriptor, so contention is enforced by the kernel exactly the same way
// whether the two callers are goroutines in this process or two separate
// `relay` invocations on the same machine.
func Acquire(ctx context.Context, gatewayOrigin, account string) (*Lock, error) {
	path, err := lockPath(gatewayOrigin, account)
	if err != nil {
		return nil, fmt.Errorf("session lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("session lock: open %s: %w", path, err)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		err := tryLock(f)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !isWouldBlock(err) {
			_ = f.Close()
			return nil, fmt.Errorf("session lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Unlock releases the lock and closes the underlying file descriptor.
func (l *Lock) Unlock() error {
	unlockErr := unlockFile(l.f)
	closeErr := l.f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// lockPath derives a stable per-(gatewayOrigin, account) lock file path
// under the OS temp directory — content-addressed so it never collides
// with, or needs to parse, another pair's lock file.
func lockPath(gatewayOrigin, account string) (string, error) {
	dir := filepath.Join(os.TempDir(), "relay-cli-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(gatewayOrigin + "\x00" + account))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".lock"), nil
}
