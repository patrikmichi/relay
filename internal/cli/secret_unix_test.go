//go:build linux || darwin

package cli

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestRestoreOnInterruptRestoresTheTerminalBeforeExiting(t *testing.T) {
	restored := make(chan struct{})
	exited := make(chan int, 1)
	stop := restoreOnInterrupt(func() { close(restored) }, func(code int) {
		select {
		case <-restored:
			exited <- code
		default:
			exited <- -1
		}
	})
	defer stop()

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exited:
		if code != 130 {
			t.Fatalf("exit code = %d, want 130 after restoring echo", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt was not handled")
	}
}

func TestRestoreOnInterruptStopsListening(t *testing.T) {
	called := make(chan struct{}, 1)
	stop := restoreOnInterrupt(func() { called <- struct{}{} }, func(int) {})
	stop()
	// Another handler keeps the test process alive for the signal.
	hold := make(chan os.Signal, 1)
	signal.Notify(hold, os.Interrupt)
	defer signal.Stop(hold)
	_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	select {
	case <-hold:
	case <-time.After(5 * time.Second):
		t.Fatal("signal not delivered")
	}
	select {
	case <-called:
		t.Fatal("a stopped guard still restored the terminal")
	case <-time.After(100 * time.Millisecond):
	}
}
