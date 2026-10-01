// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// AdaptiveLimiter.Wait must reserve a distinct wake time for each concurrent
// caller, one interval after the previous reservation. Assert that every
// completion, not just the last, respects its slot.

package cliutil

import (
	"slices"
	"sync"
	"testing"
	"time"
)

func TestAdaptiveLimiterWaitConcurrentPacing(t *testing.T) {
	const rate = 200.0 // 5ms spacing
	const n = 8
	delay := time.Duration(float64(time.Second) / rate)
	l := NewAdaptiveLimiter(rate)
	l.Wait() // prime: lastRequest := now, so the burst below must pace off it
	l.mu.Lock()
	primed := l.lastRequest
	l.mu.Unlock()

	var wg sync.WaitGroup
	completions := make([]time.Time, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Wait()
			completions[i] = time.Now()
		}()
	}
	allDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(allDone)
	}()
	select {
	case <-allDone:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Wait calls did not all return")
	}

	// Reservations are strictly increasing, at least one interval apart, and
	// a caller never returns before its slot (timers do not fire early). So
	// by the k-th completion (0-based) k+1 callers hold distinct slots, the
	// latest at least (k+1) intervals after priming. This is a lower bound
	// only, so scheduler delay cannot flake it; any burst of two or more
	// callers inside one interval violates it.
	slices.SortFunc(completions, func(a, b time.Time) int { return a.Compare(b) })
	for k, at := range completions {
		if want := time.Duration(k+1) * delay; at.Sub(primed) < want {
			t.Errorf("completion %d at %v after priming, want >= %v (callers shared a slot)", k, at.Sub(primed), want)
		}
	}
}

func TestAdaptiveLimiterWaitNilNoop(t *testing.T) {
	var l *AdaptiveLimiter // disabled limiter
	done := make(chan struct{})
	go func() { l.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("nil AdaptiveLimiter.Wait blocked; expected immediate no-op")
	}
}
