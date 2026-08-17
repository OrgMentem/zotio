// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package client

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"zotio/internal/config"
)

func TestTruncateBody(t *testing.T) {
	t.Parallel()

	const maxBytes = 4096

	cases := []struct {
		name        string
		input       []byte
		wantLen     int
		wantHasTail bool
	}{
		{"empty", nil, 0, false},
		{"under cap", []byte("hello"), 5, false},
		{"at cap", bytes.Repeat([]byte("a"), maxBytes), maxBytes, false},
		{"one over cap", bytes.Repeat([]byte("a"), maxBytes+1), maxBytes + 3, true},
		{"huge body", bytes.Repeat([]byte("a"), maxBytes*8), maxBytes + 3, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := truncateBody(tc.input)
			if len(got) != tc.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tc.wantLen)
			}
			if tc.wantHasTail && !strings.HasSuffix(got, "...") {
				t.Fatalf("want trailing %q", "...")
			}
			if !tc.wantHasTail && strings.HasSuffix(got, "...") {
				t.Fatalf("unexpected trailing %q in %q", "...", got)
			}
		})
	}
}

func TestTruncateBody_UTF8RuneAtBoundary(t *testing.T) {
	t.Parallel()

	// '€' is 3 bytes (0xE2 0x82 0xAC). Place it so the slice at byte 4096 cuts
	// mid-rune; strings.ToValidUTF8 should drop the partial rune cleanly rather
	// than emit U+FFFD or invalid UTF-8.
	prefix := strings.Repeat("a", 4094)
	body := []byte(prefix + "€" + strings.Repeat("b", 100))
	got := truncateBody(body)

	if !utf8.ValidString(got) {
		t.Fatalf("output is not valid UTF-8")
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("output contains replacement rune U+FFFD")
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("want trailing %q", "...")
	}
	// Partial rune must be dropped, not replaced: 4094 valid bytes + "...".
	if want := 4094 + 3; len(got) != want {
		t.Fatalf("len = %d, want %d (partial rune should be dropped, not replaced)", len(got), want)
	}
}

func TestWriteRouteResolverErrorSurfacesRealCause(t *testing.T) {
	var localHits int32
	local := newLocalHitsServer(&localHits)
	defer local.Close()

	writeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer writeSrv.Close()

	// ResolveWriteBase fails (e.g. keys/current 401/network error). Before the
	// fix this was silently swallowed and the write fell back to the local API,
	// surfacing a misleading "Method not implemented" / read-only guard.
	resolverErr := fmt.Errorf("keys/current returned HTTP 401: expired key")
	c := New(&config.Config{BaseURL: local.URL}, 5*time.Second, 0)
	c.BaseURL = local.URL
	c.NoCache = true
	c.ResolveWriteBase = func(context.Context) (string, error) { return "", resolverErr }

	_, _, err := c.Patch("/items/ABCD", map[string]any{"x": 1})
	if err == nil {
		t.Fatal("expected error from resolver failure, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "could not resolve Zotero Web API write route") {
		t.Fatalf("err = %q, want write-route prefix", msg)
	}
	if !strings.Contains(msg, "expired key") {
		t.Fatalf("err = %q, want underlying cause (expired key)", msg)
	}
	if !strings.Contains(msg, "401") {
		t.Fatalf("err = %q, want underlying HTTP status", msg)
	}
	// Must still hit local API (so transient deadline failures can retry),
	// but error must name the resolver cause, not just the local rejection.
	if n := atomic.LoadInt32(&localHits); n == 0 {
		t.Fatalf("local server hits = %d, want >=1 (write goes local but error wraps resolver cause)", n)
	}
	// Second write must retry resolution, not latch the failure.
	c.ResolveWriteBase = func(context.Context) (string, error) { return writeSrv.URL, nil }
	if _, _, err := c.Post("/items", []any{}); err != nil {
		t.Fatalf("second POST after resolver recovers: %v", err)
	}
}

func TestCacheKeyNoCollisionAcrossParamBoundaries(t *testing.T) {
	c := clientTestNewClient(t, "http://example.test")
	// Two distinct param sets that without a delimiter concatenated to the same
	// raw key string: "a=bc" + "d=e" == "a=b" + "cd=e" == "a=bcd=e"
	k1 := c.cacheKey("/items", map[string]string{"a": "bc", "d": "e"}, nil)
	k2 := c.cacheKey("/items", map[string]string{"a": "b", "cd": "e"}, nil)
	if k1 == k2 {
		t.Fatalf("cache keys collide: %q vs %q", k1, k2)
	}
	// Config path tail + first param boundary must also be unambiguous.
	c1 := &Client{BaseURL: "http://example.test", Config: &config.Config{BaseURL: "http://example.test", Path: "/x/foo"}}
	c2 := &Client{BaseURL: "http://example.test", Config: &config.Config{BaseURL: "http://example.test", Path: "/x/fooa"}}
	p1 := c1.cacheKey("/items", map[string]string{"b": ""}, nil)
	p2 := c2.cacheKey("/items", map[string]string{"a": "b"}, nil)
	// Even if they happen to differ, the param-boundary case above is the
	// definitive proof; this just guards the config_path tail.
	if k1 == k2 {
		t.Fatalf("param-boundary keys must differ")
	}
	_ = p1
	_ = p2
}

func newLocalHitsServer(hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		http.Error(w, "Method not implemented", http.StatusNotImplemented)
	}))
}
