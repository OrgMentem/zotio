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

// thQueuedWaiter is one reservation enqueued by thEnqueueWaiter.
type thQueuedWaiter struct {
	waiter *adaptiveLimiterWaiter
	slot   time.Time // reservation at enqueue time
	cancel context.CancelFunc
	done   chan time.Time // receives the return time of WaitContext
}

// thEnqueueWaiter starts one WaitContext call and blocks until its
// reservation is the newest entry in the queue, so callers enqueue in a
// deterministic order.
func thEnqueueWaiter(t *testing.T, l *AdaptiveLimiter) thQueuedWaiter {
	t.Helper()
	l.mu.Lock()
	want := len(l.waiters) + 1
	l.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan time.Time, 1)
	go func() {
		l.WaitContext(ctx)
		done <- time.Now()
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		if len(l.waiters) == want {
			w := l.waiters[want-1]
			slot := w.at
			l.mu.Unlock()
			return thQueuedWaiter{waiter: w, slot: slot, cancel: cancel, done: done}
		}
		l.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiter %d did not reserve a slot", want)
	return thQueuedWaiter{}
}

func thRecvDone(t *testing.T, name string, done <-chan time.Time) time.Time {
	t.Helper()
	select {
	case at := <-done:
		return at
	case <-time.After(5 * time.Second):
		t.Fatalf("waiter %s did not return", name)
		return time.Time{}
	}
}

// Cancelling a waiter in the middle of the queue must compact only the
// reservations queued behind it: earlier waiters keep their slot, later
// waiters move forward exactly one interval in their original order, and the
// woken followers then return at the compacted deadline instead of sleeping
// until the slot they held before the cancellation.
func TestAdaptiveLimiterWaitContextCancellationCompactsQueuedFollowers(t *testing.T) {
	const rate = 2.0 // 500ms interval: wide margin between old and new deadlines
	delay := time.Duration(float64(time.Second) / rate)
	l := NewAdaptiveLimiter(rate)
	l.Wait()

	a := thEnqueueWaiter(t, l)
	b := thEnqueueWaiter(t, l)
	c := thEnqueueWaiter(t, l)
	d := thEnqueueWaiter(t, l)
	for _, pair := range [][2]thQueuedWaiter{{a, b}, {b, c}, {c, d}} {
		if got := pair[1].slot.Sub(pair[0].slot); got != delay {
			t.Fatalf("queued slots are %v apart, want %v", got, delay)
		}
	}

	b.cancel()
	thRecvDone(t, "b", b.done)

	l.mu.Lock()
	queue := append([]*adaptiveLimiterWaiter(nil), l.waiters...)
	slots := make([]time.Time, len(queue))
	for i, w := range queue {
		slots[i] = w.at
	}
	l.mu.Unlock()
	if len(queue) != 3 || queue[0] != a.waiter || queue[1] != c.waiter || queue[2] != d.waiter {
		t.Fatalf("queue after cancelling b has %d waiters or is out of order, want exactly [a c d]", len(queue))
	}
	if !slots[0].Equal(a.slot) {
		t.Errorf("slot a moved by %v, want unchanged (it was queued before the cancelled waiter)", slots[0].Sub(a.slot))
	}
	if !slots[1].Equal(b.slot) {
		t.Errorf("slot c = %v after b's slot, want b's slot reused", slots[1].Sub(b.slot))
	}
	if !slots[2].Equal(c.slot) {
		t.Errorf("slot d = %v after c's original slot, want c's original slot", slots[2].Sub(c.slot))
	}

	aDone := thRecvDone(t, "a", a.done)
	cDone := thRecvDone(t, "c", c.done)
	dDone := thRecvDone(t, "d", d.done)
	if aDone.Before(a.slot) || cDone.Before(b.slot) || dDone.Before(c.slot) {
		t.Errorf("a waiter returned before its compacted slot: a %v, c %v, d %v early",
			a.slot.Sub(aDone), b.slot.Sub(cDone), c.slot.Sub(dDone))
	}
	// A follower that was not woken would sleep to its pre-cancellation slot,
	// a full interval later than its compacted one.
	if !cDone.Before(c.slot) {
		t.Errorf("c returned %v after its compacted slot, at or past its original slot; follower was not woken", cDone.Sub(b.slot))
	}
	if !dDone.Before(d.slot) {
		t.Errorf("d returned %v after its compacted slot, at or past its original slot; follower was not woken", dDone.Sub(c.slot))
	}

	l.mu.Lock()
	leaked := len(l.waiters)
	last := l.lastRequest
	l.mu.Unlock()
	if leaked != 0 {
		t.Errorf("%d reservations remain queued after every waiter returned", leaked)
	}
	if !last.Equal(c.slot) {
		t.Errorf("lastRequest is %v after d's compacted slot, want d's compacted slot", last.Sub(c.slot))
	}
}
