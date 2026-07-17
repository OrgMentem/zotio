// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
	"zotio/internal/store"
)

func TestSyncFulltext_StoresAndIndexes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/fulltext", func(w http.ResponseWriter, r *http.Request) {
		if since := r.URL.Query().Get("since"); since != "0" {
			t.Errorf("/fulltext since=%q, want 0", since)
		}
		w.Header().Set("Last-Modified-Version", "3")
		_, _ = io.WriteString(w, `{"ATT1":3}`)
	})
	mux.HandleFunc("/items/ATT1/fulltext", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"content":"hello world","indexedChars":11,"totalChars":11}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	// full=true -> cursor 0, since=0 always sent.
	if err := syncFulltext(context.Background(), c, db, true); err != nil {
		t.Fatalf("syncFulltext: %v", err)
	}

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

func TestSyncFulltext_PreCanceledContextSkipsPerItemFetches(t *testing.T) {
	var indexRequests atomic.Int64
	var perItemRequests atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/fulltext", func(w http.ResponseWriter, r *http.Request) {
		indexRequests.Add(1)
		w.Header().Set("Last-Modified-Version", "9")
		_, _ = io.WriteString(w, `{"ATT1":3,"ATT2":4}`)
	})
	mux.HandleFunc("/items/ATT1/fulltext", func(w http.ResponseWriter, r *http.Request) {
		perItemRequests.Add(1)
		_, _ = io.WriteString(w, `{"content":"should not be fetched"}`)
	})
	mux.HandleFunc("/items/ATT2/fulltext", func(w http.ResponseWriter, r *http.Request) {
		perItemRequests.Add(1)
		_, _ = io.WriteString(w, `{"content":"should not be fetched"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := syncFulltext(ctx, c, db, true); err == nil {
		t.Fatal("syncFulltext with canceled context succeeded, want error")
	}

	if got := indexRequests.Load(); got != 1 {
		t.Fatalf("/fulltext index requests = %d, want 1 to exercise canceled per-item fanout", got)
	}
	if got := perItemRequests.Load(); got != 0 {
		t.Fatalf("per-item fulltext requests = %d, want 0 after pre-cancelled context", got)
	}
}

func TestSyncFulltext_FetchFailureRetainsCheckpointAndSuccessfulRows(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/fulltext", func(w http.ResponseWriter, r *http.Request) {
		if since := r.URL.Query().Get("since"); since != "4" {
			t.Errorf("/fulltext since=%q, want 4", since)
		}
		w.Header().Set("Last-Modified-Version", "9")
		_, _ = io.WriteString(w, `{"ATTOK":9,"ATTFAIL":9}`)
	})
	mux.HandleFunc("/items/ATTOK/fulltext", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"content":"stored despite peer failure"}`)
	})
	mux.HandleFunc("/items/ATTFAIL/fulltext", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary failure", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.SaveLibraryVersion("fulltext", 4); err != nil {
		t.Fatalf("seed fulltext checkpoint: %v", err)
	}

	if err := syncFulltext(context.Background(), c, db, false); err == nil {
		t.Fatal("syncFulltext succeeded despite attachment fetch failure")
	}
	if got, err := db.GetLibraryVersion("fulltext"); err != nil || got != 4 {
		t.Fatalf("fulltext checkpoint = %d, %v; want unchanged 4", got, err)
	}
	data, ok, err := db.Fulltext("ATTOK")
	if err != nil || !ok || len(data) == 0 {
		t.Fatalf("successful ATTOK row = %q, %t, %v; want stored row", data, ok, err)
	}
}

func TestSyncFulltext_PersistenceFailureRetainsCheckpoint(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/fulltext", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "9")
		_, _ = io.WriteString(w, `{"ATT1":9}`)
	})
	mux.HandleFunc("/items/ATT1/fulltext", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"content":"cannot persist"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.SaveLibraryVersion("fulltext", 4); err != nil {
		t.Fatalf("seed fulltext checkpoint: %v", err)
	}
	triggerDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open trigger connection: %v", err)
	}
	defer triggerDB.Close()
	if _, err := triggerDB.Exec(`
		CREATE TRIGGER reject_fulltext
		BEFORE INSERT ON resources
		WHEN NEW.resource_type = 'fulltext'
		BEGIN
			SELECT RAISE(ABORT, 'reject fulltext');
		END`); err != nil {
		t.Fatalf("create fulltext rejection trigger: %v", err)
	}

	if err := syncFulltext(context.Background(), c, db, false); err == nil {
		t.Fatal("syncFulltext succeeded despite persistence failure")
	}
	if got, err := db.GetLibraryVersion("fulltext"); err != nil || got != 4 {
		t.Fatalf("fulltext checkpoint = %d, %v; want unchanged 4", got, err)
	}
}
