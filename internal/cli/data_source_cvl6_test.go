// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean cvl6): fixture-backed parity tests proving --data-source local
// reproduces the live endpoint's scoped key sets and ordering.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zotio/internal/store"
)

// cvl6 seed items in arbitrary insertion order; the local planner must sort and
// filter them independently to match the live (pre-sorted) fixtures.
var cvl6Items = []json.RawMessage{
	json.RawMessage(`{"key":"A","version":1,"data":{"key":"A","itemType":"journalArticle","title":"Beta","date":"2020","collections":["COL1"],"tags":[{"tag":"ml"}],"creators":[{"lastName":"Zhang"}]}}`),
	json.RawMessage(`{"key":"B","version":1,"data":{"key":"B","itemType":"journalArticle","title":"Alpha","date":"2022","collections":["COL1","COL2"],"tags":[{"tag":"nlp"}]}}`),
	json.RawMessage(`{"key":"C","version":1,"data":{"key":"C","itemType":"book","title":"Gamma","date":"2019","collections":["COL2"],"tags":[{"tag":"ml"}]}}`),
}

func cvl6SeedLocalDB(t *testing.T) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0") // unused in local mode
	dbPath := defaultDBPath("zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", cvl6Items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

var childrenParityItems = []json.RawMessage{
	json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"book","title":"Parent One","dateModified":"2026-01-01T00:00:00Z"}}`),
	json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"book","title":"Parent Two","dateModified":"2026-01-02T00:00:00Z"}}`),
	json.RawMessage(`{"key":"C1","version":1,"data":{"key":"C1","itemType":"attachment","title":"Alpha Attachment","parentItem":"P1","dateModified":"2026-01-03T00:00:00Z"}}`),
	json.RawMessage(`{"key":"C2","version":1,"data":{"key":"C2","itemType":"note","title":"Beta Note","parentItem":"P1","dateModified":"2026-01-04T00:00:00Z"}}`),
	json.RawMessage(`{"key":"C3","version":1,"data":{"key":"C3","itemType":"note","title":"Other Parent Note","parentItem":"P2","dateModified":"2026-01-05T00:00:00Z"}}`),
	json.RawMessage(`{"key":"T1","version":1,"data":{"key":"T1","itemType":"journalArticle","title":"Top Level","dateModified":"2026-01-06T00:00:00Z"}}`),
}

func childrenParitySeedLocalDB(t *testing.T) *rootFlags {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0") // unused in local mode
	dbPath := defaultDBPath("zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", childrenParityItems); err != nil {
		t.Fatalf("seed child items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return &rootFlags{dataSource: "local", noCache: true, timeout: time.Second}
}

func itemKeysFromRawList(t *testing.T, data json.RawMessage) []string {
	t.Helper()
	var got []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode item list %q: %v", string(data), err)
	}
	keys := make([]string, 0, len(got))
	for _, item := range got {
		keys = append(keys, item.Key)
	}
	return keys
}

func runItemsListKeys(t *testing.T, flags *rootFlags, args []string) []string {
	t.Helper()
	cmd := newItemsListCmd(flags)
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items list %v: %v", args, err)
	}
	var env struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	keys := make([]string, 0, len(env.Results))
	for _, r := range env.Results {
		keys = append(keys, r["key"].(string))
	}
	return keys
}

func equalKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] { //nolint:gosec // G602: loop bounded by the len(a)==len(b) guard above; b[i] is always in range.
			return false
		}
	}
	return true
}

// TestItemsListLocalParity_ItemTypeSort asserts the live and local key order
// match for an itemType-filtered, title-sorted query.
func TestItemsListLocalParity_ItemTypeSort(t *testing.T) {
	// Live fixture returns the API's already-filtered+sorted truth: [B, A].
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[` + string(cvl6Items[1]) + `,` + string(cvl6Items[0]) + `]`))
	}))
	defer srv.Close()

	args := []string{"--item-type", "journalArticle", "--sort", "title", "--direction", "asc"}

	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	liveFlags := &rootFlags{asJSON: true, dataSource: "live", noCache: true, timeout: time.Second}
	liveKeys := runItemsListKeys(t, liveFlags, args)
	if want := []string{"B", "A"}; !equalKeys(liveKeys, want) {
		t.Fatalf("live keys = %v, want %v", liveKeys, want)
	}

	cvl6SeedLocalDB(t)
	localFlags := &rootFlags{asJSON: true, dataSource: "local", noCache: true, timeout: time.Second}
	localKeys := runItemsListKeys(t, localFlags, args)

	if !equalKeys(localKeys, liveKeys) {
		t.Fatalf("local keys = %v, want parity with live %v", localKeys, liveKeys)
	}
}

// TestItemsListLocalParity_Tag asserts tag-scoped local results match the live
// truth (tag=ml, title asc -> [A, C]).
func TestItemsListLocalParity_Tag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[` + string(cvl6Items[0]) + `,` + string(cvl6Items[2]) + `]`))
	}))
	defer srv.Close()

	args := []string{"--tag", "ml", "--sort", "title", "--direction", "asc"}

	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	liveFlags := &rootFlags{asJSON: true, dataSource: "live", noCache: true, timeout: time.Second}
	liveKeys := runItemsListKeys(t, liveFlags, args)
	if want := []string{"A", "C"}; !equalKeys(liveKeys, want) {
		t.Fatalf("live keys = %v, want %v", liveKeys, want)
	}

	cvl6SeedLocalDB(t)
	localFlags := &rootFlags{asJSON: true, dataSource: "local", noCache: true, timeout: time.Second}
	localKeys := runItemsListKeys(t, localFlags, args)
	if !equalKeys(localKeys, liveKeys) {
		t.Fatalf("local tag keys = %v, want parity with live %v", localKeys, liveKeys)
	}
}

// TestItemsListLocalNoMatchEmptyArray confirms an unmatched local scope returns
// an empty array, not an error (matching a live empty list).
func TestItemsListLocalNoMatchEmptyArray(t *testing.T) {
	cvl6SeedLocalDB(t)
	flags := &rootFlags{asJSON: true, dataSource: "local", noCache: true, timeout: time.Second}
	keys := runItemsListKeys(t, flags, []string{"--tag", "doesnotexist"})
	if len(keys) != 0 {
		t.Fatalf("expected no keys, got %v", keys)
	}
}

func TestResolveReadLocalItemsChildrenScopesToParent(t *testing.T) {
	flags := childrenParitySeedLocalDB(t)
	params := map[string]string{"sort": "title", "direction": "asc"}

	data, prov, err := resolveRead(context.Background(), nil, flags, "items", false, "/items/P1/children", params, nil)
	if err != nil {
		t.Fatalf("resolveRead children: %v", err)
	}
	if prov.Source != "local" || prov.ResourceType != "items" || !prov.Scoped {
		t.Fatalf("provenance = %+v, want scoped local items", prov)
	}
	if keys := itemKeysFromRawList(t, data); !equalKeys(keys, []string{"C1", "C2"}) {
		t.Fatalf("children keys = %v, want [C1 C2]", keys)
	}
}

func TestResolveReadLocalItemsChildrenItemTypeAndEmptyParent(t *testing.T) {
	flags := childrenParitySeedLocalDB(t)

	data, _, err := resolveRead(context.Background(), nil, flags, "items", false, "/items/P1/children", map[string]string{"itemType": "note"}, nil)
	if err != nil {
		t.Fatalf("resolveRead note children: %v", err)
	}
	if keys := itemKeysFromRawList(t, data); !equalKeys(keys, []string{"C2"}) {
		t.Fatalf("note children keys = %v, want [C2]", keys)
	}

	data, _, err = resolveRead(context.Background(), nil, flags, "items", false, "/items/NOPE/children", nil, nil)
	if err != nil {
		t.Fatalf("resolveRead unknown-parent children: %v", err)
	}
	if string(data) != "[]" {
		t.Fatalf("unknown-parent children raw JSON = %q, want []", string(data))
	}
}

func TestResolveReadLocalItemsSingleGetStillUsesID(t *testing.T) {
	flags := childrenParitySeedLocalDB(t)

	data, prov, err := resolveRead(context.Background(), nil, flags, "items", false, "/items/C1", nil, nil)
	if err != nil {
		t.Fatalf("resolveRead item get: %v", err)
	}
	if prov.Source != "local" || prov.ResourceType != "items" || prov.Scoped {
		t.Fatalf("provenance = %+v, want unscoped local items get", prov)
	}

	var got struct {
		Key  string `json:"key"`
		Data struct {
			ParentItem string `json:"parentItem"`
			ItemType   string `json:"itemType"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode item get %q: %v", string(data), err)
	}
	if got.Key != "C1" || got.Data.ParentItem != "P1" || got.Data.ItemType != "attachment" {
		t.Fatalf("item get = %#v, want C1 attachment under P1", got)
	}
}

// PATCH(glean bugfix): local base-resource reads must list all stored rows
// even when generated list commands pass isList=false.
var bug6Collections = []json.RawMessage{
	json.RawMessage(`{"key":"COL1","version":1,"data":{"key":"COL1","name":"First"}}`),
	json.RawMessage(`{"key":"COL2","version":1,"data":{"key":"COL2","name":"Second"}}`),
}

func bug6SeedLocalCollections(t *testing.T) *rootFlags {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	dbPath := defaultDBPath("zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("collections", bug6Collections); err != nil {
		t.Fatalf("seed collections: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return &rootFlags{dataSource: "local", noCache: true, timeout: time.Second}
}

func TestResolveReadLocalBaseResourcePathListsCollections(t *testing.T) {
	flags := bug6SeedLocalCollections(t)

	data, prov, err := resolveRead(context.Background(), nil, flags, "collections", false, "/collections", nil, nil)
	if err != nil {
		t.Fatalf("resolveRead list: %v", err)
	}
	if prov.Source != "local" || prov.ResourceType != "collections" {
		t.Fatalf("provenance = %+v, want local collections", prov)
	}

	var got []struct {
		Key  string `json:"key"`
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode list %q: %v", string(data), err)
	}
	if len(got) != 2 {
		t.Fatalf("list count = %d, want 2: %v", len(got), got)
	}
	namesByKey := map[string]string{}
	for _, item := range got {
		namesByKey[item.Key] = item.Data.Name
	}
	if namesByKey["COL1"] != "First" || namesByKey["COL2"] != "Second" {
		t.Fatalf("list rows = %#v, want both seeded collections", namesByKey)
	}
}

func TestResolveReadLocalCollectionSingleItemStillUsesID(t *testing.T) {
	flags := bug6SeedLocalCollections(t)

	data, prov, err := resolveRead(context.Background(), nil, flags, "collections", false, "/collections/COL1", nil, nil)
	if err != nil {
		t.Fatalf("resolveRead get: %v", err)
	}
	if prov.Source != "local" || prov.ResourceType != "collections" {
		t.Fatalf("provenance = %+v, want local collections", prov)
	}

	var got struct {
		Key  string `json:"key"`
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode single %q: %v", string(data), err)
	}
	if got.Key != "COL1" || got.Data.Name != "First" {
		t.Fatalf("single item = %#v, want COL1 First", got)
	}
}

var sliceBCollections = []json.RawMessage{
	json.RawMessage(`{"key":"C1","version":1,"data":{"key":"C1","name":"First"}}`),
	json.RawMessage(`{"key":"C2","version":1,"data":{"key":"C2","name":"Second"}}`),
	json.RawMessage(`{"key":"C3","version":1,"data":{"key":"C3","name":"Third"}}`),
	json.RawMessage(`{"key":"C4","version":1,"data":{"key":"C4","name":"Fourth"}}`),
}

func sliceBSeedLocalCollections(t *testing.T, rows []json.RawMessage) *rootFlags {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0") // unused in local mode
	dbPath := defaultDBPath("zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if len(rows) > 0 {
		if _, _, err := db.UpsertBatch("collections", rows); err != nil {
			t.Fatalf("seed collections: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return &rootFlags{dataSource: "local", noCache: true, timeout: time.Second}
}

func collectionKeysFromRawList(t *testing.T, data json.RawMessage) []string {
	t.Helper()
	var got []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode collection list %q: %v", string(data), err)
	}
	keys := make([]string, 0, len(got))
	for _, row := range got {
		keys = append(keys, row.Key)
	}
	return keys
}

func rawMessageKeys(t *testing.T, rows []json.RawMessage) []string {
	t.Helper()
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		var got struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(row, &got); err != nil {
			t.Fatalf("decode row %q: %v", string(row), err)
		}
		keys = append(keys, got.Key)
	}
	return keys
}

func assertStringSlicesEqual(t *testing.T, got, want []string) {
	t.Helper()
	if !equalKeys(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestResolveReadLocalCollectionsAppliesStartPagination(t *testing.T) {
	flags := sliceBSeedLocalCollections(t, sliceBCollections)

	allData, _, err := resolveRead(context.Background(), nil, flags, "collections", false, "/collections", nil, nil)
	if err != nil {
		t.Fatalf("resolveRead all collections: %v", err)
	}
	allKeys := collectionKeysFromRawList(t, allData)
	if len(allKeys) != 4 {
		t.Fatalf("unpaginated collection count = %d, want 4: %v", len(allKeys), allKeys)
	}

	data, prov, err := resolveRead(context.Background(), nil, flags, "collections", false, "/collections", map[string]string{"start": "2"}, nil)
	if err != nil {
		t.Fatalf("resolveRead start=2: %v", err)
	}
	if prov.Source != "local" || prov.ResourceType != "collections" {
		t.Fatalf("provenance = %+v, want local collections", prov)
	}

	keys := collectionKeysFromRawList(t, data)
	assertStringSlicesEqual(t, keys, allKeys[2:])
	for _, got := range keys {
		for _, skipped := range allKeys[:2] {
			if got == skipped {
				t.Fatalf("start=2 returned skipped first-page key %q: all=%v got=%v", got, allKeys, keys)
			}
		}
	}
}

func TestResolveReadLocalCollectionsStartPastEndReturnsEmptyArray(t *testing.T) {
	flags := sliceBSeedLocalCollections(t, sliceBCollections)

	data, _, err := resolveRead(context.Background(), nil, flags, "collections", false, "/collections", map[string]string{"start": "99"}, nil)
	if err != nil {
		t.Fatalf("resolveRead start past end: %v", err)
	}
	if string(data) != "[]" {
		t.Fatalf("start past end data = %q, want []", string(data))
	}
}

func TestResolveReadLocalCollectionsAppliesLimitPagination(t *testing.T) {
	flags := sliceBSeedLocalCollections(t, sliceBCollections)

	allData, _, err := resolveRead(context.Background(), nil, flags, "collections", false, "/collections", nil, nil)
	if err != nil {
		t.Fatalf("resolveRead all collections: %v", err)
	}
	allKeys := collectionKeysFromRawList(t, allData)
	if len(allKeys) != 4 {
		t.Fatalf("unpaginated collection count = %d, want 4: %v", len(allKeys), allKeys)
	}

	data, _, err := resolveRead(context.Background(), nil, flags, "collections", false, "/collections", map[string]string{"limit": "2"}, nil)
	if err != nil {
		t.Fatalf("resolveRead limit=2: %v", err)
	}
	keys := collectionKeysFromRawList(t, data)
	assertStringSlicesEqual(t, keys, allKeys[:2])
}

func TestResolveReadLocalCollectionsAppliesStartBeforeLimit(t *testing.T) {
	flags := sliceBSeedLocalCollections(t, sliceBCollections)

	allData, _, err := resolveRead(context.Background(), nil, flags, "collections", false, "/collections", nil, nil)
	if err != nil {
		t.Fatalf("resolveRead all collections: %v", err)
	}
	allKeys := collectionKeysFromRawList(t, allData)
	if len(allKeys) != 4 {
		t.Fatalf("unpaginated collection count = %d, want 4: %v", len(allKeys), allKeys)
	}

	data, _, err := resolveRead(context.Background(), nil, flags, "collections", false, "/collections", map[string]string{"start": "1", "limit": "2"}, nil)
	if err != nil {
		t.Fatalf("resolveRead start=1 limit=2: %v", err)
	}
	keys := collectionKeysFromRawList(t, data)
	assertStringSlicesEqual(t, keys, allKeys[1:3])
}

func TestResolveReadLocalCollectionsEmptyStoreStillErrors(t *testing.T) {
	flags := sliceBSeedLocalCollections(t, nil)

	data, _, err := resolveRead(context.Background(), nil, flags, "collections", false, "/collections", nil, nil)
	if err == nil {
		t.Fatalf("resolveRead empty collections data = %q, want error", string(data))
	}
	if got, want := err.Error(), `no local data for "collections". Run 'zotio sync' first`; got != want {
		t.Fatalf("empty collections error = %q, want %q", got, want)
	}
}

func TestHasUnreproducibleParamsScopesGenericLocalWarnings(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]string
		want   bool
	}{
		{
			name:   "pagination and format are reproducible",
			params: map[string]string{"limit": "2", "start": "1", "format": "json"},
			want:   false,
		},
		{
			name:   "real filter is unreproducible",
			params: map[string]string{"tag": "ml"},
			want:   true,
		},
		{
			name:   "empty filter value is ignored",
			params: map[string]string{"tag": ""},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasUnreproducibleParams(tt.params); got != tt.want {
				t.Fatalf("hasUnreproducibleParams(%v) = %v, want %v", tt.params, got, tt.want)
			}
		})
	}
}

func TestPaginateLocalRowsAppliesOffsetBeforeLimit(t *testing.T) {
	rows := []json.RawMessage{
		json.RawMessage(`{"key":"A"}`),
		json.RawMessage(`{"key":"B"}`),
		json.RawMessage(`{"key":"C"}`),
		json.RawMessage(`{"key":"D"}`),
	}
	tests := []struct {
		name   string
		params map[string]string
		want   []string
	}{
		{
			name:   "start skips leading rows",
			params: map[string]string{"start": "2"},
			want:   []string{"C", "D"},
		},
		{
			name:   "limit keeps first page",
			params: map[string]string{"limit": "2"},
			want:   []string{"A", "B"},
		},
		{
			name:   "start applies before limit",
			params: map[string]string{"start": "1", "limit": "2"},
			want:   []string{"B", "C"},
		},
		{
			name:   "start past end is empty",
			params: map[string]string{"start": "99"},
			want:   []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rawMessageKeys(t, paginateLocalRows(rows, tt.params))
			assertStringSlicesEqual(t, got, tt.want)
		})
	}
}
