// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

// versionServer serves the batch version map (format=versions) and counts how
// many times it is actually asked. The version it reports changes between
// calls, which is what makes a stale read visible.
type versionServer struct {
	mu       sync.Mutex
	requests int
	versions []int
}

func (v *versionServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("format") != "versions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		v.mu.Lock()
		i := v.requests
		v.requests++
		v.mu.Unlock()

		ver := v.versions[len(v.versions)-1]
		if i < len(v.versions) {
			ver = v.versions[i]
		}
		keys := strings.Split(r.URL.Query().Get("itemKey"), ",")
		body := map[string]int{}
		for _, k := range keys {
			if k != "" {
				body[k] = ver
			}
		}
		payload, _ := json.Marshal(body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Last-Modified-Version", fmt.Sprintf("%d", ver))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (v *versionServer) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.requests
}

// cachingClient returns a client with the response cache ENABLED, isolated to
// this test's own directory. Every other vault push test sets NoCache, which is
// exactly why the staleness this file pins went unnoticed.
func cachingClient(t *testing.T, baseURL string) *client.Client {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	c := client.New(&config.Config{BaseURL: baseURL}, 5*time.Second, 0)
	if c.NoCache {
		t.Fatal("client.New returned NoCache=true, so this test cannot exercise the cache")
	}
	return c
}

// TestFetchNoteVersionsObservesLiveVersions pins the fix for a defect found by
// reading the read path against its own contract: fetchNoteVersions issued the
// batch version map through the CACHED Get, while getNote twenty lines above it
// used the cache-bypassing GetWithVersion.
//
// Every version in that map becomes either a skip decision or an
// If-Unmodified-Since-Version precondition, so serving it from a five-minute
// cache makes a remote edit invisible: the note is silently skipped, or the
// guarded PATCH 412s on a conflict that does not exist.
//
// Without the fix this test fails twice over: the second call returns the stale
// version 10 instead of 20, and the server is asked exactly once.
func TestFetchNoteVersionsObservesLiveVersions(t *testing.T) {
	vs := &versionServer{versions: []int{10, 20}}
	srv := vs.start(t)
	c := cachingClient(t, srv.URL)

	first, err := fetchNoteVersions(c, []string{"NOTEKEY1"})
	if err != nil {
		t.Fatalf("first fetchNoteVersions: %v", err)
	}
	if got := first["NOTEKEY1"]; got != 10 {
		t.Fatalf("first version = %d, want 10", got)
	}

	// The remote moved. A cached map would hide it.
	second, err := fetchNoteVersions(c, []string{"NOTEKEY1"})
	if err != nil {
		t.Fatalf("second fetchNoteVersions: %v", err)
	}
	if got := second["NOTEKEY1"]; got != 20 {
		t.Fatalf("second version = %d, want 20: the version map was served from the response cache, "+
			"so a remote edit is invisible to the push diff", got)
	}
	if n := vs.count(); n != 2 {
		t.Errorf("server requests = %d, want 2: the second read did not reach the live plane", n)
	}
}

// TestFetchNoteVersionsAbsentKeyStaysAbsent guards the behavior the cache fix
// must not disturb. A key missing from Zotero's response means the note was
// deleted remotely, and it is signalled by absence from the map rather than by
// a zero version, because zero is also what an unparseable version coerces to.
func TestFetchNoteVersionsAbsentKeyStaysAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// PRESENT1 exists; GONEKEY2 is simply not in the reply.
		_, _ = w.Write([]byte(`{"PRESENT1":7}`))
	}))
	t.Cleanup(srv.Close)
	c := cachingClient(t, srv.URL)

	got, err := fetchNoteVersions(c, []string{"PRESENT1", "GONEKEY2"})
	if err != nil {
		t.Fatalf("fetchNoteVersions: %v", err)
	}
	if v, ok := got["PRESENT1"]; !ok || v != 7 {
		t.Errorf("PRESENT1 = %d (present=%v), want 7", v, ok)
	}
	if v, ok := got["GONEKEY2"]; ok {
		t.Errorf("GONEKEY2 present with version %d, want absent so the diff can report remote_deleted", v)
	}
}

// TestFetchNoteVersionsPropagatesReadFailure pins that a failed version fetch
// is unknown state, not absence. Returning a partial map here would let the
// push diff read a transport failure as "every note was deleted remotely".
//
// The status is 400 rather than 500 deliberately: the client retries 5xx with
// backoff, which would add seconds to the suite to prove nothing extra.
func TestFetchNoteVersionsPropagatesReadFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	c := cachingClient(t, srv.URL)

	got, err := fetchNoteVersions(c, []string{"NOTEKEY1"})
	if err == nil {
		t.Fatalf("fetchNoteVersions succeeded on a 400, returning %v", got)
	}
	if got != nil {
		t.Errorf("map = %v, want nil so no caller can mistake a failure for absence", got)
	}
}
