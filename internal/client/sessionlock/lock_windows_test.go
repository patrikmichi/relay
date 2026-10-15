//go:build windows

package sessionlock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestTryLock_SecondHandleIsRefusedUntilUnlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pair.lock")
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
		t.Fatalf("contention error %v not classified as would-block", err)
	}
	if err := unlockFile(first); err != nil {
		t.Fatalf("unlockFile: %v", err)
	}
	if err := tryLock(second); err != nil {
		t.Fatalf("tryLock after release: %v", err)
	}
	if err := unlockFile(second); err != nil {
		t.Fatalf("unlockFile: %v", err)
	}
}

func TestIsWouldBlock_OnlyLockViolation(t *testing.T) {
	if !isWouldBlock(windows.ERROR_LOCK_VIOLATION) {
		t.Error("ERROR_LOCK_VIOLATION must be retried")
	}
	for _, err := range []error{windows.ERROR_ACCESS_DENIED, windows.ERROR_INVALID_HANDLE, errors.New("other")} {
		if isWouldBlock(err) {
			t.Errorf("%v must not be retried", err)
		}
	}
}
