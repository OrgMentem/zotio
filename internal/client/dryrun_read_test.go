// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package client

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestDryRunExecutesReads pins the fix for the P1 defect where --dry-run
// stubbed EVERY method, including GET, and returned a fabricated
// {"dry_run":true} body with a nil error indistinguishable from real data
// through Get()/GetWithHeaders() (which discard status). A preview command
// that reads current state before describing a mutation needs that read to
// be real, or the preview describes a fiction. GET/HEAD must always dial
// through, dry-run or not.
func TestDryRunExecutesReads(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"real":true}`))
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.DryRun = true

	body, err := c.Get("/items/ABCD1234", nil)
	if err != nil {
		t.Fatalf("Get returned error under dry-run: %v", err)
	}
	if hits := atomic.LoadInt32(&hits); hits != 1 {
		t.Fatalf("server hits = %d, want 1 (GET must dial through under --dry-run)", hits)
	}
	if !bytes.Equal(body, []byte(`{"real":true}`)) {
		t.Fatalf("body = %s, want the server's real body", body)
	}
	if bytes.Contains(body, []byte("dry_run")) {
		t.Fatalf("body = %s, must not be the dry-run sentinel", body)
	}
}

// TestDryRunSuppressesWrites pins the half of the contract that must NOT
// regress: mutating verbs stay suppressed under --dry-run, returning the
// existing fabricated sentinel (nil error, no network call) so commands
// relying on --dry-run to preview a write without applying it are unaffected.
func TestDryRunSuppressesWrites(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"should_not_be_returned":true}`))
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.DryRun = true

	cases := []struct {
		verb string
		run  func() ([]byte, int, error)
	}{
		{"POST", func() ([]byte, int, error) { return c.Post("/items", map[string]string{"title": "x"}) }},
		{"PUT", func() ([]byte, int, error) { return c.Put("/items/1", map[string]string{"title": "x"}) }},
		{"PATCH", func() ([]byte, int, error) { return c.Patch("/items/1", map[string]string{"title": "x"}) }},
		{"DELETE", func() ([]byte, int, error) { return c.Delete("/items/1") }},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.verb, func(t *testing.T) {
			body, status, err := tc.run()
			if err != nil {
				t.Fatalf("%s returned error under dry-run: %v", tc.verb, err)
			}
			if status != 0 {
				t.Fatalf("%s status = %d, want 0 (dry-run sentinel)", tc.verb, status)
			}
			if !bytes.Equal(body, []byte(`{"dry_run": true}`)) {
				t.Fatalf("%s body = %s, want dry-run sentinel", tc.verb, body)
			}
		})
	}

	if hits := atomic.LoadInt32(&hits); hits != 0 {
		t.Fatalf("server hits = %d, want 0 (no mutating verb should dial out under --dry-run)", hits)
	}
}

// TestDryRunReadSurfacesServerErrors pins the property that makes --dry-run
// useful for previewing a mutation of something that may already be gone: a
// GET that 404s or 500s under dry-run must surface that error, not a silent
// fabricated success. Before the fix, doRequest never dialed for GET under
// DryRun at all, so a remotely-deleted resource looked exactly like a normal
// preview instead of failing loudly.
func TestDryRunReadSurfacesServerErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"NotFound", http.StatusNotFound},
		// 501 rather than 500: 501 is the one 5xx doRequest never retries
		// (see TestNonTransient501DoesNotRetry), so this case stays a single
		// round trip instead of paying the 5xx retry/backoff loop (1s+2s+4s).
		{"ServerError", http.StatusNotImplemented},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				http.Error(w, "gone", tc.status)
			}))
			defer server.Close()

			c := clientTestNewClient(t, server.URL)
			c.DryRun = true

			_, err := c.Get("/items/DELETED", nil)
			if err == nil {
				t.Fatalf("Get returned nil error for a %d response under dry-run", tc.status)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error type = %T, want *APIError", err)
			}
			if apiErr.StatusCode != tc.status {
				t.Fatalf("APIError.StatusCode = %d, want %d", apiErr.StatusCode, tc.status)
			}
			if hits := atomic.LoadInt32(&hits); hits != 1 {
				t.Fatalf("server hits = %d, want 1", hits)
			}
		})
	}
}

// TestDryRunGetVsDoRequestStatus documents, at the doRequest layer, that the
// dry-run mutating-verb sentinel (status 0, nil error) can never reach the
// Get/GetWithHeaders/GetWithHeadersContext family anymore, because GET never
// takes the dry-run branch. This is the resolution to the "sentinel is
// indistinguishable from real data through Get()" hazard: closed by
// construction rather than by a status-code convention Get() would have to
// start honoring.
func TestDryRunGetVsDoRequestStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"real":true}`))
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.DryRun = true

	_, status, _, err := c.doRequest(context.Background(), http.MethodGet, "/items", nil, nil, nil)
	if err != nil {
		t.Fatalf("doRequest(GET) returned error under dry-run: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("doRequest(GET) status = %d, want %d (real response, not the dry-run sentinel's 0)", status, http.StatusOK)
	}
}
