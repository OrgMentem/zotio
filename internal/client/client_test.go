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
	// A config path must participate in the key: two clients reading the same
	// path with the same params but different config files must not share a
	// cache entry.
	c1 := &Client{BaseURL: "http://example.test", Config: &config.Config{BaseURL: "http://example.test", Path: "/x/foo"}}
	c2 := &Client{BaseURL: "http://example.test", Config: &config.Config{BaseURL: "http://example.test", Path: "/x/fooa"}}
	p1 := c1.cacheKey("/items", map[string]string{"a": "b"}, nil)
	p2 := c2.cacheKey("/items", map[string]string{"a": "b"}, nil)
	if p1 == p2 {
		t.Fatalf("config-path keys collide: %q vs %q", p1, p2)
	}
}

func newLocalHitsServer(hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		http.Error(w, "Method not implemented", http.StatusNotImplemented)
	}))
}
func TestGetFromWriteBaseWithVersion(t *testing.T) {
	cases := []struct {
		name        string
		header      string
		body        string
		wantVersion int
	}{
		{
			name:        "header version wins over body",
			header:      "77",
			body:        `{"version":5}`,
			wantVersion: 77,
		},
		{
			name:        "header absent body numeric version",
			header:      "",
			body:        `{"version":42}`,
			wantVersion: 42,
		},
		{
			name:        "header absent body version json number large",
			header:      "",
			body:        `{"version":123456}`,
			wantVersion: 123456,
		},
		{
			name:        "header absent empty string version",
			header:      "",
			body:        `{"version":""}`,
			wantVersion: 0,
		},
		{
			name:        "neither header nor body version",
			header:      "",
			body:        `{"key":"ABC","title":"no version"}`,
			wantVersion: 0,
		},
		{
			name:        "invalid body version string",
			header:      "",
			body:        `{"version":"not-a-number"}`,
			wantVersion: 0,
		},
		{
			name:        "malformed json body yields zero",
			header:      "",
			body:        `not json`,
			wantVersion: 0,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set("Last-Modified-Version", tc.header)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := clientTestNewClient(t, srv.URL)
			c.NoCache = true

			body, got, err := c.GetFromWriteBaseWithVersionContext(context.Background(), "/items/ABC", nil)
			if err != nil {
				t.Fatalf("GetFromWriteBaseWithVersionContext err = %v, want nil", err)
			}
			if got != tc.wantVersion {
				t.Fatalf("version = %d, want %d", got, tc.wantVersion)
			}
			if string(body) != tc.body {
				t.Fatalf("body = %q, want %q", string(body), tc.body)
			}
		})
	}

	t.Run("nil context delegates to base context", func(t *testing.T) {
		const wantVersion = 99
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Last-Modified-Version", "99")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":99}`))
		}))
		defer srv.Close()

		c := clientTestNewClient(t, srv.URL)
		c.NoCache = true

		// Context form with nil should fall back to baseCtx and succeed.
		bodyNil, verNil, errNil := c.GetFromWriteBaseWithVersionContext(nil, "/items/ABC", nil)
		if errNil != nil {
			t.Fatalf("GetFromWriteBaseWithVersionContext(nil) err = %v, want nil", errNil)
		}
		if verNil != wantVersion {
			t.Fatalf("GetFromWriteBaseWithVersionContext(nil) version = %d, want %d", verNil, wantVersion)
		}
		if string(bodyNil) != `{"version":99}` {
			t.Fatalf("GetFromWriteBaseWithVersionContext(nil) body = %q, want %q", string(bodyNil), `{"version":99}`)
		}

		// Non-context wrapper must delegate to the Context form and return the same result.
		bodyWrap, verWrap, errWrap := c.GetFromWriteBaseWithVersion("/items/ABC", nil)
		if errWrap != nil {
			t.Fatalf("GetFromWriteBaseWithVersion err = %v, want nil", errWrap)
		}
		if verWrap != wantVersion {
			t.Fatalf("GetFromWriteBaseWithVersion version = %d, want %d", verWrap, wantVersion)
		}
		if string(bodyWrap) != `{"version":99}` {
			t.Fatalf("GetFromWriteBaseWithVersion body = %q, want %q", string(bodyWrap), `{"version":99}`)
		}

		// Explicit background context must also match.
		_, verBg, errBg := c.GetFromWriteBaseWithVersionContext(context.Background(), "/items/ABC", nil)
		if errBg != nil {
			t.Fatalf("GetFromWriteBaseWithVersionContext(Background) err = %v, want nil", errBg)
		}
		if verBg != wantVersion {
			t.Fatalf("GetFromWriteBaseWithVersionContext(Background) version = %d, want %d", verBg, wantVersion)
		}
	})
}

func TestWriteBaseForRead(t *testing.T) {
	t.Run("uses WriteBaseURL directly", func(t *testing.T) {
		var readHits, writeHits int32
		readSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&readHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":1}`))
		}))
		defer readSrv.Close()
		writeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&writeHits, 1)
			w.Header().Set("Last-Modified-Version", "55")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":55}`))
		}))
		defer writeSrv.Close()

		c := clientTestNewClient(t, readSrv.URL)
		c.NoCache = true
		c.WriteBaseURL = writeSrv.URL
		c.ResolveWriteBase = func(context.Context) (string, error) {
			t.Fatalf("ResolveWriteBase should not be called when WriteBaseURL already set")
			return "", nil
		}

		base, err := c.writeBaseForRead(context.Background())
		if err != nil {
			t.Fatalf("writeBaseForRead err = %v, want nil", err)
		}
		if base != writeSrv.URL {
			t.Fatalf("writeBaseForRead base = %q, want %q", base, writeSrv.URL)
		}

		_, ver, err := c.GetFromWriteBaseWithVersionContext(context.Background(), "/items/ABC", nil)
		if err != nil {
			t.Fatalf("GetFromWriteBaseWithVersionContext err = %v, want nil", err)
		}
		if ver != 55 {
			t.Fatalf("version = %d, want %d", ver, 55)
		}
		if n := atomic.LoadInt32(&readHits); n != 0 {
			t.Fatalf("read plane hits = %d, want %d", n, 0)
		}
		if n := atomic.LoadInt32(&writeHits); n != 1 {
			t.Fatalf("write plane hits = %d, want %d", n, 1)
		}
	})

	t.Run("calls ResolveWriteBase when WriteBaseURL empty", func(t *testing.T) {
		var readHits, writeHits, resolveCalls int32
		readSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&readHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":1}`))
		}))
		defer readSrv.Close()
		writeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&writeHits, 1)
			w.Header().Set("Last-Modified-Version", "88")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":88}`))
		}))
		defer writeSrv.Close()

		c := clientTestNewClient(t, readSrv.URL)
		c.NoCache = true
		c.WriteBaseURL = ""
		c.ResolveWriteBase = func(ctx context.Context) (string, error) {
			atomic.AddInt32(&resolveCalls, 1)
			return writeSrv.URL, nil
		}

		base, err := c.writeBaseForRead(context.Background())
		if err != nil {
			t.Fatalf("writeBaseForRead err = %v, want nil", err)
		}
		if base != writeSrv.URL {
			t.Fatalf("writeBaseForRead base = %q, want %q", base, writeSrv.URL)
		}
		if n := atomic.LoadInt32(&resolveCalls); n != 1 {
			t.Fatalf("resolve calls = %d, want %d", n, 1)
		}

		// Reset counters after the direct writeBaseForRead call which already resolved and cached the base.
		// The subsequent Get must go to the resolved write host, not the read host.
		atomic.StoreInt32(&readHits, 0)
		atomic.StoreInt32(&writeHits, 0)

		_, ver, err := c.GetFromWriteBaseWithVersionContext(context.Background(), "/items/ABC", nil)
		if err != nil {
			t.Fatalf("GetFromWriteBaseWithVersionContext err = %v, want nil", err)
		}
		if ver != 88 {
			t.Fatalf("version = %d, want %d", ver, 88)
		}
		if n := atomic.LoadInt32(&readHits); n != 0 {
			t.Fatalf("read plane hits = %d, want %d", n, 0)
		}
		if n := atomic.LoadInt32(&writeHits); n != 1 {
			t.Fatalf("write plane hits = %d, want %d", n, 1)
		}
	})

	t.Run("resolver error fails without fallback", func(t *testing.T) {
		var readHits int32
		readSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&readHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":1}`))
		}))
		defer readSrv.Close()

		c := clientTestNewClient(t, readSrv.URL)
		c.NoCache = true
		c.WriteBaseURL = ""
		resolverErr := fmt.Errorf("keys/current returned HTTP 401: expired key")
		c.ResolveWriteBase = func(context.Context) (string, error) { return "", resolverErr }

		_, err := c.writeBaseForRead(context.Background())
		if err == nil {
			t.Fatal("writeBaseForRead err = nil, want error")
		}
		if !strings.Contains(err.Error(), "could not resolve Zotero Web API write base") {
			t.Fatalf("writeBaseForRead err = %q, want write base prefix", err.Error())
		}
		if !strings.Contains(err.Error(), "expired key") {
			t.Fatalf("writeBaseForRead err = %q, want underlying cause", err.Error())
		}

		_, _, err = c.GetFromWriteBaseWithVersionContext(context.Background(), "/items/ABC", nil)
		if err == nil {
			t.Fatal("GetFromWriteBaseWithVersionContext err = nil, want error")
		}
		if !strings.Contains(err.Error(), "could not resolve Zotero Web API write base") {
			t.Fatalf("GetFromWriteBaseWithVersionContext err = %q, want write base prefix", err.Error())
		}
		if n := atomic.LoadInt32(&readHits); n != 0 {
			t.Fatalf("read plane hits = %d, want %d", n, 0)
		}
	})

	t.Run("resolver empty base fails without fallback", func(t *testing.T) {
		var readHits int32
		readSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&readHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":1}`))
		}))
		defer readSrv.Close()

		c := clientTestNewClient(t, readSrv.URL)
		c.NoCache = true
		c.WriteBaseURL = ""
		c.ResolveWriteBase = func(context.Context) (string, error) { return "", nil }

		_, err := c.writeBaseForRead(context.Background())
		if err == nil {
			t.Fatal("writeBaseForRead err = nil, want error")
		}
		if !strings.Contains(err.Error(), "could not resolve the Zotero Web API write base") {
			t.Fatalf("writeBaseForRead err = %q, want empty-base message", err.Error())
		}
		if !strings.Contains(err.Error(), "refusing to take a write precondition from the local read plane") {
			t.Fatalf("writeBaseForRead err = %q, want local-plane refusal", err.Error())
		}

		_, _, err = c.GetFromWriteBaseWithVersionContext(context.Background(), "/items/ABC", nil)
		if err == nil {
			t.Fatal("GetFromWriteBaseWithVersionContext err = nil, want error")
		}
		if n := atomic.LoadInt32(&readHits); n != 0 {
			t.Fatalf("read plane hits = %d, want %d", n, 0)
		}
	})
}
