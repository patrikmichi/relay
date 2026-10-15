package sessionlock

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAcquire_MutualExclusion exercises the real flock-backed file lock (not
// a mock) across goroutines that each open their own file descriptor on the
// same lock path — the kernel arbitrates this identically to two separate
// `relay` processes. A counter incremented without a lock and observed by a
// concurrent holder would show more than one holder active at once.
func TestAcquire_MutualExclusion(t *testing.T) {
	ctx := context.Background()
	const gateway, account = "https://gw.example.invalid", "user@example.invalid"

	var active int32
	var maxActive int32
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lock, err := Acquire(ctx, gateway, account)
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			n := atomic.AddInt32(&active, 1)
			for {
				m := atomic.LoadInt32(&maxActive)
				if n <= m || atomic.CompareAndSwapInt32(&maxActive, m, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			if err := lock.Unlock(); err != nil {
				t.Errorf("Unlock: %v", err)
			}
		}()
	}
	wg.Wait()

	if maxActive != 1 {
		t.Fatalf("mutual exclusion violated: %d holders observed active simultaneously", maxActive)
	}
}

// TestAcquire_RespectsContextDeadline proves Acquire does not block forever
// on an already-held lock — a caller with a bounded context gets ctx.Err()
// back instead of hanging past its deadline.
func TestAcquire_RespectsContextDeadline(t *testing.T) {
	const gateway, account = "https://gw2.example.invalid", "user@example.invalid"

	holder, err := Acquire(context.Background(), gateway, account)
	if err != nil {
		t.Fatalf("Acquire (holder): %v", err)
	}
	defer func() { _ = holder.Unlock() }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = Acquire(ctx, gateway, account)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected Acquire to fail once ctx deadline passed, got nil error")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Acquire took %s to give up after a 50ms deadline", elapsed)
	}
}

// TestAcquire_ReleasedOnUnlock proves the lock becomes available again
// immediately after Unlock — real kernel-enforced liveness, not a
// time-based staleness guess.
func TestAcquire_ReleasedOnUnlock(t *testing.T) {
	ctx := context.Background()
	const gateway, account = "https://gw3.example.invalid", "user@example.invalid"

	first, err := Acquire(ctx, gateway, account)
	if err != nil {
		t.Fatalf("Acquire (first): %v", err)
	}
	if err := first.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	done := make(chan struct{})
	go func() {
		second, err := Acquire(ctx, gateway, account)
		if err != nil {
			t.Errorf("Acquire (second): %v", err)
			return
		}
		_ = second.Unlock()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire did not complete after the first Unlock — lock not actually released")
	}
}
