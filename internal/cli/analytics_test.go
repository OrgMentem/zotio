// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
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
func TestAnalyticsTypeFiltersItemsAndRejectsUnknown(t *testing.T) {
	db := openAnalyticsTestStore(t)
	for key, raw := range map[string]string{
		"A1": `{"key":"A1","data":{"itemType":"journalArticle","date":"2020"}}`,
		"A2": `{"key":"A2","data":{"itemType":"journalArticle","date":"2021"}}`,
		"B1": `{"key":"B1","data":{"itemType":"book","date":"2020"}}`,
	} {
		if err := db.Upsert("items", key, json.RawMessage(raw)); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	scope, err := analyticsScopeForType(db, "journalArticle")
	if err != nil {
		t.Fatalf("journalArticle scope: %v", err)
	}
	if got, want := len(scope.rows), 2; got != want {
		t.Fatalf("journalArticle count = %d, want %d", got, want)
	}
	if scope.resourceType != "items" || scope.itemType != "journalArticle" {
		t.Fatalf("scope = %+v, want items filtered to journalArticle", scope)
	}
	if _, err := analyticsScopeForType(db, "bogusType"); err == nil || !strings.Contains(err.Error(), "unknown analytics type") {
		t.Fatalf("bogusType error = %v, want unknown analytics type", err)
	}
}

func TestAnalyticsGroupByCollectionHonorsLimit(t *testing.T) {
	db := openAnalyticsTestStore(t)
	seed := []string{
		`{"key":"C1","data":{"itemType":"journalArticle","collections":["COL-A","COL-B"]}}`,
		`{"key":"C2","data":{"itemType":"book","collections":["COL-A"]}}`,
	}
	for i, raw := range seed {
		if err := db.Upsert("items", string(rune('A'+i)), json.RawMessage(raw)); err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}
	var out bytes.Buffer
	if err := runGroupBy(&out, db, "items", "collection", 1, &rootFlags{asJSON: true}); err != nil {
		t.Fatalf("collection grouping: %v", err)
	}
	var got []groupByResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode grouping: %v", err)
	}
	if len(got) != 1 || got[0].Value != "COL-A" || got[0].Count != 2 {
		t.Fatalf("limited collection groups = %+v, want [{COL-A 2}]", got)
	}
}

func TestAnalyticsUnsupportedGroupByErrors(t *testing.T) {
	if err := validateAnalyticsGroupBy("nonsense"); err == nil {
		t.Fatal("unsupported group-by unexpectedly accepted")
	}
}
func TestAnalyticsCommandReportsFilteredItemTypeCount(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for key, raw := range map[string]string{
		"J1": `{"key":"J1","data":{"itemType":"journalArticle"}}`,
		"J2": `{"key":"J2","data":{"itemType":"journalArticle"}}`,
		"B1": `{"key":"B1","data":{"itemType":"book"}}`,
	} {
		if err := db.Upsert("items", key, json.RawMessage(raw)); err != nil {
			db.Close()
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	var out bytes.Buffer
	cmd := newAnalyticsCmd(&rootFlags{asJSON: true})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", dbPath, "--type", "journalArticle"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analytics command: %v", err)
	}
	var result struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode command result: %v (%s)", err, out.String())
	}
	if result.Count != 2 {
		t.Fatalf("journalArticle command count = %d, want 2", result.Count)
	}
}

func TestAnalyticsCommandRejectsInvalidLimit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	cmd := newAnalyticsCmd(&rootFlags{})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", dbPath, "--type", "items", "--limit", "0"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--limit must be greater than zero") {
		t.Fatalf("invalid limit error = %v", err)
	}
}

func TestAnalyticsCommandNonexistentDB_ReportsEmpty_Human(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	cmd := newAnalyticsCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--db", dbPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analytics error = %v; want nil empty result", err)
	}
	got := out.String()
	if !strings.Contains(got, "Resource Type") || !strings.Contains(got, "Count") {
		t.Fatalf("stdout = %q; want empty analytics table header", got)
	}
	if strings.Contains(errOut.String(), "opening local database") || strings.Contains(out.String(), "opening local database") {
		t.Fatalf("output must not contain store-open error on fresh install; stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestAnalyticsCommandNonexistentDB_ReportsEmpty_JSON(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	cmd := newAnalyticsCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--db", dbPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analytics --json error = %v; want nil", err)
	}
	var status map[string]int
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &status); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if len(status) != 0 {
		t.Fatalf("status = %v; want empty map", status)
	}
	if strings.Contains(errOut.String(), "opening local database") {
		t.Fatalf("stderr = %q; must not contain store-open error", errOut.String())
	}
}

func TestAnalyticsCommandNonexistentDB_TypeReportsEmpty_Human(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	cmd := newAnalyticsCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", dbPath, "--type", "items"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analytics --type items error = %v; want nil", err)
	}
	if want := "items: 0 records"; !strings.Contains(out.String(), want) {
		t.Fatalf("stdout = %q; want %q", out.String(), want)
	}
}

func TestAnalyticsCommandNonexistentDB_TypeReportsEmpty_JSON(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	cmd := newAnalyticsCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", dbPath, "--type", "journalArticle"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analytics --type journalArticle --json error = %v; want nil", err)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if got := result["count"]; got != float64(0) {
		t.Fatalf("count = %v; want 0", got)
	}
	if got := result["resource_type"]; got != "items" {
		t.Fatalf("resource_type = %v; want items", got)
	}
	if got := result["item_type"]; got != "journalArticle" {
		t.Fatalf("item_type = %v; want journalArticle", got)
	}
}

func TestAnalyticsCommandNonexistentDB_GroupByReportsEmpty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[asJSON], func(t *testing.T) {
			cmd := newAnalyticsCmd(&rootFlags{asJSON: asJSON})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"--db", dbPath, "--type", "items", "--group-by", "year"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("analytics --group-by error = %v; want nil", err)
			}
			if asJSON {
				var arr []any
				if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &arr); err != nil {
					t.Fatalf("decode %q: %v", out.String(), err)
				}
				if len(arr) != 0 {
					t.Fatalf("group-by JSON = %v; want empty array", arr)
				}
			} else {
				if !strings.Contains(out.String(), "year") {
					t.Fatalf("stdout = %q; want header containing group-by field", out.String())
				}
			}
		})
	}
}
