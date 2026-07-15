// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// fixture-backed parity tests proving --data-source local
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
	"strings"
	"testing"
	"time"

	"zotio/internal/store"
)

// local query planner seed items in arbitrary insertion order; the local planner must sort and
// filter them independently to match the live (pre-sorted) fixtures.
var localQueryPlannerItems = []json.RawMessage{
	json.RawMessage(`{"key":"A","version":1,"data":{"key":"A","itemType":"journalArticle","title":"Beta","date":"2020","collections":["COL1"],"tags":[{"tag":"ml"}],"creators":[{"lastName":"Zhang"}]}}`),
	json.RawMessage(`{"key":"B","version":1,"data":{"key":"B","itemType":"journalArticle","title":"Alpha","date":"2022","collections":["COL1","COL2"],"tags":[{"tag":"nlp"}]}}`),
	json.RawMessage(`{"key":"C","version":1,"data":{"key":"C","itemType":"book","title":"Gamma","date":"2019","collections":["COL2"],"tags":[{"tag":"ml"}]}}`),
}

func seedLocalQueryPlannerDB(t *testing.T) {
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
	if _, _, err := db.UpsertBatch("items", localQueryPlannerItems); err != nil {
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
		_, _ = w.Write([]byte(`[` + string(localQueryPlannerItems[1]) + `,` + string(localQueryPlannerItems[0]) + `]`))
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

	seedLocalQueryPlannerDB(t)
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
		_, _ = w.Write([]byte(`[` + string(localQueryPlannerItems[0]) + `,` + string(localQueryPlannerItems[2]) + `]`))
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

	seedLocalQueryPlannerDB(t)
	localFlags := &rootFlags{asJSON: true, dataSource: "local", noCache: true, timeout: time.Second}
	localKeys := runItemsListKeys(t, localFlags, args)
	if !equalKeys(localKeys, liveKeys) {
		t.Fatalf("local tag keys = %v, want parity with live %v", localKeys, liveKeys)
	}
}

// TestItemsListLocalNoMatchEmptyArray confirms an unmatched local scope returns
// an empty array, not an error (matching a live empty list).
func TestItemsListLocalNoMatchEmptyArray(t *testing.T) {
	seedLocalQueryPlannerDB(t)
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

// local base-resource reads must list all stored rows
// even when generated list commands pass isList=false.
var localBaseResourceCollections = []json.RawMessage{
	json.RawMessage(`{"key":"COL1","version":1,"data":{"key":"COL1","name":"First"}}`),
	json.RawMessage(`{"key":"COL2","version":1,"data":{"key":"COL2","name":"Second"}}`),
}

func seedLocalBaseResourceCollections(t *testing.T) *rootFlags {
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
	if _, _, err := db.UpsertBatch("collections", localBaseResourceCollections); err != nil {
		t.Fatalf("seed collections: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return &rootFlags{dataSource: "local", noCache: true, timeout: time.Second}
}

func TestResolveReadLocalBaseResourcePathListsCollections(t *testing.T) {
	flags := seedLocalBaseResourceCollections(t)

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
	flags := seedLocalBaseResourceCollections(t)

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

var localPaginationCollections = []json.RawMessage{
	json.RawMessage(`{"key":"C1","version":1,"data":{"key":"C1","name":"First"}}`),
	json.RawMessage(`{"key":"C2","version":1,"data":{"key":"C2","name":"Second"}}`),
	json.RawMessage(`{"key":"C3","version":1,"data":{"key":"C3","name":"Third"}}`),
	json.RawMessage(`{"key":"C4","version":1,"data":{"key":"C4","name":"Fourth"}}`),
}

func seedLocalPaginationCollections(t *testing.T, rows []json.RawMessage) *rootFlags {
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
	flags := seedLocalPaginationCollections(t, localPaginationCollections)

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
	flags := seedLocalPaginationCollections(t, localPaginationCollections)

	data, _, err := resolveRead(context.Background(), nil, flags, "collections", false, "/collections", map[string]string{"start": "99"}, nil)
	if err != nil {
		t.Fatalf("resolveRead start past end: %v", err)
	}
	if string(data) != "[]" {
		t.Fatalf("start past end data = %q, want []", string(data))
	}
}

func TestResolveReadLocalCollectionsAppliesLimitPagination(t *testing.T) {
	flags := seedLocalPaginationCollections(t, localPaginationCollections)

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
	flags := seedLocalPaginationCollections(t, localPaginationCollections)

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
	flags := seedLocalPaginationCollections(t, nil)

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

var localTrashFixture = []json.RawMessage{
	json.RawMessage(`{"key":"TRASH1","version":3,"data":{"key":"TRASH1","itemType":"book","title":"Discarded Book","dateModified":"2026-07-01T10:00:00Z"}}`),
	json.RawMessage(`{"key":"TRASH2","version":4,"data":{"key":"TRASH2","itemType":"journalArticle","title":"Discarded Article","dateModified":"2026-07-03T10:00:00Z"}}`),
	json.RawMessage(`{"key":"TRASH3","version":5,"itemType":"note","title":"Discarded Note","dateModified":"2026-07-02T10:00:00Z"}`),
}

var (
	localItemsSyncedAt = time.Date(2026, time.July, 9, 12, 30, 0, 0, time.UTC)
	localTrashSyncedAt = time.Date(2026, time.July, 10, 15, 45, 0, 0, time.UTC)
)

func seedLocalTrashDB(t *testing.T, trash []json.RawMessage, trashSynced bool) (*rootFlags, time.Time) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0")
	dbPath := defaultDBPath("zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	trashSyncedAt := localTrashSyncedAt
	// This live-items row is a regression sentinel: the old route interpreted
	// /items/trash as Get("items", "trash") and returned this payload.
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"trash","version":99,"data":{"key":"trash","itemType":"book","title":"Wrong live-items row"}}`),
	}); err != nil {
		t.Fatalf("seed items sentinel: %v", err)
	}
	if len(trash) > 0 {
		if _, _, err := db.UpsertBatch("items-trash", trash); err != nil {
			t.Fatalf("seed items-trash: %v", err)
		}
	}
	if _, err := db.DB().Exec(
		`INSERT OR REPLACE INTO sync_state(resource_type, last_cursor, last_synced_at, total_count) VALUES (?, ?, ?, ?)`,
		"items", "", localItemsSyncedAt, 1,
	); err != nil {
		t.Fatalf("seed items sync state: %v", err)
	}
	if trashSynced {
		if _, err := db.DB().Exec(
			`INSERT OR REPLACE INTO sync_state(resource_type, last_cursor, last_synced_at, total_count) VALUES (?, ?, ?, ?)`,
			"items-trash", "", trashSyncedAt, len(trash),
		); err != nil {
			t.Fatalf("seed items-trash sync state: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return &rootFlags{asJSON: true, dataSource: "local", noCache: true, timeout: time.Second}, trashSyncedAt
}

type localTrashEnvelope struct {
	Results []json.RawMessage `json:"results"`
	Meta    struct {
		Source       string `json:"source"`
		ResourceType string `json:"resource_type"`
		SyncedAt     string `json:"synced_at"`
	} `json:"meta"`
}

func runLocalTrash(t *testing.T, flags *rootFlags, args ...string) (localTrashEnvelope, error) {
	t.Helper()
	cmd := newItemsTrashCmd(flags)
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err != nil {
		return localTrashEnvelope{}, err
	}
	var env localTrashEnvelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode trash output %q: %v", out.String(), err)
	}
	return env, nil
}

func TestItemsTrashLocalListsTrashResourceWithProvenance(t *testing.T) {
	flags, wantSyncedAt := seedLocalTrashDB(t, localTrashFixture, true)

	env, err := runLocalTrash(t, flags)
	if err != nil {
		t.Fatalf("items trash: %v", err)
	}
	keys := rawMessageKeys(t, env.Results)
	if !equalKeys(keys, []string{"TRASH2", "TRASH3", "TRASH1"}) {
		t.Fatalf("trash keys = %v, want [TRASH2 TRASH3 TRASH1]", keys)
	}
	if env.Meta.Source != "local" || env.Meta.ResourceType != "items-trash" {
		t.Fatalf("provenance = %+v, want local items-trash", env.Meta)
	}
	syncedAt, err := time.Parse(time.RFC3339, env.Meta.SyncedAt)
	if err != nil {
		t.Fatalf("parse synced_at %q: %v", env.Meta.SyncedAt, err)
	}
	if !syncedAt.Equal(wantSyncedAt) {
		t.Fatalf("synced_at = %v, want items-trash timestamp %v", syncedAt, wantSyncedAt)
	}
}

func TestItemsTrashLocalAppliesStartBeforeLimit(t *testing.T) {
	flags, _ := seedLocalTrashDB(t, localTrashFixture, true)

	page, err := runLocalTrash(t, flags, "--start", "1", "--limit", "1")
	if err != nil {
		t.Fatalf("items trash page: %v", err)
	}
	if got := rawMessageKeys(t, page.Results); !equalKeys(got, []string{"TRASH3"}) {
		t.Fatalf("paginated trash keys = %v, want [TRASH3]", got)
	}
}

func TestItemsTrashLocalPastEndWriteThroughCacheReturnsArrayWithProvenance(t *testing.T) {
	flags, _ := seedLocalTrashDB(t, localTrashFixture[:1], false)

	env, err := runLocalTrash(t, flags, "--start", "10")
	if err != nil {
		t.Fatalf("items trash past-end page: %v", err)
	}
	if env.Results == nil || len(env.Results) != 0 {
		t.Fatalf("past-end items-trash results = %q, want []", env.Results)
	}
	if env.Meta.Source != "local" || env.Meta.ResourceType != "items-trash" {
		t.Fatalf("past-end provenance = %+v, want local items-trash", env.Meta)
	}
}

func TestItemsTrashLocalEmptyStoreReturnsSyncRemediation(t *testing.T) {
	flags, _ := seedLocalTrashDB(t, nil, false)

	env, err := runLocalTrash(t, flags)
	if err == nil {
		t.Fatalf("items trash empty store results = %q, want error", env.Results)
	}
	if got, want := err.Error(), `no local data for "items-trash". Run 'zotio sync' first`; got != want {
		t.Fatalf("empty items-trash error = %q, want %q", got, want)
	}
}

func TestItemsTrashLocalSyncedEmptyReturnsArrayWithProvenance(t *testing.T) {
	flags, wantSyncedAt := seedLocalTrashDB(t, nil, true)

	env, err := runLocalTrash(t, flags)
	if err != nil {
		t.Fatalf("items trash synced empty: %v", err)
	}
	if env.Results == nil || len(env.Results) != 0 {
		t.Fatalf("synced-empty items-trash results = %q, want []", env.Results)
	}
	if env.Meta.Source != "local" || env.Meta.ResourceType != "items-trash" {
		t.Fatalf("synced-empty provenance = %+v, want local items-trash", env.Meta)
	}
	syncedAt, err := time.Parse(time.RFC3339, env.Meta.SyncedAt)
	if err != nil {
		t.Fatalf("parse synced_at %q: %v", env.Meta.SyncedAt, err)
	}
	if !syncedAt.Equal(wantSyncedAt) {
		t.Fatalf("synced_at = %v, want items-trash timestamp %v", syncedAt, wantSyncedAt)
	}
}

// TestResolveLocalItemListRejectsNonNumericPagination asserts malformed start/limit
// query params surface a validation error instead of being silently clamped to 0.
func TestResolveLocalItemListRejectsNonNumericPagination(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	cases := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"non-numeric start", map[string]string{"start": "abc"}, "invalid start"},
		{"non-numeric limit", map[string]string{"limit": "xyz"}, "invalid limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, handled, err := resolveLocalItemList(db, "/items", tc.params)
			if !handled {
				t.Fatalf("handled = false, want true for item-list path")
			}
			if err == nil {
				t.Fatalf("err = nil, want validation error (data=%q)", data)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}
