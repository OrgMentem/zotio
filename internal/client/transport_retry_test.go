// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/config"
)

// --timeout is the operation's budget. A server that accepts the connection
// and never answers must cost one timeout, not one per retry.
func TestStalledServerCostsOneTimeout(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	const timeout = 300 * time.Millisecond
	c := New(&config.Config{BaseURL: srv.URL}, timeout, 0)
	c.NoCache = true

	start := time.Now()
	_, err := c.Get("/items", nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Get against a stalled server succeeded")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1: a timed-out read must not be repeated", got)
	}
	if elapsed > 2*timeout {
		t.Errorf("elapsed = %v, want about one timeout (%v)", elapsed, timeout)
	}
}

// A dropped connection is retried, but spaced by the same exponential schedule
// as a 5xx instead of four attempts back to back.
func TestTransportErrorRetriesAreSpacedByBackoff(t *testing.T) {
	base := 100 * time.Millisecond
	restore := SetRetryBackoffBaseForTest(base)
	defer restore()

	var mu sync.Mutex
	var stamps []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		mu.Unlock()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	c := New(&config.Config{BaseURL: srv.URL}, 2*time.Second, 0)
	c.NoCache = true
	if _, err := c.Get("/items", nil); err == nil {
		t.Fatal("Get against a dropping server succeeded")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stamps) != 4 {
		t.Fatalf("attempts = %d, want 4 (initial + 3 retries)", len(stamps))
	}
	for i, want := range []time.Duration{base, 2 * base, 4 * base} {
		if gap := stamps[i+1].Sub(stamps[i]); gap < want-20*time.Millisecond {
			t.Errorf("gap %d = %v, want at least %v", i, gap, want-20*time.Millisecond)
		}
	}
}

// Nothing listening cannot start succeeding within the retry window, and a
// delay here would postpone the mirror fallback of --data-source auto.
func TestRefusedConnectionIsNotRetried(t *testing.T) {
	restore := SetRetryBackoffBaseForTest(300 * time.Millisecond)
	defer restore()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	c := New(&config.Config{BaseURL: "http://" + addr}, 2*time.Second, 0)
	c.NoCache = true
	start := time.Now()
	if _, err := c.Get("/items", nil); err == nil {
		t.Fatal("Get against a closed port succeeded")
	}
	if elapsed := time.Since(start); elapsed >= 300*time.Millisecond {
		t.Fatalf("elapsed = %v, want no retry backoff for a refused connection", elapsed)
	}
}
