// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// verifies adaptive limiter sleeps are context-cancellable.

package cliutil

import (
	"context"
	"testing"
	"time"
)

func TestAdaptiveLimiterWaitContextCancelsSleep(t *testing.T) {
	limiter := NewAdaptiveLimiter(1)
	limiter.Wait()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		limiter.WaitContext(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("WaitContext did not return promptly after context cancellation")
	}
}

func TestAdaptiveLimiterWaitContextCancellationRefundsSlot(t *testing.T) {
	limiter := NewAdaptiveLimiter(1)
	limiter.Wait()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		limiter.WaitContext(ctx)
		close(done)
	}()

	var cancelledSlot time.Time
	deadline := time.Now().Add(time.Second)
	for cancelledSlot.IsZero() && time.Now().Before(deadline) {
		limiter.mu.Lock()
		if len(limiter.waiters) == 1 {
			cancelledSlot = limiter.waiters[0].at
		}
		limiter.mu.Unlock()
		if cancelledSlot.IsZero() {
			time.Sleep(time.Millisecond)
		}
	}
	if cancelledSlot.IsZero() {
		t.Fatal("cancelled waiter did not reserve a slot")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not return")
	}

	nextCtx, cancelNext := context.WithCancel(context.Background())
	nextDone := make(chan struct{})
	go func() {
		limiter.WaitContext(nextCtx)
		close(nextDone)
	}()

	var nextSlot time.Time
	deadline = time.Now().Add(time.Second)
	for nextSlot.IsZero() && time.Now().Before(deadline) {
		limiter.mu.Lock()
		if len(limiter.waiters) == 1 {
			nextSlot = limiter.waiters[0].at
		}
		limiter.mu.Unlock()
		if nextSlot.IsZero() {
			time.Sleep(time.Millisecond)
		}
	}
	cancelNext()
	select {
	case <-nextDone:
	case <-time.After(time.Second):
		t.Fatal("second waiter did not return")
	}
	if nextSlot.IsZero() {
		t.Fatal("second waiter did not reserve a slot")
	}
	if delta := nextSlot.Sub(cancelledSlot); delta < -10*time.Millisecond || delta > 10*time.Millisecond {
		t.Fatalf("next slot moved by %v after cancellation, want the cancelled slot reused", delta)
	}
}
