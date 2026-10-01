// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"
)

// wwRunWorkflowArchive runs `workflow archive` against a fake read API that
// answers every GET with an empty page, except paths that failPath matches,
// which fail with 503.
func wwRunWorkflowArchive(t *testing.T, failPath string, args ...string) error {
	t.Helper()
	syncTestWithHumanFriendly(t, false)
	fastRetryBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failPath != "" && r.URL.Path == failPath {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newWorkflowArchiveCmd(&rootFlags{asJSON: true, noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	return cmd.Execute()
}

// A full archive is a full sync: a row the read plane no longer lists is
// reaped from the mirror. The separate archive loop never swept, so a
// deleted item outlived every archive.
func TestWorkflowArchiveFullReapsRowsDeletedUpstream(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	db, err := store.OpenWithContext(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	gone := json.RawMessage(`{"key":"GONE0001","version":1,"data":{"key":"GONE0001","version":1,"itemType":"book","title":"Deleted upstream"}}`)
	if err := db.UpsertContext(ctx, "items", "GONE0001", gone); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	if err := wwRunWorkflowArchive(t, "", "--full", "--db", dbPath); err != nil {
		t.Fatalf("workflow archive --full: %v", err)
	}

	db, err = store.OpenWithContext(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db.Close()
	if n, err := db.CountContext(ctx, "items"); err != nil || n != 0 {
		t.Fatalf("items after a full archive of an empty library = %d, %v; want 0", n, err)
	}
}

// Archive keeps its contract that a resource which fails to sync fails the
// run, even when the other resources sync: plain incremental sync exits 0
// there, so archive must run it with --strict.
func TestWorkflowArchiveFailsWhenAnyResourceFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	err := wwRunWorkflowArchive(t, "/users/0/items", "--db", dbPath)
	if err == nil {
		t.Fatal("workflow archive succeeded although the items resource failed")
	}
	if !strings.Contains(err.Error(), "items") {
		t.Fatalf("workflow archive error = %q, want it to name the failed items resource", err.Error())
	}
}
