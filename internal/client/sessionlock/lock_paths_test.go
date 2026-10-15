package sessionlock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func isolatedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("TMP", dir)
	} else {
		t.Setenv("TMPDIR", dir)
	}
	return dir
}

func TestLockPath_StablePerPairAndPrivate(t *testing.T) {
	dir := isolatedTempDir(t)

	a1, err := lockPath("https://gw.example.invalid", "a@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := lockPath("https://gw.example.invalid", "a@example.invalid")
	b, _ := lockPath("https://gw.example.invalid", "b@example.invalid")
	c, _ := lockPath("https://other.example.invalid", "a@example.invalid")

	if a1 != a2 {
		t.Errorf("same pair produced different paths: %s vs %s", a1, a2)
	}
	if a1 == b || a1 == c {
		t.Error("distinct pairs share a lock file")
	}
	if filepath.Dir(a1) != filepath.Join(dir, "relay-cli-locks") {
		t.Errorf("lock file %s outside the temp lock dir", a1)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Dir(a1))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("lock dir mode %o, want 700", perm)
		}
	}
}

func TestAcquire_UncreatableLockDir(t *testing.T) {
	dir := isolatedTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "relay-cli-locks"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(context.Background(), "https://gw.example.invalid", "a@example.invalid"); err == nil {
		t.Fatal("expected Acquire to fail when the lock dir cannot be created")
	}
}

func TestAcquire_UnopenableLockFile(t *testing.T) {
	isolatedTempDir(t)
	path, err := lockPath("https://gw.example.invalid", "a@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(context.Background(), "https://gw.example.invalid", "a@example.invalid"); err == nil {
		t.Fatal("expected Acquire to fail when the lock path is a directory")
	}
}

func TestAcquire_CancelledWhileHeldReturnsContextError(t *testing.T) {
	isolatedTempDir(t)
	holder, err := Acquire(context.Background(), "https://gw.example.invalid", "a@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Unlock() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Acquire(ctx, "https://gw.example.invalid", "a@example.invalid"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestUnlock_TwiceReportsError(t *testing.T) {
	isolatedTempDir(t)
	l, err := Acquire(context.Background(), "https://gw.example.invalid", "a@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatalf("first Unlock: %v", err)
	}
	if err := l.Unlock(); err == nil {
		t.Fatal("expected a second Unlock to report the closed file")
	}
}
