// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

// openAnalyticsTestStore returns a fresh temp store for group-by tests.
func openAnalyticsTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// groupByResult mirrors the JSON shape runGroupBy emits (kv.Key/kv.Count).
type groupByResult struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

func runGroupByJSON(t *testing.T, db *store.Store, resourceType, field string) []groupByResult {
	t.Helper()
	var buf bytes.Buffer
	flags := &rootFlags{asJSON: true}
	if err := runGroupBy(&buf, db, resourceType, field, 0, flags); err != nil {
		t.Fatalf("runGroupBy: %v", err)
	}
	var results []groupByResult
	if err := json.Unmarshal(buf.Bytes(), &results); err != nil {
		t.Fatalf("decode group-by output: %v (raw: %s)", err, buf.String())
	}
	return results
}

func countsByValue(results []groupByResult) map[string]int {
	m := make(map[string]int, len(results))
	for _, r := range results {
		m[r.Value] = r.Count
	}
	return m
}

// TestRunGroupByEnvelopeShape reproduces the real synced Zotero resource
// shape, where bibliographic fields live under "data". Grouping by a
// data-nested field must produce the actual per-value distribution, not a
// single bucket keyed on the literal string "<nil>" from obj[field] missing
// at the top level.
func TestRunGroupByEnvelopeShape(t *testing.T) {
	db := openAnalyticsTestStore(t)

	seed := []struct {
		key string
		raw string
	}{
		{"AAAA1111", `{"key":"AAAA1111","version":1,"data":{"itemType":"journalArticle","title":"Paper One"}}`},
		{"BBBB2222", `{"key":"BBBB2222","version":1,"data":{"itemType":"journalArticle","title":"Paper Two"}}`},
		{"CCCC3333", `{"key":"CCCC3333","version":1,"data":{"itemType":"book","title":"Book One"}}`},
	}
	for _, s := range seed {
		if err := db.Upsert("items", s.key, json.RawMessage(s.raw)); err != nil {
			t.Fatalf("seed %s: %v", s.key, err)
		}
	}

	results := runGroupByJSON(t, db, "items", "itemType")
	counts := countsByValue(results)

	if counts["<nil>"] != 0 {
		t.Fatalf("got <nil> bucket with count %d, want no such bucket", counts["<nil>"])
	}
	if got, want := counts["journalArticle"], 2; got != want {
		t.Fatalf("journalArticle count = %v, want %v", got, want)
	}
	if got, want := counts["book"], 1; got != want {
		t.Fatalf("book count = %v, want %v", got, want)
	}
	if len(results) != 2 {
		t.Fatalf("got %d buckets, want 2 (journalArticle, book)", len(results))
	}
}

// TestRunGroupByFlatFallback proves the top-level fallback still works for
// non-envelope (flat) stored objects, e.g. legacy or non-Zotero resources.
func TestRunGroupByFlatFallback(t *testing.T) {
	db := openAnalyticsTestStore(t)

	if err := db.Upsert("items", "F1", json.RawMessage(`{"status":"open"}`)); err != nil {
		t.Fatalf("seed F1: %v", err)
	}
	if err := db.Upsert("items", "F2", json.RawMessage(`{"status":"open"}`)); err != nil {
		t.Fatalf("seed F2: %v", err)
	}
	if err := db.Upsert("items", "F3", json.RawMessage(`{"status":"closed"}`)); err != nil {
		t.Fatalf("seed F3: %v", err)
	}

	results := runGroupByJSON(t, db, "items", "status")
	counts := countsByValue(results)

	if got, want := counts["open"], 2; got != want {
		t.Fatalf("open count = %v, want %v", got, want)
	}
	if got, want := counts["closed"], 1; got != want {
		t.Fatalf("closed count = %v, want %v", got, want)
	}
}

// TestRunGroupByMissingFieldSentinel proves a resource missing the requested
// field at both the nested and top level buckets under the explicit
// "(unset)" sentinel, never Go's "<nil>" formatting artifact.
func TestRunGroupByMissingFieldSentinel(t *testing.T) {
	db := openAnalyticsTestStore(t)

	if err := db.Upsert("items", "M1", json.RawMessage(`{"key":"M1","data":{"itemType":"note"}}`)); err != nil {
		t.Fatalf("seed M1: %v", err)
	}

	results := runGroupByJSON(t, db, "items", "publisher")
	counts := countsByValue(results)

	if got, want := counts[groupByUnsetSentinel], 1; got != want {
		t.Fatalf("%s count = %v, want %v", groupByUnsetSentinel, got, want)
	}
	if _, present := counts["<nil>"]; present {
		t.Fatalf("got a literal <nil> bucket, want missing values under %q", groupByUnsetSentinel)
	}
}

// TestRunGroupByNonMapDataFallsBack proves a "data" key holding a non-map
// value (string, number, etc.) is treated as absent nesting rather than
// panicking, falling back to the top-level field.
func TestRunGroupByNonMapDataFallsBack(t *testing.T) {
	db := openAnalyticsTestStore(t)

	if err := db.Upsert("items", "N1", json.RawMessage(`{"data":"not-an-object","itemType":"journalArticle"}`)); err != nil {
		t.Fatalf("seed N1: %v", err)
	}

	results := runGroupByJSON(t, db, "items", "itemType")
	counts := countsByValue(results)

	if got, want := counts["journalArticle"], 1; got != want {
		t.Fatalf("journalArticle count = %v, want %v", got, want)
	}
}
