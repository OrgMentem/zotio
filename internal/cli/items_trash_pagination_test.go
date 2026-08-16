// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Regression: zotio-89aa0ae — union must sort before paginating.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zotio/internal/store"
)

func trashResults(t *testing.T, cmdOut string) []map[string]any {
	t.Helper()
	var env struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(cmdOut), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", cmdOut, err)
	}
	return env.Results
}

func runTrashWithFlags(t *testing.T, flags *rootFlags, extraArgs ...string) (string, string) {
	t.Helper()
	cmd := newItemsTrashCmd(flags)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(extraArgs)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items trash: %v (stderr %s)", err, errOut.String())
	}
	return out.String(), errOut.String()
}

// `items trash --start N --limit M` returns the correctly ordered union page.
func TestItemsTrashUnionSortPaginates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Live returns 3 items already paginated by dateModified but not covering
	// the full candidate set; mirror adds a newer item that should sort first.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"key":"LIVE2","data":{"key":"LIVE2","itemType":"book","dateModified":"2026-07-02T00:00:00Z"}},
			{"key":"LIVE1","data":{"key":"LIVE1","itemType":"book","dateModified":"2026-07-01T00:00:00Z"}}
		]`))
	}))
	defer srv.Close()

	// Seed mirror with a newer trashed item and one that dedupes live.
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		json.RawMessage(`{"key":"MIRROR_NEW","data":{"key":"MIRROR_NEW","itemType":"book","dateModified":"2026-07-03T00:00:00Z"}}`),
		json.RawMessage(`{"key":"LIVE1","data":{"key":"LIVE1","itemType":"book","dateModified":"2026-07-01T00:00:00Z"}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Unified sorted order is MIRROR_NEW (07-03), LIVE2 (07-02), LIVE1 (07-01).
	// --start 1 --limit 1 should return LIVE2, not LIVE1 or MIRROR_NEW.
	flags := &rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		asJSON:     true,
	}
	out, _ := runTrashWithFlags(t, flags, "--start", "1", "--limit", "1")
	results := trashResults(t, out)
	if len(results) != 1 {
		t.Fatalf("results %v, want 1 page row", results)
	}
	if key, _ := results[0]["key"].(string); key != "LIVE2" {
		t.Fatalf("page key = %q, want LIVE2 (sorted union sliced by start/limit)", key)
	}
}

// Short live page is not padded with out-of-page mirror rows.
func TestItemsTrashUnionDoesNotPadShortPage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Live has one item that belongs at the end of the ordering; mirror has
	// two that sort before it. Full union order: NEWER, NEW, LIVE_OLD.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"LIVE_OLD","data":{"key":"LIVE_OLD","itemType":"book","dateModified":"2026-07-01T00:00:00Z"}}]`))
	}))
	defer srv.Close()

	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		json.RawMessage(`{"key":"NEW","data":{"key":"NEW","itemType":"book","dateModified":"2026-07-02T00:00:00Z"}}`),
		json.RawMessage(`{"key":"NEWER","data":{"key":"NEWER","itemType":"book","dateModified":"2026-07-03T00:00:00Z"}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	flags := &rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		asJSON:     true,
	}
	// --start 0 --limit 1 must return NEWER (the correct page), not LIVE_OLD
	// padded from a short terminal live page.
	out, _ := runTrashWithFlags(t, flags, "--limit", "1")
	results := trashResults(t, out)
	if len(results) != 1 || results[0]["key"] != "NEWER" {
		t.Fatalf("first page = %v, want NEWER", results)
	}
	// --start 2 --limit 10 must return LIVE_OLD alone, never an empty page or
	// mirror rows duplicated from outside the page.
	out, _ = runTrashWithFlags(t, flags, "--start", "2", "--limit", "10")
	results = trashResults(t, out)
	if len(results) != 1 || results[0]["key"] != "LIVE_OLD" {
		t.Fatalf("tail page = %v, want LIVE_OLD", results)
	}
}

// Missing dateModified sorts last without panicking; tie uses key.
func TestItemsTrashUnionMissingDateSortsLast(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"key":"HAS_DATE","data":{"key":"HAS_DATE","dateModified":"2026-07-02T00:00:00Z"}},
			{"key":"NO_DATE","data":{"key":"NO_DATE"}}
		]`))
	}))
	defer srv.Close()

	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Mirror has one unparseable and one valid date.
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		json.RawMessage(`{"key":"BAD_DATE","data":{"key":"BAD_DATE","dateModified":"not-a-date"}}`),
		json.RawMessage(`{"key":"MIRROR_NEW","data":{"key":"MIRROR_NEW","dateModified":"2026-07-03T00:00:00Z"}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	flags := &rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		asJSON:     true,
	}
	out, _ := runTrashWithFlags(t, flags)
	results := trashResults(t, out)
	if len(results) != 4 {
		t.Fatalf("results = %v, want 4", results)
	}
	// First is newest valid date, last two are missing/invalid sorted by key.
	if results[0]["key"] != "MIRROR_NEW" || results[1]["key"] != "HAS_DATE" {
		t.Fatalf("head order = %v, want MIRROR_NEW then HAS_DATE", results)
	}
	// Tail: BAD_DATE and NO_DATE, key tie-breaker puts BAD_DATE before NO_DATE.
	tail := []string{results[2]["key"].(string), results[3]["key"].(string)}
	if tail[0] != "BAD_DATE" || tail[1] != "NO_DATE" {
		t.Fatalf("tail = %v, want [BAD_DATE NO_DATE] (missing last, key tie-breaker)", tail)
	}
}

// Mirror-unavailable path still sorts and paginates without panic.
func TestItemsTrashUnionNoMirrorStillPaginates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// No store seeded — resolveLocal will error and the union falls back to
	// sorting and paginating the live set.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"key":"B","data":{"key":"B","dateModified":"2026-07-01T00:00:00Z"}},
			{"key":"A","data":{"key":"A","dateModified":"2026-07-02T00:00:00Z"}}
		]`))
	}))
	defer srv.Close()

	flags := &rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		asJSON:     true,
	}
	// Reverse input order; correct output is A then B by date desc.
	out, _ := runTrashWithFlags(t, flags)
	results := trashResults(t, out)
	if len(results) != 2 || results[0]["key"] != "A" || results[1]["key"] != "B" {
		t.Fatalf("sorted live = %v, want [A B] date desc", results)
	}
	out, _ = runTrashWithFlags(t, flags, "--start", "1", "--limit", "1")
	results = trashResults(t, out)
	if len(results) != 1 || results[0]["key"] != "B" {
		t.Fatalf("paged live = %v, want B", results)
	}
}
