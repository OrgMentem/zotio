// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"zotio/internal/store"
)

// A full archive fetches to exhaustion. Delegating to sync inherited its
// default 100-page cap, so a library past 10,000 items failed every --full
// archive and restarted it from page zero on each retry.
func TestFxbWorkflowArchiveFullFetchesPastSyncPageCap(t *testing.T) {
	const fullPages = 101 // one past sync's default --max-pages
	const pageLimit = 100
	syncTestWithHumanFriendly(t, false)
	fastRetryBackoff(t)
	var itemRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/users/0/items" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		itemRequests.Add(1)
		start, err := strconv.Atoi(r.URL.Query().Get("start"))
		if err != nil {
			http.Error(w, "missing start offset", http.StatusBadRequest)
			return
		}
		n := pageLimit
		if start >= fullPages*pageLimit {
			n = 1 // the short page ends the walk
		}
		page := make([]json.RawMessage, n)
		for i := range page {
			key := fmt.Sprintf("K%07d", start+i)
			page[i] = json.RawMessage(fmt.Sprintf(`{"key":%q,"version":1,"data":{"key":%q,"version":1,"itemType":"book","title":"Item %s"}}`, key, key, key))
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	dbPath := filepath.Join(t.TempDir(), "archive.db")
	cmd := newWorkflowArchiveCmd(&rootFlags{asJSON: true, noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--full", "--db", dbPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workflow archive --full over %d pages: %v", fullPages+1, err)
	}
	if got := itemRequests.Load(); got != fullPages+1 {
		t.Fatalf("item page requests = %d, want %d", got, fullPages+1)
	}

	ctx := context.Background()
	db, err := store.OpenWithContext(ctx, dbPath)
	if err != nil {
		t.Fatalf("open archived store: %v", err)
	}
	defer db.Close()
	if n, err := db.CountContext(ctx, "items"); err != nil || n != fullPages*pageLimit+1 {
		t.Fatalf("archived items = %d, %v; want %d", n, err, fullPages*pageLimit+1)
	}
}
