// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: writes to the read-only Zotero local API return 501 (PUT/PATCH); 501 is
// never transient, so it must not trigger the 5xx retry/backoff loop.

package client

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/config"
)

func TestPatchDoesNotRetry501(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "Method not implemented", http.StatusNotImplemented)
	}))
	defer srv.Close()

	c := New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true

	_, status, err := c.Patch("/items/ABCD", map[string]any{"deleted": 0})
	if err == nil {
		t.Fatal("expected an error for HTTP 501")
	}
	if status != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", status)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("501 was retried: %d requests, want exactly 1 (no retry)", n)
	}
}
