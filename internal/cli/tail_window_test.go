// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

// tailWindowServer serves total changed items from /items the way the Web API
// does: it honours start, caps a page at pageCap whatever limit asks for, and
// reports the library version and Total-Results on every page. version is
// consulted per request so a test can move the library mid-walk.
type tailWindowServer struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

func newTailWindowServer(t *testing.T, total, pageCap int, version func(start int, request int) string, failAt int) *tailWindowServer {
	t.Helper()
	s := &tailWindowServer{}
	mux := http.NewServeMux()
	itemRequests := 0
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.RequestURI())
		itemRequests++
		request := itemRequests
		s.mu.Unlock()

		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		if failAt > 0 && start >= failAt {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		end := min(start+pageCap, total)
		var objs []string
		for i := start; i < end; i++ {
			objs = append(objs, fmt.Sprintf(`{"key":"K%04d","version":%d,"data":{"key":"K%04d"}}`, i, i+1, i))
		}
		w.Header().Set("Last-Modified-Version", version(start, request))
		w.Header().Set("Total-Results", strconv.Itoa(total))
		_, _ = fmt.Fprintf(w, "[%s]", strings.Join(objs, ","))
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "40")
		_, _ = fmt.Fprint(w, `{"items":[],"collections":[],"searches":[],"tags":[]}`)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func tailWindowClient(url string) *client.Client {
	c := client.New(&config.Config{BaseURL: url}, 5*time.Second, 0)
	c.NoCache = true
	return c
}

func constantVersion(v string) func(int, int) string {
	return func(int, int) string { return v }
}

// The Web API answers one page of at most 25 objects, and its
// Last-Modified-Version is the version of the whole library. A poll that
// emitted one response and saved that version skipped the other objects for
// good, on the baseline and on every later poll.
func TestEmitChanges_PagedWindowEmitsEveryObjectBeforeCursorMoves(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cursor int
	}{
		{name: "baseline", cursor: 0},
		{name: "incremental", cursor: 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := tailTestStore(t)
			srv := newTailWindowServer(t, 26, 25, constantVersion("40"), 0)
			c := tailWindowClient(srv.URL)
			if tc.cursor > 0 {
				if err := db.SaveLibraryVersion("tail:items", srv.URL, tc.cursor); err != nil {
					t.Fatalf("seeding cursor: %v", err)
				}
			}

			var buf bytes.Buffer
			n, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf, nil)
			if err != nil {
				t.Fatalf("emitChanges: %v", err)
			}
			if n != 26 {
				t.Fatalf("emitted %d events, want all 26 changed objects", n)
			}
			seen := map[string]bool{}
			for _, ev := range ndjsonEvents(t, buf.String()) {
				seen[fmt.Sprint(ev["key"])] = true
			}
			if len(seen) != 26 || !seen["K0000"] || !seen["K0025"] {
				t.Errorf("distinct keys emitted = %d (K0000=%v K0025=%v), want 26 including both pages", len(seen), seen["K0000"], seen["K0025"])
			}
			if v, src, _ := db.StoredLibraryVersionContext(context.Background(), "tail:items"); v != 40 || src != srv.URL {
				t.Errorf("cursor = (%d,%q), want (40,%q) once the whole window was emitted", v, src, srv.URL)
			}
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if len(srv.paths) != 2 || !strings.Contains(srv.paths[0], "start=0") || !strings.Contains(srv.paths[1], "start=25") {
				t.Errorf("page requests = %v, want one at start=0 and one at start=25", srv.paths)
			}
			for _, p := range srv.paths {
				if tc.cursor > 0 && !strings.Contains(p, "since=30") {
					t.Errorf("request %q lost the since filter", p)
				}
			}
		})
	}
}

// A later page that fails leaves the window incomplete: nothing may be
// emitted and the cursor must stay, so the whole window is retried.
func TestEmitChanges_FailedLaterPageEmitsNothingAndKeepsCursor(t *testing.T) {
	fastRetryBackoff(t)
	db := tailTestStore(t)
	srv := newTailWindowServer(t, 26, 25, constantVersion("40"), 25)
	c := tailWindowClient(srv.URL)
	if err := db.SaveLibraryVersion("tail:items", srv.URL, 30); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	var buf bytes.Buffer
	n, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf, nil)
	if err == nil {
		t.Fatal("emitChanges with a failing second page: want error, got nil")
	}
	if n != 0 || buf.Len() != 0 {
		t.Errorf("emitted %d events / %q, want none from an incomplete window", n, buf.String())
	}
	if v, _, _ := db.StoredLibraryVersionContext(context.Background(), "tail:items"); v != 30 {
		t.Errorf("cursor after a failed page = %d, want 30 (unchanged)", v)
	}
}

// Pages read at different library versions are not one snapshot: an edit can
// shift offsets so an object sits on no page. The walk restarts, and a window
// that never settles fails with the cursor held.
func TestEmitChanges_VersionDriftRestartsWindowThenHoldsCursor(t *testing.T) {
	t.Run("settles on retry", func(t *testing.T) {
		db := tailTestStore(t)
		// The library moves between the two pages of the first attempt only.
		srv := newTailWindowServer(t, 26, 25, func(start, request int) string {
			if request == 2 {
				return "41"
			}
			return "40"
		}, 0)
		c := tailWindowClient(srv.URL)
		if err := db.SaveLibraryVersion("tail:items", srv.URL, 30); err != nil {
			t.Fatalf("seeding cursor: %v", err)
		}

		var buf bytes.Buffer
		n, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf, nil)
		if err != nil {
			t.Fatalf("emitChanges: %v", err)
		}
		if n != 26 {
			t.Errorf("emitted %d events, want 26 from the settled window", n)
		}
		if v, _, _ := db.StoredLibraryVersionContext(context.Background(), "tail:items"); v != 40 {
			t.Errorf("cursor = %d, want 40 (the version the complete window was read at)", v)
		}
	})
	t.Run("never settles", func(t *testing.T) {
		db := tailTestStore(t)
		srv := newTailWindowServer(t, 26, 25, func(start, request int) string {
			return strconv.Itoa(40 + request)
		}, 0)
		c := tailWindowClient(srv.URL)
		if err := db.SaveLibraryVersion("tail:items", srv.URL, 30); err != nil {
			t.Fatalf("seeding cursor: %v", err)
		}

		var buf bytes.Buffer
		n, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf, nil)
		if err == nil {
			t.Fatal("emitChanges over a library that keeps changing: want error, got nil")
		}
		if n != 0 || buf.Len() != 0 {
			t.Errorf("emitted %d events / %q, want none", n, buf.String())
		}
		if v, _, _ := db.StoredLibraryVersionContext(context.Background(), "tail:items"); v != 30 {
			t.Errorf("cursor = %d, want 30 (unchanged)", v)
		}
	})
}
