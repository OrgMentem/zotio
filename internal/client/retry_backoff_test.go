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
	base := 100 * time.Millisecond
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

	// The larger test base keeps scheduler noise small relative to each wait.
	// The lower bounds distinguish 100/200/400ms from a constant retry delay;
	// a wall-clock ratio would also measure unrelated HTTP and OS scheduling.
	expected := []time.Duration{base, 2 * base, 4 * base}
	for i, want := range expected {
		got := timestamps[i+1].Sub(timestamps[i])
		if got < want-20*time.Millisecond {
			t.Fatalf("gap %d: wait = %v, want at least %v", i, got, want-20*time.Millisecond)
		}
	}
	if elapsed > 5*time.Second {
		t.Fatalf("total elapsed = %v, want <5s with base=%v", elapsed, base)
	}
	if elapsed < 600*time.Millisecond {
		t.Fatalf("total elapsed = %v, want >= 600ms (sleeps missing?)", elapsed)
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
