// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/config"
)

func TestRetryBackoffScheduleScalesWithBase(t *testing.T) {
	base := 20 * time.Millisecond
	restore := SetRetryBackoffBaseForTest(base)
	defer restore()

	var hits atomic.Int64
	// To keep dependencies minimal, collect timestamps in a slice. The test
	// uses a single client so httptest's handler runs effectively serially.
	var timestamps []time.Time

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		timestamps = append(timestamps, time.Now())
		http.Error(w, "temporary failure", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(&config.Config{BaseURL: srv.URL}, 2*time.Second, 0)
	c.NoCache = true

	start := time.Now()
	_, err := c.Get("/items", nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Get on persistent 500 succeeded, want error after retries")
	}

	if got := hits.Load(); got != 4 {
		t.Fatalf("attempts = %d, want 4 (initial + 3 retries)", got)
	}

	if len(timestamps) != 4 {
		t.Fatalf("timestamps len = %d, want 4", len(timestamps))
	}

	// Expected waits between attempts: 1*base, 2*base, 4*base = 20ms, 40ms, 80ms.
	// Total expected sleep ≈ 140ms. Allow generous tolerance for scheduling jitter
	// but still distinguish 1x/2x/4x growth. We assert each interval individually.
	expected := []time.Duration{base, 2 * base, 4 * base}
	const tolerance = 25 * time.Millisecond // high jitter on loaded CI
	for i, want := range expected {
		got := timestamps[i+1].Sub(timestamps[i])
		// Subtract the request round-trip (~<5ms) which is included in the gap;
		// the sleep dominates so we just check the gap is at least want and not wildly over.
		if got < want-tolerance/2 {
			t.Fatalf("gap %d: wait = %v, want ~%v (too short)", i, got, want)
		}
		if got > want+tolerance+80*time.Millisecond {
			t.Fatalf("gap %d: wait = %v, want ~%v (too long)", i, got, want)
		}
	}

	// Growth check: each gap should be ~2x the previous (within tolerance).
	for i := 1; i < len(expected); i++ {
		prev := timestamps[i].Sub(timestamps[i-1])
		cur := timestamps[i+1].Sub(timestamps[i])
		if cur < prev {
			t.Fatalf("backoff not growing: gap %d (%v) < gap %d (%v)", i, cur, i-1, prev)
		}
		// Ratio roughly 2; allow 1.4 to 2.8 due to jitter
		ratio := float64(cur) / float64(prev)
		if ratio < 1.4 || ratio > 2.8 {
			t.Fatalf("backoff ratio gap %d/gap %d = %.2f, want ~2.0", i, i-1, ratio)
		}
	}

	// Total wall time should be ~140ms, not 7s. Fail if we slept real seconds.
	if elapsed > 1*time.Second {
		t.Fatalf("total elapsed = %v, want <1s with base=%v", elapsed, base)
	}
	if elapsed < 80*time.Millisecond {
		t.Fatalf("total elapsed = %v, want >= 80ms (sleeps missing?)", elapsed)
	}
}

func TestRetryBackoffDefaultIsOneSecond(t *testing.T) {
	// Restore to default and confirm the seam defaults to 1s.
	// This is the production default verification the task requires.
	restore := SetRetryBackoffBaseForTest(time.Second)
	restore() // reset to previous (which should be 1s after init)
	if got := retryBackoffBase(); got != time.Second {
		t.Fatalf("retryBackoffBase default = %v, want 1s", got)
	}
	// Also check the raw atomic holds 1e9 nanos.
	if got := retryBackoffBaseNanos.Load(); got != int64(time.Second) {
		t.Fatalf("retryBackoffBaseNanos = %d, want %d", got, int64(time.Second))
	}
}
