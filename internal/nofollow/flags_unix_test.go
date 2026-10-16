//go:build unix

package nofollow

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadFlagsRefuseSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if f, err := os.OpenFile(link, ReadFlags, 0); err == nil {
		_ = f.Close()
		t.Fatal("opening a symlink with ReadFlags must fail")
	}
	f, err := os.OpenFile(target, ReadFlags, 0)
	if err != nil {
		t.Fatalf("regular file: %v", err)
	}
	_ = f.Close()
}

func TestReadFlagsDoNotBlockOnFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(fifo, ReadFlags, 0)
		if err == nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO with ReadFlags blocked")
	}
}
