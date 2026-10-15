//go:build windows

package txn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTryLock_SecondHandleWouldBlockUntilUnlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.lock")
	open := func() *os.File {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	first, second := open(), open()

	if err := tryLock(first); err != nil {
		t.Fatalf("first tryLock: %v", err)
	}
	err := tryLock(second)
	if err == nil {
		t.Fatal("second handle acquired a held lock")
	}
	if !isWouldBlock(err) {
		t.Fatalf("contention error %v not recognised as would-block", err)
	}
	if err := unlockFile(first); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := tryLock(second); err != nil {
		t.Fatalf("tryLock after unlock: %v", err)
	}
	_ = unlockFile(second)
}
