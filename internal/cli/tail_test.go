// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

// malformed body must not advance cursor: the transient 2xx should be retried
// on the next poll rather than permanently skipped.
func TestEmitChanges_MalformedPageDoesNotAdvanceCursor(t *testing.T) {
	db := tailTestStore(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got != "10" {
			t.Errorf("since = %q, want 10", got)
		}
		w.Header().Set("Last-Modified-Version", "11")
		_, _ = io.WriteString(w, "not-json")
	})
	// /deleted must not be reached because emitChanges returns before it.
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		t.Error("/deleted should not be fetched when page is malformed")
		w.Header().Set("Last-Modified-Version", "11")
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	if err := db.SaveLibraryVersion("tail:items", srv.URL, 10); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	var buf bytes.Buffer
	_, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err == nil {
		t.Fatal("emitChanges with malformed body: want error, got nil")
	}
	if !strings.Contains(err.Error(), "decoding change page") {
		t.Errorf("error = %q, want to mention decoding change page", err.Error())
	}
	if buf.Len() != 0 {
		t.Errorf("writer got %q, want empty on malformed page", buf.String())
	}
	// Cursor must remain at 10; advancing to 11 would skip the unseen changes.
	if v, src, _ := db.StoredLibraryVersion("tail:items"); v != 10 || src != srv.URL {
		t.Errorf("cursor after malformed page = (%d,%q), want (10,%q)", v, src, srv.URL)
	}
}

// Singleton envelope is valid JSON but never a tail page; treat as decoding
// failure and do not advance.
func TestEmitChanges_SingletonEnvelopeDoesNotAdvanceCursor(t *testing.T) {
	db := tailTestStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "5")
		_, _ = io.WriteString(w, `{"key":"ABC123","version":1,"data":{"title":"hi"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true

	var buf bytes.Buffer
	_, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err == nil {
		t.Fatal("singleton envelope: want error, got nil")
	}
	if v, _, _ := db.StoredLibraryVersion("tail:items"); v != 0 {
		t.Errorf("cursor after singleton = %d, want 0 (unchanged)", v)
	}
}

// Empty page is the legitimate "no changes" signal and must advance the cursor
// (the invariant from the task: gate on err==nil && isPage, not on emitted>0).
func TestEmitChanges_EmptyPageAdvancesCursor(t *testing.T) {
	db := tailTestStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `{"items":[],"collections":[],"searches":[],"tags":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true

	var buf bytes.Buffer
	n, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err != nil {
		t.Fatalf("empty page: %v", err)
	}
	if n != 0 {
		t.Errorf("empty page emitted %d, want 0", n)
	}
	if buf.Len() != 0 {
		t.Errorf("empty page wrote %q, want empty", buf.String())
	}
	if v, src, _ := db.StoredLibraryVersion("tail:items"); v != 7 || src != srv.URL {
		t.Errorf("cursor after empty page = (%d,%q), want (7,%q)", v, src, srv.URL)
	}
}

// Malformed /deleted payload must surface as error and not advance the cursor
// past unseen deletions.
func TestEmitChanges_MalformedDeletionsDoesNotAdvanceCursor(t *testing.T) {
	db := tailTestStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "12")
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "12")
		_, _ = io.WriteString(w, `not-json`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true

	// Seed after srv is known so the source matches.
	if err := db.SaveLibraryVersion("tail:items", srv.URL, 10); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	var buf bytes.Buffer
	_, err := emitChanges(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &buf)
	if err == nil {
		t.Fatal("malformed deletions: want error, got nil")
	}
	if !strings.Contains(err.Error(), "decoding deletions") {
		t.Errorf("error = %q, want to mention decoding deletions", err.Error())
	}
	if v, src, _ := db.StoredLibraryVersion("tail:items"); v != 10 || src != srv.URL {
		t.Errorf("cursor after malformed deletions = (%d,%q), want (10,%q)", v, src, srv.URL)
	}
}

// --follow=false with cmd.SetOut must capture NDJSON on the Cobra writer (not
// os.Stdout), exercising both call sites now routed through cmd.OutOrStdout().
func TestTail_FollowFalseWritesToCobraOut(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "1")
		_, _ = io.WriteString(w, `[{"key":"X","version":1,"data":{"title":"hello"}}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })

	cmd := RootCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"tail", "items", "--follow=false", "--db", filepath.Join(t.TempDir(), "tail.db")})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("tail --follow=false: %v (stderr %q)", err, errOut.String())
	}
	events := ndjsonEvents(t, out.String())
	if len(events) != 1 {
		t.Fatalf("want 1 event in Cobra Out, got %d: %q", len(events), out.String())
	}
	if events[0]["event"] != "upsert" || events[0]["key"] != "X" {
		t.Errorf("event = %v, want upsert/X", events[0])
	}
}
