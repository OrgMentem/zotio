// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Zotero answers a missing tag and a missing collection's child list with an
// empty-bodied HTTP 500. That is a permanent not-found: it must come back as
// 404 without eating the retry budget, and an empty 500 anywhere else must
// stay a server error.
func TestZoteroEmpty500RewritesToNotFoundOnlyForMissingObjectPaths(t *testing.T) {
	restore := SetRetryBackoffBaseForTest(time.Millisecond)
	defer restore()

	for _, tc := range []struct {
		path         string
		wantStatus   int
		wantRequests int32
	}{
		{"/users/0/tags/zzz", 404, 1},
		{"/users/0/collections/ABC123/collections", 404, 1},
		{"/users/0/items", 500, 4},
		{"/users/0/tags", 500, 4},
		{"/users/0/collections/ABC123", 500, 4},
	} {
		var requests int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requests, 1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		c := clientTestNewClient(t, server.URL)
		c.Quiet = true
		c.NoCache = true
		_, status, _, err := c.doRequest(context.Background(), http.MethodGet, tc.path, nil, nil, nil)
		server.Close()
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.wantStatus || status != tc.wantStatus {
			t.Errorf("%s: err = %v status = %d, want APIError with status %d", tc.path, err, status, tc.wantStatus)
		}
		if got := atomic.LoadInt32(&requests); got != tc.wantRequests {
			t.Errorf("%s: requests = %d, want %d", tc.path, got, tc.wantRequests)
		}
	}
}

// A mutating request whose server status was definitively a 4xx stays a plain
// APIError even when the body cannot be read; only 5xx and 2xx stay ambiguous.
func TestMutatingErrorBodyReadFailureClassifiedByStatus(t *testing.T) {
	for _, tc := range []struct {
		status        int
		wantAmbiguous bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusPreconditionFailed, false},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
	} {
		transport := clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: tc.status,
				Header:     make(http.Header),
				Body:       io.NopCloser(errorReader{err: errors.New("body reset")}),
				Request:    req,
			}, nil
		})
		c := clientTestNewClient(t, "https://example.test")
		c.NoCache = true
		c.Quiet = true
		c.HTTPClient = &http.Client{Transport: transport}
		_, status, _, err := c.doRequest(context.Background(), http.MethodPost, "/items", nil, map[string]any{"t": "x"}, map[string]string{"If-Unmodified-Since-Version": "7"})
		if got := IsAmbiguousWriteError(err); got != tc.wantAmbiguous {
			t.Errorf("HTTP %d: ambiguous = %v, want %v (err %v)", tc.status, got, tc.wantAmbiguous, err)
		}
		if !tc.wantAmbiguous {
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status || status != tc.status {
				t.Errorf("HTTP %d: err %v status %d does not carry the server's status", tc.status, err, status)
			}
		}
	}
}

// A refused dial wrote nothing, so a mutating request that fails that way is a
// definite failure; a connection that is accepted and then dropped stays
// ambiguous.
func TestMutatingRefusedDialIsNotAmbiguous(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	c := clientTestNewClient(t, "http://"+addr)
	c.NoCache = true
	c.Quiet = true
	_, _, err = c.Post("/items", map[string]string{"title": "x"})
	if err == nil {
		t.Fatal("Post to a closed port succeeded")
	}
	if IsAmbiguousWriteError(err) {
		t.Fatalf("refused dial reported as ambiguous write: %v", err)
	}

	dropper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer dropper.Close()
	c = clientTestNewClient(t, dropper.URL)
	c.NoCache = true
	c.Quiet = true
	_, _, err = c.Post("/items", map[string]string{"title": "x"})
	if !IsAmbiguousWriteError(err) {
		t.Fatalf("dropped connection error = %v, want ambiguous write", err)
	}
}

// --quiet prints nothing: a retried request must not write its retry notice
// to stderr, and without Quiet the notice is still shown.
func TestQuietSilencesRetryNotices(t *testing.T) {
	restore := SetRetryBackoffBaseForTest(time.Millisecond)
	defer restore()

	run := func(quiet bool) string {
		var hits int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&hits, 1) == 1 {
				http.Error(w, "boom", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()
		c := clientTestNewClient(t, server.URL)
		c.NoCache = true
		c.Quiet = quiet

		orig := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = w
		_, getErr := c.Get("/items", nil)
		os.Stderr = orig
		_ = w.Close()
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		if getErr != nil {
			t.Fatalf("Get: %v", getErr)
		}
		return buf.String()
	}
	if out := run(true); out != "" {
		t.Errorf("quiet client wrote to stderr: %q", out)
	}
	if out := run(false); !strings.Contains(out, "retrying") {
		t.Errorf("non-quiet client stderr = %q, want a retry notice", out)
	}
}

// The cache generation is advanced before a mutation is dispatched, so a
// process killed after the server commits but before the post-response
// invalidation cannot leave the pre-write responses valid for the next run.
func TestMutationAdvancesCacheGenerationBeforeDispatch(t *testing.T) {
	var c *Client
	var markerDuringPost string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			data, _ := os.ReadFile(c.cacheGenerationMarkerPath())
			markerDuringPost = string(data)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	c = clientTestNewClient(t, server.URL)
	c.cacheDir = t.TempDir() + "/cache"
	c.Quiet = true
	if _, err := c.Get("/items", nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(c.cacheGenerationMarkerPath())
	if _, _, err := c.Post("/items", map[string]string{"title": "x"}); err != nil {
		t.Fatal(err)
	}
	if markerDuringPost == "" || markerDuringPost == string(before) {
		t.Fatalf("generation marker during the POST = %q (before = %q), want it advanced before dispatch", markerDuringPost, before)
	}
}

// CloneForRead is read-only in fact, not only in its name: a mutation through
// the clone is refused before any request is sent, while reads and the
// original client keep working.
func TestCloneForReadRefusesMutations(t *testing.T) {
	var posts, gets int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			atomic.AddInt32(&gets, 1)
		} else {
			atomic.AddInt32(&posts, 1)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.NoCache = true
	clone := c.CloneForRead(server.URL)
	if _, _, err := clone.Post("/items", map[string]string{"title": "x"}); err == nil {
		t.Error("clone.Post succeeded, want a read-only refusal")
	}
	if _, _, err := clone.Delete("/items/ABCD"); err == nil {
		t.Error("clone.Delete succeeded, want a read-only refusal")
	}
	if _, err := clone.Get("/items", nil); err != nil {
		t.Errorf("clone.Get: %v", err)
	}
	if _, _, err := c.Post("/items", map[string]string{"title": "x"}); err != nil {
		t.Errorf("original client Post: %v", err)
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Errorf("server saw %d mutations, want exactly the original client's 1", got)
	}
}

// A refused dial proves nothing was written only for the first hop. When the
// first hop committed and answered with a redirect whose target refuses the
// connection, the write may have been applied, so it stays ambiguous.
func TestMutatingRefusedDialAfterRedirectStaysAmbiguous(t *testing.T) {
	var posts int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		// Stop accepting and drop this connection after the reply, so the
		// followed same-origin redirect must dial again and is refused. 307
		// keeps the method and body.
		_ = server.Listener.Close()
		w.Header().Set("Connection", "close")
		http.Redirect(w, r, "/items-moved", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.NoCache = true
	c.Quiet = true
	_, _, err := c.Post("/items", map[string]string{"title": "x"})
	if err == nil {
		t.Fatal("Post succeeded, want a failed redirect dial")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("err = %v, want the redirect hop's refused dial", err)
	}
	if atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("server saw %d requests, want 1", posts)
	}
	if !IsAmbiguousWriteError(err) {
		t.Fatalf("refused dial after a redirect = %v, want ambiguous write", err)
	}
}

// A refused dial is a definite failure only when no hop ever held a
// connection. net/http retries a request internally on a reused connection
// whose response was lost; if that retry's dial is refused the first send may
// have committed. The fake transport reports the connection through the
// request's httptrace hooks, as the real transport does.
func TestMutatingRefusedDialAfterConnectionAcquiredStaysAmbiguous(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for _, tc := range []struct {
		name          string
		acquireConn   func(*httptrace.ClientTrace)
		wantAmbiguous bool
	}{
		{"no connection ever acquired", func(*httptrace.ClientTrace) {}, false},
		{"GotConn fired before the refused retry", func(tr *httptrace.ClientTrace) { tr.GotConn(httptrace.GotConnInfo{Reused: true}) }, true},
		{"WroteHeaders fired before the refused retry", func(tr *httptrace.ClientTrace) { tr.WroteHeaders() }, true},
	} {
		transport := clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
				tc.acquireConn(trace)
			}
			return nil, refused
		})
		c := clientTestNewClient(t, "https://example.test")
		c.NoCache = true
		c.Quiet = true
		c.HTTPClient = &http.Client{Transport: transport}
		_, _, _, err := c.doRequest(context.Background(), http.MethodPost, "/items", nil, map[string]any{"title": "x"}, nil)
		if err == nil {
			t.Fatalf("%s: request succeeded", tc.name)
		}
		if got := IsAmbiguousWriteError(err); got != tc.wantAmbiguous {
			t.Errorf("%s: ambiguous = %v, want %v (err %v)", tc.name, got, tc.wantAmbiguous, err)
		}
	}
}
