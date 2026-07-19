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
	"strings"
	"testing"
	"zotio/internal/store"
)

func TestWorkflowArchive_FetchFailureRetainsCursorAndReportsIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resource := strings.TrimPrefix(r.URL.Path, "/users/0/")
		if resource == "items" {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "archive.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.SaveSyncState("items", "PREVIOUS", 3); err != nil {
		_ = db.Close()
		t.Fatalf("seed sync state: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newWorkflowArchiveCmd(&rootFlags{asJSON: true, noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--db", dbPath})

	err = cmd.Execute()
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("archive error = %v, exit=%d; want degraded non-zero error", err, ExitCode(err))
	}
	var result struct {
		Status   string   `json:"status"`
		Failures []string `json:"failures"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode archive result %q: %v", out.String(), err)
	}
	if result.Status != "incomplete" || len(result.Failures) == 0 {
		t.Fatalf("archive result = %+v, want incomplete result with failures", result)
	}
	if strings.Contains(out.String(), "Archived ") {
		t.Fatalf("archive reported success after failure: %s", out.String())
	}

	db, err = store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db.Close()
	cursor, _, _, err := db.GetSyncState("items")
	if err != nil || cursor != "PREVIOUS" {
		t.Fatalf("items cursor = %q, %v; want unchanged PREVIOUS", cursor, err)
	}
}

func TestWorkflowArchivePaginatesWithStartAndUsesZoteroKeys(t *testing.T) {
	var itemStarts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resource := strings.TrimPrefix(r.URL.Path, "/users/0/")
		if resource != "items" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		start := r.URL.Query().Get("start")
		itemStarts = append(itemStarts, start)
		switch start {
		case "0":
			items := make([]map[string]any, 0, 100)
			for i := range 100 {
				items = append(items, map[string]any{
					"key":  fmt.Sprintf("ITEM-%03d", i),
					"data": map[string]any{"itemType": "book", "title": fmt.Sprintf("Item %d", i)},
				})
			}
			_ = json.NewEncoder(w).Encode(items)
		case "100":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"key":  "ITEM-100",
				"data": map[string]any{"itemType": "book", "title": "Item 100"},
			}})
		default:
			http.Error(w, "unexpected pagination offset", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "archive.db")
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newWorkflowArchiveCmd(&rootFlags{asJSON: true, noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--db", dbPath, "--full"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive: %v; stderr=%s", err, errOut.String())
	}
	if got, want := strings.Join(itemStarts, ","), "0,100"; got != want {
		t.Fatalf("items pagination starts = %q, want %q", got, want)
	}

	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open archive store: %v", err)
	}
	defer db.Close()
	if item, err := db.Get("items", "ITEM-100"); err != nil || item == nil {
		t.Fatalf("get item by Zotero key: item=%s, err=%v", item, err)
	}
	if item, err := db.Get("items", "items-100"); err != nil {
		t.Fatalf("get synthetic item ID: %v", err)
	} else if item != nil {
		t.Fatal("archive stored a synthetic item ID")
	}
}
