package txn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// pollInterval mirrors internal/client/sessionlock's flock retry cadence —
// same kernel-liveness reasoning applies here: a held lock always means a
// live holder, never a guess based on file age.
const pollInterval = 20 * time.Millisecond

// LockSet holds every per-key advisory lock acquired for one operation.
// Unlock releases them in reverse acquisition order.
type LockSet struct {
	files []*os.File
}

// AcquireAll sorts keys (deterministic order — the same operation
// requested from any process acquires its locks in the same sequence, so
// two overlapping multi-target operations can never deadlock each other)
// and blocks until every one is held or ctx is done. On any failure it
// releases whatever it already acquired before returning.
func AcquireAll(ctx context.Context, keys []string) (*LockSet, error) {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)

	ls := &LockSet{}
	for _, k := range sorted {
		f, err := acquireOne(ctx, k)
		if err != nil {
			ls.Unlock()
			return nil, err
		}
		ls.files = append(ls.files, f)
	}
	return ls, nil
}

// Unlock releases every held lock, reverse of acquisition order. Safe to
// call on a partially-populated LockSet (used on the AcquireAll failure
// path) or to call twice.
func (ls *LockSet) Unlock() {
	for i := len(ls.files) - 1; i >= 0; i-- {
		f := ls.files[i]
		_ = unlockFile(f)
		_ = f.Close()
	}
	ls.files = nil
}

func acquireOne(ctx context.Context, key string) (*os.File, error) {
	path, err := lockPath(key)
	if err != nil {
		return nil, fmt.Errorf("transaction lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("transaction lock: open %s: %w", path, err)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		err := tryLock(f)
		if err == nil {
			return f, nil
		}
		if !isWouldBlock(err) {
			_ = f.Close()
			return nil, fmt.Errorf("transaction lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func lockPath(key string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "relay", "transaction-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".lock"), nil
}
