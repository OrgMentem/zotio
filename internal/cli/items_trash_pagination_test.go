// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Regression: zotio-89aa0ae — union must sort before paginating.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

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

// liveTrashHandler returns a handler that honors start/limit like the real
// Zotero Web API: limit defaults to 25, caps at 100, start defaults to 0.
func liveTrashHandler(t *testing.T, items []json.RawMessage) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limitStr := q.Get("limit")
		startStr := q.Get("start")
		limit := 25 // Zotero default
		if limitStr != "" {
			if v, err := strconv.Atoi(limitStr); err == nil {
				limit = v
			}
		}
		if limit > 100 {
			limit = 100
		}
		if limit < 0 {
			limit = 0
		}
		start := 0
		if startStr != "" {
			if v, err := strconv.Atoi(startStr); err == nil {
				start = v
			}
		}
		if start < 0 {
			start = 0
		}
		var slice []json.RawMessage
		if start < len(items) {
			end := start + limit
			if limit == 0 {
				// limit 0 means unbounded in our CLI but real API would use 25;
				// however production always sends explicit 100, so treat 0 as len.
				end = len(items)
			}
			if end > len(items) {
				end = len(items)
			}
			slice = items[start:end]
		} else {
			slice = []json.RawMessage{}
		}
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.Marshal(slice)
		_, _ = w.Write(out)
	}
}

func mustTrashItem(key, date string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"key": key,
		"data": map[string]any{
			"key":          key,
			"itemType":     "book",
			"dateModified": date,
		},
	})
	return json.RawMessage(raw)
}

// `items trash --start N --limit M` returns the correctly ordered union page.
func TestItemsTrashUnionSortPaginates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	liveItems := []json.RawMessage{
		mustTrashItem("LIVE2", "2026-07-02T00:00:00Z"),
		mustTrashItem("LIVE1", "2026-07-01T00:00:00Z"),
	}
	srv := httptest.NewServer(liveTrashHandler(t, liveItems))
	defer srv.Close()

	// Seed mirror with a newer trashed item and one that dedupes live.
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		mustTrashItem("MIRROR_NEW", "2026-07-03T00:00:00Z"),
		mustTrashItem("LIVE1", "2026-07-01T00:00:00Z"),
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

	liveItems := []json.RawMessage{
		mustTrashItem("LIVE_OLD", "2026-07-01T00:00:00Z"),
	}
	srv := httptest.NewServer(liveTrashHandler(t, liveItems))
	defer srv.Close()

	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		mustTrashItem("NEW", "2026-07-02T00:00:00Z"),
		mustTrashItem("NEWER", "2026-07-03T00:00:00Z"),
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

	liveItems := []json.RawMessage{
		mustTrashItem("HAS_DATE", "2026-07-02T00:00:00Z"),
		json.RawMessage(`{"key":"NO_DATE","data":{"key":"NO_DATE"}}`),
	}
	srv := httptest.NewServer(liveTrashHandler(t, liveItems))
	defer srv.Close()

	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Mirror has one unparseable and one valid date.
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		json.RawMessage(`{"key":"BAD_DATE","data":{"key":"BAD_DATE","dateModified":"not-a-date"}}`),
		mustTrashItem("MIRROR_NEW", "2026-07-03T00:00:00Z"),
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
	liveItems := []json.RawMessage{
		mustTrashItem("B", "2026-07-01T00:00:00Z"),
		mustTrashItem("A", "2026-07-02T00:00:00Z"),
	}
	srv := httptest.NewServer(liveTrashHandler(t, liveItems))
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

// Multi-page: >25 live trash rows plus mirror-only rows. Verifies the live
// fetcher pages with limit=100 (not a single default-25 GET) and that
// --start/--limit paginate the complete sorted union with non-overlapping pages.
func TestItemsTrashUnionMultiPage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// 35 live rows: LIVE001 .. LIVE035 with distinct dates.
	const liveCount = 35
	liveItems := make([]json.RawMessage, liveCount)
	baseDate := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := range liveCount {
		key := fmt.Sprintf("LIVE%03d", i+1)
		date := baseDate.Add(time.Duration(i) * 24 * time.Hour).Format(time.RFC3339)
		liveItems[i] = mustTrashItem(key, date)
	}
	srv := httptest.NewServer(liveTrashHandler(t, liveItems))
	defer srv.Close()

	// 3 mirror-only rows: one newer than all live, one mid, one older.
	mirrorItems := []json.RawMessage{
		mustTrashItem("MIRROR_NEW", "2026-08-20T00:00:00Z"),
		mustTrashItem("MIRROR_MID", baseDate.Add(15*24*time.Hour).Format(time.RFC3339)),
		mustTrashItem("MIRROR_OLD", "2026-06-01T00:00:00Z"),
	}
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items-trash", mirrorItems); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	flags := &rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		asJSON:     true,
	}

	// Full union without pagination must contain every live row plus mirrors.
	out, _ := runTrashWithFlags(t, flags)
	results := trashResults(t, out)
	wantTotal := liveCount + len(mirrorItems) // all distinct
	if len(results) != wantTotal {
		t.Fatalf("full union len %d, want %d", len(results), wantTotal)
	}
	// Verify every live key is present.
	seen := make(map[string]bool, len(results))
	for _, r := range results {
		if k, _ := r["key"].(string); k != "" {
			seen[k] = true
		}
	}
	for _, raw := range liveItems {
		k := jsonStringField(raw, "key")
		if !seen[k] {
			t.Fatalf("missing live key %q in union", k)
		}
	}
	// Verify sorted descending by dateModified (missing last, not expected here).
	// Use same logic as production to avoid brittle string checks.
	for i := 1; i < len(results); i++ {
		prevDate := results[i-1]["data"].(map[string]any)["dateModified"]
		currDate := results[i]["data"].(map[string]any)["dateModified"]
		// Parse; mirror dates are valid.
		prevT, _ := time.Parse(time.RFC3339, prevDate.(string))
		currT, _ := time.Parse(time.RFC3339, currDate.(string))
		if currT.After(prevT) {
			t.Fatalf("not sorted desc at %d: %q (%v) before %q (%v)", i, results[i-1]["key"], prevT, results[i]["key"], currT)
		}
		if currT.Equal(prevT) {
			ki := results[i-1]["key"].(string)
			kj := results[i]["key"].(string)
			if ki > kj {
				t.Fatalf("tie-breaker violated at %d: %q > %q with equal date %v", i, ki, kj, prevT)
			}
		}
	}

	// Pagination must return correctly ordered, non-overlapping pages across the full set.
	// Re-derive expected order by sorting a copy with production comparator.
	var all []json.RawMessage
	all = append(all, liveItems...)
	all = append(all, mirrorItems...)
	all = sortTrashItems(append([]json.RawMessage(nil), all...))
	// Helper to extract keys from sorted raws.
	sortedKeys := make([]string, len(all))
	for i, r := range all {
		sortedKeys[i] = jsonStringField(r, "key")
	}

	// Page 0: start 0 limit 10
	out, _ = runTrashWithFlags(t, flags, "--start", "0", "--limit", "10")
	page0 := trashResults(t, out)
	if len(page0) != 10 {
		t.Fatalf("page0 len %d, want 10", len(page0))
	}
	for i, r := range page0 {
		if k := r["key"].(string); k != sortedKeys[i] {
			t.Fatalf("page0[%d] = %q, want %q", i, k, sortedKeys[i])
		}
	}
	// Page 1: start 10 limit 10
	out, _ = runTrashWithFlags(t, flags, "--start", "10", "--limit", "10")
	page1 := trashResults(t, out)
	if len(page1) != 10 {
		t.Fatalf("page1 len %d, want 10", len(page1))
	}
	for i, r := range page1 {
		if k := r["key"].(string); k != sortedKeys[10+i] {
			t.Fatalf("page1[%d] = %q, want %q", i, k, sortedKeys[10+i])
		}
	}
	// Overlap check.
	overlap := make(map[string]bool, 10)
	for _, r := range page0 {
		overlap[r["key"].(string)] = true
	}
	for _, r := range page1 {
		if overlap[r["key"].(string)] {
			t.Fatalf("overlap key %q in both pages", r["key"])
		}
	}
	// Tail page: start beyond first 20, verify remainder.
	out, _ = runTrashWithFlags(t, flags, "--start", "30", "--limit", "10")
	tail := trashResults(t, out)
	wantTail := sortedKeys[30:]
	if len(wantTail) > 10 {
		wantTail = wantTail[:10]
	}
	if len(tail) != len(wantTail) {
		t.Fatalf("tail len %d, want %d", len(tail), len(wantTail))
	}
	for i, r := range tail {
		if k := r["key"].(string); k != wantTail[i] {
			t.Fatalf("tail[%d] = %q, want %q", i, k, wantTail[i])
		}
	}
	// Short page beyond end must not pad with out-of-page mirror rows (already verified by sorting).
	out, _ = runTrashWithFlags(t, flags, "--start", "100", "--limit", "10")
	empty := trashResults(t, out)
	if len(empty) != 0 {
		t.Fatalf("beyond-end page = %v, want empty", empty)
	}
}
