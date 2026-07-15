// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// hybrid routing — non-GET requests go to the lazily-resolved WriteBaseURL
// (the Web API) while GETs stay on BaseURL (the local API).

package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/config"
)

func TestWriteRoutingSplitsReadsAndWrites(t *testing.T) {
	var readHits, writeHits, resolveCalls int32
	readSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&readHits, 1)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer readSrv.Close()
	writeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&writeHits, 1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer writeSrv.Close()

	c := New(&config.Config{BaseURL: readSrv.URL}, 5*time.Second, 0)
	c.NoCache = true
	c.ResolveWriteBase = func(ctx context.Context) (string, error) {
		atomic.AddInt32(&resolveCalls, 1)
		return writeSrv.URL, nil
	}

	if _, err := c.Get("/items", nil); err != nil {
		t.Fatalf("GET: %v", err)
	}
	if _, _, err := c.Patch("/items/A", map[string]any{"x": 1}); err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	if _, _, err := c.Post("/items", []any{}); err != nil {
		t.Fatalf("POST: %v", err)
	}

	if n := atomic.LoadInt32(&readHits); n != 1 {
		t.Errorf("read server hits = %d, want 1 (GET stays on BaseURL)", n)
	}
	if n := atomic.LoadInt32(&writeHits); n != 2 {
		t.Errorf("write server hits = %d, want 2 (PATCH+POST routed)", n)
	}
	if n := atomic.LoadInt32(&resolveCalls); n != 1 {
		t.Errorf("resolver calls = %d, want 1 (lazy, resolved once)", n)
	}
}

func TestWriteRoutingFallsBackWhenResolverEmpty(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	c.ResolveWriteBase = func(ctx context.Context) (string, error) { return "", nil } // nothing to route to

	if _, _, err := c.Post("/items", []any{}); err != nil {
		t.Fatalf("POST: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("base server hits = %d, want 1 (write falls back to BaseURL)", n)
	}
}

func TestWriteRoutingRetriesAfterTransientResolveFailure(t *testing.T) {
	var baseHits, writeHits, resolveCalls int32
	baseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&baseHits, 1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer baseSrv.Close()
	writeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&writeHits, 1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer writeSrv.Close()

	c := New(&config.Config{BaseURL: baseSrv.URL}, 5*time.Second, 0)
	c.NoCache = true
	// First resolution fails transiently (e.g. network timeout); the second
	// succeeds. A sync.Once would consume the single attempt and latch the
	// read-only fallback forever; resolution must instead retry until it wins.
	c.ResolveWriteBase = func(ctx context.Context) (string, error) {
		if atomic.AddInt32(&resolveCalls, 1) == 1 {
			return "", context.DeadlineExceeded
		}
		return writeSrv.URL, nil
	}

	if _, _, err := c.Post("/items", []any{}); err != nil {
		t.Fatalf("first POST: %v", err)
	}
	if _, _, err := c.Post("/items", []any{}); err != nil {
		t.Fatalf("second POST: %v", err)
	}

	if n := atomic.LoadInt32(&resolveCalls); n != 2 {
		t.Errorf("resolver calls = %d, want 2 (transient failure must be retried, not latched)", n)
	}
	if n := atomic.LoadInt32(&baseHits); n != 1 {
		t.Errorf("base server hits = %d, want 1 (only the first write falls back)", n)
	}
	if n := atomic.LoadInt32(&writeHits); n != 1 {
		t.Errorf("write server hits = %d, want 1 (second write routes after retry succeeds)", n)
	}
}
