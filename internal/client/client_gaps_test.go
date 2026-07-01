// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean test-gaps rf41): covers client do, retry, cache, sanitization, and cache-key behavior.

package client

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/config"
)

func clientTestNewClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c := New(&config.Config{BaseURL: baseURL}, 5*time.Second, 0)
	c.BaseURL = baseURL
	return c
}

func TestDoReturnsSuccessBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ok" {
			t.Fatalf("path = %q, want /ok", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	got, status, err := c.do(context.Background(), http.MethodGet, "/ok", nil, nil, nil)
	if err != nil {
		t.Fatalf("do returned error: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if !bytes.Equal(got, []byte(`{"ok":true}`)) {
		t.Fatalf("body = %s, want %s", got, `{"ok":true}`)
	}
}

func TestDoClientErrorReturnsAPIErrorWithoutRetry(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	_, status, err := c.do(context.Background(), http.MethodGet, "/missing", nil, nil, nil)
	if err == nil {
		t.Fatal("do returned nil error for 404")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("APIError.StatusCode = %d, want %d", apiErr.StatusCode, http.StatusNotFound)
	}
	if apiErr.Method != http.MethodGet || apiErr.Path != "/missing" {
		t.Fatalf("APIError request = %s %s, want GET /missing", apiErr.Method, apiErr.Path)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("server hits = %d, want 1", got)
	}
}

func TestDoRetriesServerErrorThenSucceeds(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			http.Error(w, "try again", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"retried":true}`))
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	got, status, err := c.do(context.Background(), http.MethodGet, "/retry", nil, nil, nil)
	if err != nil {
		t.Fatalf("do returned error after retry: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if !bytes.Equal(got, []byte(`{"retried":true}`)) {
		t.Fatalf("body = %s, want retry success body", got)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("server hits = %d, want 2", got)
	}
}

func TestGetCachesAndMutationInvalidatesCache(t *testing.T) {
	var getHits int32
	var mutationHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			count := atomic.AddInt32(&getHits, 1)
			_, _ = w.Write([]byte(`{"version":` + strconv.Itoa(int(count)) + `}`))
		case http.MethodPost:
			atomic.AddInt32(&mutationHits, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"mutated":true}`))
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.cacheDir = t.TempDir()
	params := map[string]string{"q": "one"}

	first, err := c.Get("/items", params)
	if err != nil {
		t.Fatalf("first Get returned error: %v", err)
	}
	second, err := c.Get("/items", params)
	if err != nil {
		t.Fatalf("second Get returned error: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("cached body = %s, want first body %s", second, first)
	}
	if got := atomic.LoadInt32(&getHits); got != 1 {
		t.Fatalf("GET hits before mutation = %d, want 1", got)
	}

	if _, _, err := c.Post("/items", map[string]string{"title": "new"}); err != nil {
		t.Fatalf("Post returned error: %v", err)
	}
	if got := atomic.LoadInt32(&mutationHits); got != 1 {
		t.Fatalf("mutation hits = %d, want 1", got)
	}

	third, err := c.Get("/items", params)
	if err != nil {
		t.Fatalf("third Get returned error: %v", err)
	}
	if bytes.Equal(third, first) {
		t.Fatalf("third body = %s, want a refreshed response after mutation", third)
	}
	if got := atomic.LoadInt32(&getHits); got != 2 {
		t.Fatalf("GET hits after mutation = %d, want 2", got)
	}
}

func TestSanitizeJSONResponse(t *testing.T) {
	clean := []byte(`{"items":[1]}`)
	if got := sanitizeJSONResponse(clean); !bytes.Equal(got, clean) {
		t.Fatalf("clean JSON sanitized to %q, want unchanged %q", got, clean)
	}
	if got := sanitizeJSONResponse(sanitizeJSONResponse(clean)); !bytes.Equal(got, clean) {
		t.Fatalf("sanitize is not idempotent for clean JSON: got %q", got)
	}

	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{name: "bom and xssi newline", in: []byte("\xEF\xBB\xBF)]}'\n \t\r\n{\"ok\":true}"), want: []byte(`{"ok":true}`)},
		{name: "xssi without newline", in: []byte(")]}'   {\"ok\":true}"), want: []byte(`{"ok":true}`)},
		{name: "angular prefix", in: []byte("{}&& \n[1]"), want: []byte(`[1]`)},
		{name: "for loop prefix", in: []byte("for(;;);\t{\"x\":1}"), want: []byte(`{"x":1}`)},
		{name: "while loop prefix", in: []byte("while(1);\r\nnull"), want: []byte(`null`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeJSONResponse(tc.in); !bytes.Equal(got, tc.want) {
				t.Fatalf("sanitizeJSONResponse(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCacheKeyDeterministicAndVaries(t *testing.T) {
	c := clientTestNewClient(t, "http://example.test")
	params := map[string]string{"b": "2", "a": "1"}
	reordered := map[string]string{"a": "1", "b": "2"}

	first := c.cacheKey("/items", params)
	if second := c.cacheKey("/items", params); second != first {
		t.Fatalf("cacheKey is not deterministic: %q then %q", first, second)
	}
	if got := c.cacheKey("/items", reordered); got != first {
		t.Fatalf("cacheKey depends on map iteration/order: %q vs %q", got, first)
	}
	if got := c.cacheKey("/other", params); got == first {
		t.Fatal("cacheKey did not change when path changed")
	}
	if got := c.cacheKey("/items", map[string]string{"a": "1", "b": "3"}); got == first {
		t.Fatal("cacheKey did not change when params changed")
	}
}

func TestReadWriteCacheHonorsFreshness(t *testing.T) {
	c := clientTestNewClient(t, "http://example.test")
	c.cacheDir = t.TempDir()
	params := map[string]string{"q": "cache"}
	want := []byte(`{"cached":true}`)

	c.writeCache("/items", params, want)
	got, ok := c.readCache("/items", params)
	if !ok {
		t.Fatal("readCache missed immediately after writeCache")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("cached body = %s, want %s", got, want)
	}

	cacheFile := filepath.Join(c.cacheDir, c.cacheKey("/items", params)+".json")
	old := time.Now().Add(-6 * time.Minute)
	if err := os.Chtimes(cacheFile, old, old); err != nil {
		t.Fatalf("aging cache file: %v", err)
	}
	if got, ok := c.readCache("/items", params); ok {
		t.Fatalf("readCache hit expired cache with body %s", got)
	}
}
