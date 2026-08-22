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

// groupByResult mirrors the JSON shape analytics grouping emits (value/count).
type groupByResult struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

func runGroupByJSON(t *testing.T, db *store.Store, resourceType, field string) []groupByResult {
	t.Helper()
	rows, err := analyticsGroupRows(db, resourceType, field, 0)
	if err != nil {
		t.Fatalf("analyticsGroupRows: %v", err)
	}
	results := make([]groupByResult, 0, len(rows))
	for _, row := range rows {
		var r groupByResult
		if v, ok := row["value"].(string); ok {
			r.Value = v
		}
		switch c := row["count"].(type) {
		case int:
			r.Count = c
		case int64:
			r.Count = int(c)
		case float64:
			r.Count = int(c)
		case json.Number:
			if n, err := c.Int64(); err == nil {
				r.Count = int(n)
			}
		}
		results = append(results, r)
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
	rows, err := analyticsGroupRows(db, "items", "collection", 1)
	if err != nil {
		t.Fatalf("collection grouping: %v", err)
	}
	var got []groupByResult
	for _, row := range rows {
		var r groupByResult
		if v, ok := row["value"].(string); ok {
			r.Value = v
		}
		switch c := row["count"].(type) {
		case int:
			r.Count = c
		case int64:
			r.Count = int(c)
		case float64:
			r.Count = int(c)
		}
		got = append(got, r)
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

// analyticsEnvelope mirrors the shared {meta, results} shape analytics answers
// in JSON mode. Results is always an array of row objects carrying a count.
type analyticsEnvelope struct {
	Results []map[string]any `json:"results"`
	Meta    struct {
		Source       string `json:"source"`
		Reason       string `json:"reason,omitempty"`
		ResourceType string `json:"resource_type,omitempty"`
		GroupBy      string `json:"group_by,omitempty"`
	} `json:"meta"`
}

func analyticsDecodeEnvelope(t *testing.T, out []byte) analyticsEnvelope {
	t.Helper()
	var env analyticsEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("decode envelope %s: %v", string(out), err)
	}
	if env.Results == nil {
		t.Fatalf("envelope results is nil, want non-nil slice (empty array, not null): %s", string(out))
	}
	return env
}

func analyticsIntFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
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
	env := analyticsDecodeEnvelope(t, out.Bytes())
	if len(env.Results) != 1 {
		t.Fatalf("results len = %d, want 1: %s", len(env.Results), out.String())
	}
	row := env.Results[0]
	if got := analyticsIntFromAny(row["count"]); got != 2 {
		t.Fatalf("count = %v, want 2: %+v", row["count"], row)
	}
	if got, want := row["resource_type"], "items"; got != want {
		t.Fatalf("resource_type = %v, want %v: %+v", got, want, row)
	}
	if got, want := row["item_type"], "journalArticle"; got != want {
		t.Fatalf("item_type = %v, want %v: %+v", got, want, row)
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
	env := analyticsDecodeEnvelope(t, bytes.TrimSpace(out.Bytes()))
	if len(env.Results) != 0 {
		t.Fatalf("results = %v; want empty array", env.Results)
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
	env := analyticsDecodeEnvelope(t, bytes.TrimSpace(out.Bytes()))
	if len(env.Results) != 0 {
		t.Fatalf("results = %v; want empty array", env.Results)
	}
	if strings.Contains(errOut.String(), "opening local database") {
		t.Fatalf("stderr = %q; must not contain store-open error", errOut.String())
	}
}

func TestAnalyticsCommandNonexistentDB_TypeReportsEmpty_Human(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	cmd := newAnalyticsCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--db", dbPath, "--type", "items"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analytics --type items error = %v; want nil", err)
	}
	env := analyticsDecodeEnvelope(t, bytes.TrimSpace(out.Bytes()))
	if len(env.Results) != 1 {
		t.Fatalf("results len = %d, want 1 zero row: %s", len(env.Results), out.String())
	}
	row := env.Results[0]
	if got := analyticsIntFromAny(row["count"]); got != 0 {
		t.Fatalf("count = %v; want 0", row["count"])
	}
	if got := row["resource_type"]; got != "items" {
		t.Fatalf("resource_type = %v; want items", got)
	}
	if strings.Contains(errOut.String(), "opening local database") || strings.Contains(out.String(), "opening local database") {
		t.Fatalf("output must not contain store-open error on fresh install; stdout=%q stderr=%q", out.String(), errOut.String())
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
	env := analyticsDecodeEnvelope(t, out.Bytes())
	if len(env.Results) != 1 {
		t.Fatalf("results len = %d, want 1: %s", len(env.Results), out.String())
	}
	row := env.Results[0]
	if got := analyticsIntFromAny(row["count"]); got != 0 {
		t.Fatalf("count = %v; want 0", got)
	}
	if got := row["resource_type"]; got != "items" {
		t.Fatalf("resource_type = %v; want items", got)
	}
	if got := row["item_type"]; got != "journalArticle" {
		t.Fatalf("item_type = %v; want journalArticle", got)
	}
}

func TestAnalyticsCommandNonexistentDB_GroupByReportsEmpty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	t.Run("json", func(t *testing.T) {
		cmd := newAnalyticsCmd(&rootFlags{asJSON: true})
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--db", dbPath, "--type", "items", "--group-by", "year"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("analytics --group-by error = %v; want nil", err)
		}
		env := analyticsDecodeEnvelope(t, bytes.TrimSpace(out.Bytes()))
		if len(env.Results) != 0 {
			t.Fatalf("group-by JSON results = %v; want empty array", env.Results)
		}
	})
	t.Run("piped_default_is_json", func(t *testing.T) {
		cmd := newAnalyticsCmd(&rootFlags{})
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--db", dbPath, "--type", "items", "--group-by", "year"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("analytics --group-by error = %v; want nil", err)
		}
		env := analyticsDecodeEnvelope(t, bytes.TrimSpace(out.Bytes()))
		if len(env.Results) != 0 {
			t.Fatalf("piped default group-by results = %v; want empty array", env.Results)
		}
	})
}

// New tests covering the migrated envelope shapes.

func TestAnalyticsCommandBreakdownReturnsResourceTypeRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Seed two resource kinds with different counts so sort order is observable.
	for i := 0; i < 3; i++ {
		key := filepath.Base(t.TempDir()) + "_i" + string(rune('0'+i))
		// Use distinct keys; content does not matter for Status().
		if err := db.Upsert("items", key, json.RawMessage(`{"key":"`+key+`","data":{"itemType":"journalArticle"}}`)); err != nil {
			db.Close()
			t.Fatalf("seed items %s: %v", key, err)
		}
	}
	if err := db.Upsert("collections", "COL1", json.RawMessage(`{"key":"COL1","data":{"name":"C1"}}`)); err != nil {
		db.Close()
		t.Fatalf("seed collections: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	var out bytes.Buffer
	cmd := newAnalyticsCmd(&rootFlags{asJSON: true})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", dbPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analytics no-flag: %v", err)
	}
	env := analyticsDecodeEnvelope(t, out.Bytes())
	if len(env.Results) < 2 {
		t.Fatalf("results len = %d, want >=2: %s", len(env.Results), out.String())
	}
	// Every row must carry resource_type and count.
	for _, row := range env.Results {
		if _, ok := row["resource_type"]; !ok {
			t.Fatalf("row missing resource_type: %+v", row)
		}
		if _, ok := row["count"]; !ok {
			t.Fatalf("row missing count: %+v", row)
		}
	}
	// Sorted by count descending, then resource_type ascending.
	for i := 1; i < len(env.Results); i++ {
		prev := analyticsIntFromAny(env.Results[i-1]["count"])
		curr := analyticsIntFromAny(env.Results[i]["count"])
		if prev < curr {
			t.Fatalf("results not sorted by count descending: %+v before %+v", env.Results[i-1], env.Results[i])
		}
		if prev == curr {
			a := env.Results[i-1]["resource_type"].(string)
			b := env.Results[i]["resource_type"].(string)
			if a > b {
				t.Fatalf("results with equal count not sorted by resource_type ascending: %q before %q", a, b)
			}
		}
	}
	// The top row should be the kind with highest count (items).
	if got := env.Results[0]["resource_type"]; got != "items" {
		t.Fatalf("first row resource_type = %v, want items (highest count)", got)
	}
}

func TestAnalyticsCommandGroupByPlainReturnsTabSeparated(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for key, raw := range map[string]string{
		"A1": `{"key":"A1","data":{"itemType":"journalArticle"}}`,
		"A2": `{"key":"A2","data":{"itemType":"journalArticle"}}`,
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
	cmd := newAnalyticsCmd(&rootFlags{plain: true})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", dbPath, "--type", "items", "--group-by", "itemType"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analytics --plain: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "{") || strings.Contains(got, "\"value\"") {
		t.Fatalf("plain output looks like JSON, want tab-separated rows: %q", got)
	}
	if !strings.Contains(got, "\t") {
		t.Fatalf("plain output missing tab separator: %q", got)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) < 2 {
		t.Fatalf("plain output lines = %d, want header plus rows: %q", len(lines), got)
	}
	header := strings.ToLower(lines[0])
	if !strings.Contains(header, "value") || !strings.Contains(header, "count") {
		t.Fatalf("plain header = %q, want to contain value and count", lines[0])
	}
	if !strings.Contains(got, "journalArticle") || !strings.Contains(got, "book") {
		t.Fatalf("plain output missing expected values: %q", got)
	}
}

func TestAnalyticsCommandGroupByCSVReturnsCommaSeparated(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for key, raw := range map[string]string{
		"A1": `{"key":"A1","data":{"itemType":"journalArticle"}}`,
		"A2": `{"key":"A2","data":{"itemType":"journalArticle"}}`,
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
	cmd := newAnalyticsCmd(&rootFlags{csv: true})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", dbPath, "--type", "items", "--group-by", "itemType"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analytics --csv: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "\t") {
		t.Fatalf("csv output contains tab, want comma-separated: %q", got)
	}
	if !strings.Contains(got, ",") {
		t.Fatalf("csv output missing comma: %q", got)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) < 2 {
		t.Fatalf("csv lines = %d, want header plus rows: %q", len(lines), got)
	}
	header := strings.ToLower(lines[0])
	if !strings.Contains(header, "value") || !strings.Contains(header, "count") {
		t.Fatalf("csv header = %q, want value and count", lines[0])
	}
}

func TestAnalyticsCommandGroupBySelectNarrowsFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for key, raw := range map[string]string{
		"A1": `{"key":"A1","data":{"itemType":"journalArticle"}}`,
		"A2": `{"key":"A2","data":{"itemType":"journalArticle"}}`,
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
	cmd := newAnalyticsCmd(&rootFlags{asJSON: true, selectFields: "value"})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", dbPath, "--type", "items", "--group-by", "itemType"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analytics --select: %v", err)
	}
	env := analyticsDecodeEnvelope(t, out.Bytes())
	if len(env.Results) == 0 {
		t.Fatalf("results empty, want group-by rows: %s", out.String())
	}
	for _, row := range env.Results {
		if _, ok := row["value"]; !ok {
			t.Fatalf("row missing value after --select value: %+v", row)
		}
		if _, ok := row["count"]; ok {
			t.Fatalf("row still has count after --select value, want only value: %+v", row)
		}
	}
}

// TestAnalyticsCommandCompactKeepsRowKeyFields guards the field that carries
// the data. compactListFields is an allow-list, so a row field absent from it
// is dropped silently: under --compact (and therefore --agent, the documented
// agent path) every analytics row collapsed to a bare {"count":N} and the
// grouping key was gone. Measured live against a real mirror on 2026-08-22,
// after the envelope migration passed every other test.
func TestAnalyticsCommandCompactKeepsRowKeyFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for key, raw := range map[string]string{
		"A1": `{"key":"A1","data":{"itemType":"journalArticle"}}`,
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

	for _, tc := range []struct {
		name  string
		args  []string
		field string
	}{
		{"breakdown", []string{"--db", dbPath}, "resource_type"},
		{"type", []string{"--db", dbPath, "--type", "items"}, "resource_type"},
		{"group-by", []string{"--db", dbPath, "--type", "items", "--group-by", "itemType"}, "value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := newAnalyticsCmd(&rootFlags{asJSON: true, compact: true})
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("analytics --compact: %v", err)
			}
			env := analyticsDecodeEnvelope(t, out.Bytes())
			if len(env.Results) == 0 {
				t.Fatalf("results empty, want rows: %s", out.String())
			}
			for _, row := range env.Results {
				if v, ok := row[tc.field]; !ok || v == nil || v == "" {
					t.Fatalf("--compact dropped %s, leaving no grouping key: %+v", tc.field, row)
				}
				if _, ok := row["count"]; !ok {
					t.Fatalf("--compact dropped count: %+v", row)
				}
			}
		})
	}
}

func TestAnalyticsCommandMetaGroupByPresentAndAbsent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for key, raw := range map[string]string{
		"J1": `{"key":"J1","data":{"itemType":"journalArticle"}}`,
		"J2": `{"key":"J2","data":{"itemType":"journalArticle"}}`,
	} {
		if err := db.Upsert("items", key, json.RawMessage(raw)); err != nil {
			db.Close()
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	if err := db.Upsert("collections", "COL1", json.RawMessage(`{"key":"COL1"}`)); err != nil {
		db.Close()
		t.Fatalf("seed collections: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// --group-by mode: meta.group_by equals the requested field.
	t.Run("group_by_present", func(t *testing.T) {
		var out bytes.Buffer
		cmd := newAnalyticsCmd(&rootFlags{asJSON: true})
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--db", dbPath, "--type", "items", "--group-by", "itemType"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("group-by: %v", err)
		}
		env := analyticsDecodeEnvelope(t, out.Bytes())
		if env.Meta.GroupBy != "itemType" {
			t.Fatalf("meta.group_by = %q, want itemType", env.Meta.GroupBy)
		}
	})

	// --type mode: meta.group_by absent.
	t.Run("type_absent", func(t *testing.T) {
		var out bytes.Buffer
		cmd := newAnalyticsCmd(&rootFlags{asJSON: true})
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--db", dbPath, "--type", "items"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("type: %v", err)
		}
		env := analyticsDecodeEnvelope(t, out.Bytes())
		if env.Meta.GroupBy != "" {
			t.Fatalf("meta.group_by = %q, want empty (absent) for --type mode", env.Meta.GroupBy)
		}
		// Also assert raw JSON does not contain the key when omitempty.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
			t.Fatalf("decode raw: %v", err)
		}
		var meta map[string]any
		if err := json.Unmarshal(raw["meta"], &meta); err != nil {
			t.Fatalf("decode meta: %v", err)
		}
		if _, ok := meta["group_by"]; ok {
			t.Fatalf("meta contains group_by %v, want absent for --type mode", meta)
		}
	})

	// no-flag breakdown: meta.group_by absent.
	t.Run("breakdown_absent", func(t *testing.T) {
		var out bytes.Buffer
		cmd := newAnalyticsCmd(&rootFlags{asJSON: true})
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--db", dbPath})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("breakdown: %v", err)
		}
		env := analyticsDecodeEnvelope(t, out.Bytes())
		if env.Meta.GroupBy != "" {
			t.Fatalf("meta.group_by = %q, want empty for no-flag breakdown", env.Meta.GroupBy)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
			t.Fatalf("decode raw: %v", err)
		}
		var meta map[string]any
		if err := json.Unmarshal(raw["meta"], &meta); err != nil {
			t.Fatalf("decode meta: %v", err)
		}
		if _, ok := meta["group_by"]; ok {
			t.Fatalf("meta contains group_by %v, want absent for breakdown", meta)
		}
	})
}
