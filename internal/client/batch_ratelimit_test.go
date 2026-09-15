// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zotio/internal/config"
)

// A multi-object write carries its precondition in each object's body, so it
// gains none of the four headers that mark a request rate-limit retry-safe.
// Without PostVersionedObjects declaring it conditional, a single 429 would
// fail the whole request — up to 50 items — with no retry and no back-off,
// where the per-item guarded PATCH it replaces retried and slowed the limiter.
func TestPostVersionedObjectsRetriesRateLimit(t *testing.T) {
	t.Cleanup(SetRetryBackoffBaseForTest(time.Millisecond))

	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successful":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()

	c := New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	if _, _, err := c.PostVersionedObjects("/items", []map[string]any{{"key": "K1", "version": 7}}); err != nil {
		t.Fatalf("versioned batch write: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want the 429 retried once", attempts)
	}
}

// The declaration is opt-in: a plain Post stays unretried, because replaying a
// POST with no precondition is not at-most-once.
func TestPlainPostDoesNotRetryRateLimit(t *testing.T) {
	t.Cleanup(SetRetryBackoffBaseForTest(time.Millisecond))

	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	if _, _, err := c.Post("/items", []map[string]any{{"key": "K1"}}); err == nil {
		t.Fatal("a 429 must surface as an error for an unconditional POST")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want no retry for an unconditional POST", attempts)
	}
}

// The marker is internal plumbing and must never reach Zotero.
func TestConditionalBodyMarkerIsNeverSent(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(conditionalBodyHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successful":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()

	c := New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	if _, _, err := c.PostVersionedObjects("/items", []map[string]any{{"key": "K1", "version": 7}}); err != nil {
		t.Fatalf("versioned batch write: %v", err)
	}
	if seen != "" {
		t.Fatalf("internal marker reached the wire as %q", seen)
	}
}
