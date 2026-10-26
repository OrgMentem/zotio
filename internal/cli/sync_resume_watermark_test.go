// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"zotio/internal/store"
)

// resumeWorld is a Web-API-shaped read plane for the resume tests: 101 items
// changed since checkpoint 50 (P000..P100), one more edit (of a page-one
// item) landing at library version 101, and the since=100 window that edit
// belongs to.
type resumeWorld struct {
	mu      sync.Mutex
	version string // Last-Modified-Version of every response
	edited  bool   // P005 was edited after the first page was read
	starts  []string
}

func resumeItemJSON(key string, version int, title string) string {
	return fmt.Sprintf(`{"key":%q,"version":%d,"data":{"key":%q,"version":%d,"itemType":"book","title":%q}}`, key, version, key, version, title)
}

func (w *resumeWorld) handler(t *testing.T) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		q := r.URL.Query()
		w.starts = append(w.starts, q.Get("start"))
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Last-Modified-Version", w.version)
		var rows []string
		total := 0
		switch {
		case q.Get("since") == "50" && q.Get("start") == "0":
			for i := range 100 {
				rows = append(rows, resumeItemJSON(fmt.Sprintf("P%03d", i), 60, "original"))
			}
			total = 101
		case q.Get("since") == "50" && q.Get("start") == "100":
			rows = []string{resumeItemJSON("P100", 60, "original")}
			total = 101
		case q.Get("since") == "100":
			if w.edited {
				rows = []string{resumeItemJSON("P005", 101, "edited")}
				total = 1
			}
		default:
			t.Errorf("unexpected request %s", r.URL.RequestURI())
			http.Error(rw, "unexpected", http.StatusBadRequest)
			return
		}
		rw.Header().Set("Total-Results", fmt.Sprint(total))
		_, _ = rw.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	}
}

// A resumed incremental pass checkpoints the library version of the pass's
// FIRST response, not of whichever response its suffix happens to read. Page
// one was read at version 100; an edit that lands at 101 while the pass is
// stopped changes an item in the already-read prefix. Taking 101 from the
// resumed suffix made the next incremental pass ask for since=101 and never
// see that edit.
func TestSyncResumePreservesFirstResponseVersion(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	world := &resumeWorld{version: "100"}
	srv := httptest.NewServer(world.handler(t))
	defer srv.Close()
	c := syncTestClient(srv.URL)

	dbPath := filepath.Join(t.TempDir(), "resume.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.SaveLibraryVersion("items", c.Plane(), 50); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}

	// Run 1 stops after the first page (--max-pages 1), mid-pass.
	if res := syncResource(context.Background(), c, db, "items", 0, false, 1, false); res.Err != nil {
		t.Fatalf("first run: %v", res.Err)
	}
	if cursor, _, _, _, err := db.GetSyncResumeState("items"); err != nil || cursor != "100" {
		t.Fatalf("resume cursor after the first page = %q (err %v), want 100", cursor, err)
	}
	if v, _, _ := db.StoredLibraryVersionContext(context.Background(), "items"); v != 50 {
		t.Fatalf("checkpoint advanced to %d on an unfinished pass, want 50", v)
	}

	// The process dies; a page-one item is edited while it is down.
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	world.mu.Lock()
	world.version, world.edited = "101", true
	world.mu.Unlock()
	db, err = store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db.Close()

	// Run 2 rejects offset 100 at version 101 and restarts with watermark 100.
	if res := syncResource(context.Background(), c, db, "items", 0, false, 0, false); res.Err != nil {
		t.Fatalf("resumed run: %v", res.Err)
	}
	if v, _, _ := db.StoredLibraryVersionContext(context.Background(), "items"); v != 100 {
		t.Fatalf("checkpoint after the resumed pass = %d, want 100 (the version the prefix was read at, not the suffix's 101)", v)
	}

	// Run 3 is the ordinary incremental pass: it must reach the edit.
	if res := syncResource(context.Background(), c, db, "items", 0, false, 0, false); res.Err != nil {
		t.Fatalf("follow-up run: %v", res.Err)
	}
	raw, err := db.Get("items", "P005")
	if err != nil {
		t.Fatalf("Get(P005): %v", err)
	}
	var obj struct {
		Data struct {
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode P005: %v", err)
	}
	if obj.Data.Title != "edited" {
		t.Errorf("P005 title = %q, want \"edited\": the edit to the already-read prefix never converged", obj.Data.Title)
	}
}

// A cursor saved before the watermark existed carries no pass version, so
// resuming it could checkpoint a version its prefix was never read at: it
// restarts from the committed checkpoint instead.
func TestSyncResumeDiscardsCursorWithoutWatermark(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	world := &resumeWorld{version: "100"}
	srv := httptest.NewServer(world.handler(t))
	defer srv.Close()
	c := syncTestClient(srv.URL)

	db := syncTestOpenStore(t)
	defer db.Close()
	if err := db.SaveLibraryVersion("items", c.Plane(), 50); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
	legacyScope := fmt.Sprintf(`{"plane":%q,"mode":"incremental","since":50}`, c.Plane())
	if err := db.SaveSyncResumeStateContext(context.Background(), "items", "100", legacyScope, 100); err != nil {
		t.Fatalf("seed legacy cursor: %v", err)
	}

	if res := syncResource(context.Background(), c, db, "items", 0, false, 0, false); res.Err != nil {
		t.Fatalf("sync: %v", res.Err)
	}
	world.mu.Lock()
	defer world.mu.Unlock()
	if len(world.starts) == 0 || world.starts[0] != "0" {
		t.Errorf("request offsets = %v, want the pass to restart at 0 instead of resuming a cursor with no watermark", world.starts)
	}
	if v, _, _ := db.StoredLibraryVersionContext(context.Background(), "items"); v != 100 {
		t.Errorf("checkpoint = %d, want 100", v)
	}
}
