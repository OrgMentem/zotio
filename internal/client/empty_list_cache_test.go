// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// An empty list must not be cached: it is usually a propagation artifact.

package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zotio/internal/config"
)

func TestIsEmptyJSONList(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`[]`, true},
		{"  [ ]  ", true},
		{"[\n]\n", true},
		{`[{"key":"A"}]`, false},
		{`{}`, false},
		{`{"results":[]}`, false},
		{``, false},
		{`null`, false},
	} {
		if got := isEmptyJSONList(json.RawMessage(tc.body)); got != tc.want {
			t.Errorf("isEmptyJSONList(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// zotio reads the local desktop API while writes route to api.zotero.org, so for
// a few seconds after a write a filtered query legitimately returns nothing.
// Caching that pinned the emptiness for the full 5-minute TTL: a tag rename
// previewed during the propagation window then applied nothing and reported
// success, because the apply re-read the cached empty match set.
func TestEmptyListResponseIsNotCached(t *testing.T) {
	requests := 0
	body := `[]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New(&config.Config{BaseURL: srv.URL + "/users/0"}, 5*time.Second, 0)
	c.cacheDir = t.TempDir()

	if _, err := c.Get("/items", map[string]string{"tag": "propagating"}); err != nil {
		t.Fatalf("first GET: %v", err)
	}
	// The propagation window closes and the tag becomes visible.
	body = `[{"key":"K1","data":{"key":"K1"}}]`
	data, err := c.Get("/items", map[string]string{"tag": "propagating"})
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	if requests != 2 {
		t.Fatalf("server saw %d requests; the empty result was cached and the second read never happened", requests)
	}
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("second GET returned %d items, want the now-visible one", len(items))
	}
}

// A non-empty response must still be cached, or every read pays full latency.
func TestNonEmptyListResponseIsCached(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1","data":{"key":"K1"}}]`))
	}))
	defer srv.Close()

	c := New(&config.Config{BaseURL: srv.URL + "/users/0"}, 5*time.Second, 0)
	c.cacheDir = t.TempDir()

	for range 2 {
		if _, err := c.Get("/items", map[string]string{"tag": "stable"}); err != nil {
			t.Fatalf("GET: %v", err)
		}
	}
	if requests != 1 {
		t.Fatalf("server saw %d requests, want 1: a non-empty response should be served from cache", requests)
	}
}
