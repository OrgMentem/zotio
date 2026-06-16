// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean hhup): cover the opt-in PDF full-text sync pass.

package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"zotero-pp-cli/internal/client"
	"zotero-pp-cli/internal/config"
	"zotero-pp-cli/internal/store"
)

func TestSyncFulltext_StoresAndIndexes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/fulltext", func(w http.ResponseWriter, r *http.Request) {
		if since := r.URL.Query().Get("since"); since != "0" {
			t.Errorf("/fulltext since=%q, want 0", since)
		}
		w.Header().Set("Last-Modified-Version", "3")
		io.WriteString(w, `{"ATT1":3}`)
	})
	mux.HandleFunc("/items/ATT1/fulltext", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"content":"hello world","indexedChars":11,"totalChars":11}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	db, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	// full=true -> cursor 0, since=0 always sent.
	syncFulltext(c, db, true)

	data, ok, err := db.Fulltext("ATT1")
	if err != nil {
		t.Fatalf("Fulltext: %v", err)
	}
	if !ok {
		t.Fatal("Fulltext(ATT1) not stored")
	}
	if len(data) == 0 {
		t.Fatal("Fulltext(ATT1) empty")
	}

	// The stored full text is FTS-indexed via the shared resources_fts path.
	hits, err := db.Search("hello", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Error("Search(hello) found no full-text rows")
	}

	if v, _ := db.GetLibraryVersion("fulltext"); v != 3 {
		t.Errorf("fulltext cursor = %d, want 3", v)
	}
}
