// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase6 d27f99d4): cover resumable header-free paginated snapshot exports.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
)

type exportPaginateRequest struct {
	Start int
	Limit int
}

func newExportPaginateTestClient(t *testing.T, total int, requests *[]exportPaginateRequest) *httptest.Server {
	t.Helper()
	items := make([]json.RawMessage, total)
	for i := range items {
		items[i] = json.RawMessage(fmt.Sprintf(`{"key":"K%d","version":%d}`, i+1, i+1))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/items" {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %q", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}

		start, err := strconv.Atoi(r.URL.Query().Get("start"))
		if err != nil {
			t.Errorf("bad start query %q: %v", r.URL.Query().Get("start"), err)
			http.Error(w, "bad start", http.StatusBadRequest)
			return
		}
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil {
			t.Errorf("bad limit query %q: %v", r.URL.Query().Get("limit"), err)
			http.Error(w, "bad limit", http.StatusBadRequest)
			return
		}
		*requests = append(*requests, exportPaginateRequest{Start: start, Limit: limit})

		end := start + limit
		if start > len(items) {
			start = len(items)
		}
		if end > len(items) {
			end = len(items)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(items[start:end]); err != nil {
			t.Errorf("encode page: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResumablePaginatedFetchFullFetch(t *testing.T) {
	var requests []exportPaginateRequest
	srv := newExportPaginateTestClient(t, 250, &requests)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	flags := &rootFlags{}
	c, err := flags.newClient()
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	var got []json.RawMessage
	fetched, err := resumablePaginatedFetch(context.Background(), c, "/items", nil, 100, 0, "", func(page []json.RawMessage) error {
		got = append(got, page...)
		return nil
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if fetched != 250 {
		t.Fatalf("fetched = %d, want 250", fetched)
	}
	if len(got) != 250 {
		t.Fatalf("collected = %d, want 250", len(got))
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %+v, want 3 pages", requests)
	}
	want := []exportPaginateRequest{{Start: 0, Limit: 100}, {Start: 100, Limit: 100}, {Start: 200, Limit: 100}}
	for i := range want {
		if requests[i] != want[i] {
			t.Fatalf("request %d = %+v, want %+v", i, requests[i], want[i])
		}
	}

	var first struct {
		Key     string `json:"key"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(got[0], &first); err != nil {
		t.Fatalf("decode first item: %v", err)
	}
	if first.Key != "K1" || first.Version != 1 {
		t.Fatalf("first item = %+v, want K1/version 1", first)
	}
}

func TestResumablePaginatedFetchResumesFromCheckpoint(t *testing.T) {
	var requests []exportPaginateRequest
	srv := newExportPaginateTestClient(t, 250, &requests)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	flags := &rootFlags{}
	c, err := flags.newClient()
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	checkpointFile := filepath.Join(t.TempDir(), "export.checkpoint.json")
	if err := writeExportCheckpoint(checkpointFile, exportCheckpoint{Path: "/items", NextStart: 200, Fetched: 200}); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}

	var got []json.RawMessage
	fetched, err := resumablePaginatedFetch(context.Background(), c, "/items", map[string]string{"format": "json"}, 100, 0, checkpointFile, func(page []json.RawMessage) error {
		got = append(got, page...)
		return nil
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if fetched != 250 {
		t.Fatalf("fetched = %d, want 250", fetched)
	}
	if len(got) != 50 {
		t.Fatalf("collected = %d, want final 50", len(got))
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %+v, want one resumed page", requests)
	}
	if requests[0] != (exportPaginateRequest{Start: 200, Limit: 100}) {
		t.Fatalf("request = %+v, want start 200 limit 100", requests[0])
	}

	cp, ok := readExportCheckpoint(checkpointFile)
	if !ok {
		t.Fatalf("checkpoint not readable after fetch")
	}
	if cp != (exportCheckpoint{Path: "/items", NextStart: 250, Fetched: 250, Done: true}) {
		t.Fatalf("checkpoint = %+v, want done at 250", cp)
	}
	assertFileMode(t, checkpointFile, 0o600)
}

func TestReadExportCheckpointMissingFile(t *testing.T) {
	if cp, ok := readExportCheckpoint(filepath.Join(t.TempDir(), "missing.json")); ok {
		t.Fatalf("readExportCheckpoint returned ok=true with %+v for missing file", cp)
	}
}
