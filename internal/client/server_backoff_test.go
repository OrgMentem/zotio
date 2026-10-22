// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"zotio/internal/config"
)

// The Web API asks clients to pause with a Backoff header, even on a 2xx. The
// next request through the same client must wait it out.
func TestBackoffHeaderPausesNextRequest(t *testing.T) {
	var mu sync.Mutex
	var stamps []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		first := len(stamps) == 1
		mu.Unlock()
		if first {
			w.Header().Set("Backoff", "1")
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	for range 2 {
		if _, err := c.Get("/items", nil); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stamps) != 2 {
		t.Fatalf("requests = %d, want 2", len(stamps))
	}
	if gap := stamps[1].Sub(stamps[0]); gap < 950*time.Millisecond {
		t.Fatalf("gap between requests = %v, want >= the 1s the server asked for", gap)
	}
}

// A clone shares the session's backoff: a read client built for a global
// endpoint must not hammer a server the parent was told to leave alone.
func TestBackoffHeaderAppliesToClones(t *testing.T) {
	var mu sync.Mutex
	var stamps []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		first := len(stamps) == 1
		mu.Unlock()
		if first {
			w.Header().Set("Backoff", "1")
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	if _, err := c.Get("/items", nil); err != nil {
		t.Fatal(err)
	}
	clone := c.CloneForRead(srv.URL)
	if _, err := clone.Get("/items", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gap := stamps[1].Sub(stamps[0]); gap < 950*time.Millisecond {
		t.Fatalf("clone request gap = %v, want >= 1s", gap)
	}
}

func TestRetryAfterOnServerErrorOverridesExponentialBackoff(t *testing.T) {
	restore := SetRetryBackoffBaseForTest(10 * time.Millisecond)
	defer restore()

	var mu sync.Mutex
	var stamps []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		n := len(stamps)
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "maintenance", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	if _, err := c.Get("/items", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stamps) != 2 {
		t.Fatalf("requests = %d, want 2", len(stamps))
	}
	if gap := stamps[1].Sub(stamps[0]); gap < 950*time.Millisecond {
		t.Fatalf("retry gap = %v, want >= the 1s Retry-After (not the 10ms exponential base)", gap)
	}
}
