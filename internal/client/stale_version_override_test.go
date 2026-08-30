// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zotio/internal/cliutil"
	"zotio/internal/config"
)

// newStaleVersionProbe returns a client pointed at a server that records the
// If-Unmodified-Since-Version header of every mutating request it receives.
func newStaleVersionProbe(t *testing.T) (*Client, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			seen = append(seen, r.Header.Get("If-Unmodified-Since-Version"))
		}
		w.Header().Set("Last-Modified-Version", "9")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	c := New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	return c, &seen
}

// TestStaleVersionOverrideRewritesThePrecondition proves the rehearsal
// affordance actually reaches the wire: with ZOTIO_TEST_STALE_VERSION set, the
// version the caller resolved at apply time is replaced by the forced one, which
// is what lets a live run provoke Zotero's own 412 rather than succeeding.
func TestStaleVersionOverrideRewritesThePrecondition(t *testing.T) {
	t.Setenv(cliutil.StaleVersionEnvVar, "1")
	c, seen := newStaleVersionProbe(t)

	if _, _, err := c.PatchWithHeaders("/items/ABCD1234",
		map[string]any{"title": "x"},
		map[string]string{"If-Unmodified-Since-Version": "4242"}); err != nil {
		t.Fatalf("patch: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("mutating requests = %d, want 1", len(*seen))
	}
	if got := (*seen)[0]; got != "1" {
		t.Fatalf("If-Unmodified-Since-Version = %q, want the forced %q", got, "1")
	}
}

// TestStaleVersionOverrideUnsetIsInert is the other direction, and the one that
// matters for every ordinary run: with the var unset the caller's resolved
// version must reach the server untouched.
func TestStaleVersionOverrideUnsetIsInert(t *testing.T) {
	t.Setenv(cliutil.StaleVersionEnvVar, "")
	c, seen := newStaleVersionProbe(t)

	if _, _, err := c.PatchWithHeaders("/items/ABCD1234",
		map[string]any{"title": "x"},
		map[string]string{"If-Unmodified-Since-Version": "4242"}); err != nil {
		t.Fatalf("patch: %v", err)
	}

	if got := (*seen)[0]; got != "4242" {
		t.Fatalf("If-Unmodified-Since-Version = %q, want the caller's %q", got, "4242")
	}
}

// TestStaleVersionOverrideNeverAddsAPrecondition is the safety boundary. Several
// writes deliberately carry no If-Unmodified-Since-Version, and Zotero's
// semantics differ sharply between an absent precondition and a stale one. The
// override must only ever rewrite an existing header, never introduce one, or
// setting it during a rehearsal would silently change which requests are
// guarded and invalidate the rehearsal it exists to serve.
func TestStaleVersionOverrideNeverAddsAPrecondition(t *testing.T) {
	t.Setenv(cliutil.StaleVersionEnvVar, "1")
	c, seen := newStaleVersionProbe(t)

	if _, _, err := c.Post("/items", map[string]any{"title": "x"}); err != nil {
		t.Fatalf("post: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("mutating requests = %d, want 1", len(*seen))
	}
	if got := (*seen)[0]; got != "" {
		t.Fatalf("If-Unmodified-Since-Version = %q on a write that sent none; want empty", got)
	}
}

// TestStaleVersionOverrideRejectsUnusableValues keeps a typo or a shell quoting
// accident from silently disabling the guard it is meant to force. A
// non-numeric or non-positive value must be inert rather than sending "0" or
// "abc", either of which Zotero rejects with a different error and would send a
// rehearsal chasing the wrong failure.
func TestStaleVersionOverrideRejectsUnusableValues(t *testing.T) {
	for _, raw := range []string{"abc", "0", "-3", " ", "1.5"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(cliutil.StaleVersionEnvVar, raw)
			if v, ok := cliutil.StaleVersionOverride(); ok {
				t.Fatalf("StaleVersionOverride(%q) = (%d, true), want inert", raw, v)
			}
		})
	}
}
