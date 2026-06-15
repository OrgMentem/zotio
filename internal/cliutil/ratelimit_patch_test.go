// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean static-audit): regression test for the AdaptiveLimiter.Wait race.
// Before the fix, concurrent callers all read the same lastRequest, slept the
// same delay, and returned together; afterward each reserves a distinct wake
// time under the lock. Assert that concurrent completions are spread out by
// roughly (N-1)*delay rather than collapsing into a single delay window.

package cliutil

import (
	"sync"
	"testing"
	"time"
)

func TestAdaptiveLimiterWaitConcurrentPacing(t *testing.T) {
	const rate = 200.0 // 5ms spacing
	const n = 8
	l := NewAdaptiveLimiter(rate)
	l.Wait() // prime: lastRequest := now, so the burst below must pace off it

	var wg sync.WaitGroup
	offsets := make([]time.Duration, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			l.Wait()
			offsets[idx] = time.Since(start)
		}(i)
	}
	wg.Wait()

	var maxOff time.Duration
	for _, off := range offsets {
		if off > maxOff {
			maxOff = off
		}
	}
	delay := time.Duration(float64(time.Second) / rate) // 5ms
	// With proper pacing the last caller wakes ~(n)*delay after priming; use a
	// loose lower bound so scheduler jitter cannot flake the test. The buggy
	// version completed the whole burst within ~one delay.
	wantMin := time.Duration(float64(n-1) * float64(delay) * 0.6)
	if maxOff < wantMin {
		t.Errorf("concurrent Wait did not pace: max completion offset %v < %v (race regression)", maxOff, wantMin)
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
