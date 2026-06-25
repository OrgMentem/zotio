// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean test-gaps pg9a): Covers sync helpers and paginated syncResource behavior.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"zotero-pp-cli/internal/client"
	"zotero-pp-cli/internal/config"
	"zotero-pp-cli/internal/store"
)

func TestSyncResourceVersionBasedIncremental(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	// First sync: server reports Last-Modified-Version; syncResource stores it.
	var firstSince string
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstSince = r.URL.Query().Get("since")
		w.Header().Set("Last-Modified-Version", "100")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"a"},{"id":"b"}]`))
	}))
	defer srv1.Close()
	if res := syncResource(syncTestClient(srv1.URL), db, "items", 0, false, 0, false); res.Err != nil {
		t.Fatalf("first sync error: %v", res.Err)
	}
	if firstSince != "" {
		t.Errorf("first sync sent since=%q, want empty (no checkpoint yet)", firstSince)
	}
	if v, err := db.GetLibraryVersion("items"); err != nil || v != 100 {
		t.Fatalf("stored library version = %d (err %v), want 100", v, err)
	}

	// Second sync: a stored checkpoint and no --full must send since=100 (int).
	var secondSince string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondSince = r.URL.Query().Get("since")
		w.Header().Set("Last-Modified-Version", "150")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"c"}]`))
	}))
	defer srv2.Close()
	if res := syncResource(syncTestClient(srv2.URL), db, "items", 0, false, 0, false); res.Err != nil {
		t.Fatalf("second sync error: %v", res.Err)
	}
	if secondSince != "100" {
		t.Errorf("second sync sent since=%q, want \"100\" (stored checkpoint)", secondSince)
	}

	// An explicit --since version overrides the stored checkpoint.
	var thirdSince string
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thirdSince = r.URL.Query().Get("since")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"d"}]`))
	}))
	defer srv3.Close()
	if res := syncResource(syncTestClient(srv3.URL), db, "items", 4521, false, 0, false); res.Err != nil {
		t.Fatalf("third sync error: %v", res.Err)
	}
	if thirdSince != "4521" {
		t.Errorf("explicit --since sent since=%q, want \"4521\"", thirdSince)
	}
}

func TestSyncPageExtractionHelpers(t *testing.T) {
	items, cursor, hasMore := extractPageItems(json.RawMessage(`[{"id":"a"},{"id":"b"}]`), "after")
	if len(items) != 2 || cursor != "" || hasMore {
		t.Fatalf("bare array extraction = len %d cursor %q hasMore %v, want len 2 empty false", len(items), cursor, hasMore)
	}

	items, cursor, hasMore = extractPageItems(json.RawMessage(`{"data":[{"id":"a"}],"next_cursor":"n1","has_more":true}`), "after")
	if len(items) != 1 || cursor != "n1" || !hasMore {
		t.Fatalf("data envelope extraction = len %d cursor %q hasMore %v, want len 1 n1 true", len(items), cursor, hasMore)
	}

	items, cursor, hasMore = extractPageItems(json.RawMessage(`{"widgets":[{"id":"a"}],"response_metadata":{"next_cursor":"nested"}}`), "after")
	if len(items) != 1 || cursor != "nested" || !hasMore {
		t.Fatalf("fallback envelope extraction = len %d cursor %q hasMore %v, want len 1 nested true", len(items), cursor, hasMore)
	}

	envelope := map[string]json.RawMessage{
		"links": json.RawMessage(`{"next":"https://api.example.test/items?page%5Bcursor%5D=from-link"}`),
		"data":  json.RawMessage(`[]`),
	}
	cursor, hasMore = extractPaginationFromEnvelope(envelope, "after")
	if cursor != "from-link" || !hasMore {
		t.Fatalf("link pagination = cursor %q hasMore %v, want from-link true", cursor, hasMore)
	}

	envelope = map[string]json.RawMessage{
		"has_more": json.RawMessage(`false`),
		"data":     json.RawMessage(`[]`),
	}
	cursor, hasMore = extractPaginationFromEnvelope(envelope, "after")
	if cursor != "" || hasMore {
		t.Fatalf("empty pagination = cursor %q hasMore %v, want empty false", cursor, hasMore)
	}

	cursor = nextCursorFromLinks(map[string]json.RawMessage{
		"links": json.RawMessage(`{"next":"https://api.example.test/items?after=after-link"}`),
	}, "after")
	if cursor != "after-link" {
		t.Fatalf("nextCursorFromLinks after = %q, want after-link", cursor)
	}

	cursor = findCursorInMap(map[string]json.RawMessage{
		"nextCursor": json.RawMessage(`"camel"`),
		"after":      json.RawMessage(`"after"`),
	}, []string{"after", "nextCursor"})
	if cursor != "after" {
		t.Fatalf("findCursorInMap = %q, want first matching key after", cursor)
	}

	cursor = findCursorInMap(map[string]json.RawMessage{"after": json.RawMessage(`""`)}, []string{"after"})
	if cursor != "" {
		t.Fatalf("findCursorInMap empty string = %q, want empty", cursor)
	}
}

func TestSyncDefaultAndResourceHelpers(t *testing.T) {
	defaults := determinePaginationDefaults()
	if defaults.cursorParam != "start" || defaults.limitParam != "limit" || defaults.limit != 100 {
		t.Fatalf("determinePaginationDefaults = %+v, want start/limit/100", defaults)
	}
	if got := determineSinceParam(); got != "since" {
		t.Fatalf("determineSinceParam = %q, want since", got)
	}

	oldDispatchers := discriminatorDispatchers
	discriminatorDispatchers = map[string]discriminatorDispatch{
		"sync-test-base": {
			Field: "kind",
			Values: map[string]string{
				"child": "sync-test-child",
			},
		},
	}
	defer func() { discriminatorDispatchers = oldDispatchers }()
	if got := resolveDiscriminatedResource("sync-test-base", map[string]any{"kind": "child"}); got != "sync-test-child" {
		t.Fatalf("resolveDiscriminatedResource matched = %q, want sync-test-child", got)
	}
	if got := resolveDiscriminatedResource("sync-test-base", map[string]any{"kind": "other"}); got != "sync-test-base" {
		t.Fatalf("resolveDiscriminatedResource fallback = %q, want sync-test-base", got)
	}

	if got := extractID("items", map[string]any{"id": "item-1"}); got != "item-1" {
		t.Fatalf("extractID present = %q, want item-1", got)
	}
	if got := extractID("items", map[string]any{"title": "missing id"}); got != "" {
		t.Fatalf("extractID missing = %q, want empty", got)
	}

	// PATCH(glean bugfix): Zotero tags and global schema lists use domain-name
	// keys that are not in the generic ID fallback list.
	idCases := []struct {
		resource string
		obj      map[string]any
		want     string
	}{
		{"tags", map[string]any{"tag": "foo"}, "foo"},
		{"schema", map[string]any{"itemType": "journalArticle"}, "journalArticle"},
		{"schema-item-fields", map[string]any{"field": "title"}, "title"},
		{"schema-creator-fields", map[string]any{"field": "firstName"}, "firstName"},
	}
	for _, tc := range idCases {
		if got := extractID(tc.resource, tc.obj); got != tc.want {
			t.Fatalf("extractID(%q, %#v) = %q, want %q", tc.resource, tc.obj, got, tc.want)
		}
	}

	if got, err := syncResourcePath("items"); err != nil || got != "/items" {
		t.Fatalf("syncResourcePath(items) = %q, %v; want /items, nil", got, err)
	}
	if got, err := syncResourcePath("not-a-resource"); err == nil || got != "" {
		t.Fatalf("syncResourcePath(unknown) = %q, %v; want empty error", got, err)
	}

	resources := defaultSyncResources()
	if len(resources) == 0 {
		t.Fatal("defaultSyncResources returned no resources")
	}
	wantMembers := map[string]bool{"collections": false, "items": false, "searches": false, "tags": false}
	for _, resource := range resources {
		if _, ok := wantMembers[resource]; ok {
			wantMembers[resource] = true
		}
	}
	for resource, found := range wantMembers {
		if !found {
			t.Fatalf("defaultSyncResources missing %q in %v", resource, resources)
		}
	}
}

// PATCH(glean bugfix): schema sync must request global Zotero endpoints and
// still persist rows whose key field is itemType rather than id/key/name.
func TestSyncResourceSchemaUsesGlobalBaseAndSchemaID(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	var globalHits, libraryHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/itemTypes":
			globalHits++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[{"itemType":"book","localized":"Book"}]`)
		case "/users/0/itemTypes":
			libraryHits++
			http.Error(w, "No endpoint found", http.StatusNotFound)
		default:
			t.Errorf("unexpected schema sync path %q", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	db := syncTestOpenStore(t)
	defer db.Close()
	result := syncResource(syncTestClient(server.URL+"/users/0"), db, "schema", 0, false, 0, false)
	if result.Err != nil || result.Warn != nil {
		t.Fatalf("schema sync Err = %v Warn = %v", result.Err, result.Warn)
	}
	if result.Count != 1 {
		t.Fatalf("schema sync count = %d, want 1", result.Count)
	}
	if globalHits != 1 || libraryHits != 0 {
		t.Fatalf("schema sync hits global=%d library=%d, want global=1 library=0", globalHits, libraryHits)
	}
	syncTestAssertStoreCount(t, db, "schema", 1)
	if data, err := db.Get("schema", "book"); err != nil || len(data) == 0 {
		t.Fatalf("stored schema book = %s (err %v), want row", string(data), err)
	}
}

func TestSyncResourcePaginatesMultiplePages(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/items" {
			t.Errorf("server path = %q, want /items", r.URL.Path)
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		requests++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("start") {
		case "0":
			if got := r.URL.Query().Get("limit"); got != "100" {
				t.Errorf("limit = %q, want 100", got)
			}
			fmt.Fprint(w, syncTestItemsJSON("first", 100))
		case "100":
			fmt.Fprint(w, `[{"id":"last"}]`)
		default:
			t.Errorf("unexpected start cursor %q", r.URL.Query().Get("start"))
			http.Error(w, "unexpected cursor", http.StatusBadRequest)
			return
		}
	}))
	defer server.Close()

	db := syncTestOpenStore(t)
	defer db.Close()
	result := syncResource(syncTestClient(server.URL), db, "items", 0, false, 0, false)
	if result.Err != nil || result.Warn != nil {
		t.Fatalf("syncResource result Err = %v Warn = %v", result.Err, result.Warn)
	}
	if result.Count != 101 {
		t.Fatalf("syncResource count = %d, want 101", result.Count)
	}
	if requests != 2 {
		t.Fatalf("server requests = %d, want 2", requests)
	}
	syncTestAssertStoreCount(t, db, "items", 101)
}

func TestSyncResourceStopsOnStuckCursor(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":%s,"next_cursor":"same","has_more":true}`, syncTestItemsJSON(fmt.Sprintf("page-%d", requests), 100))
	}))
	defer server.Close()

	db := syncTestOpenStore(t)
	defer db.Close()
	result := syncResource(syncTestClient(server.URL), db, "items", 0, false, 0, false)
	if result.Err != nil || result.Warn != nil {
		t.Fatalf("syncResource result Err = %v Warn = %v", result.Err, result.Warn)
	}
	if requests != 2 {
		t.Fatalf("stuck cursor requests = %d, want 2", requests)
	}
	if result.Count != 200 {
		t.Fatalf("stuck cursor count = %d, want 200 stored before guard abort", result.Count)
	}
	syncTestAssertStoreCount(t, db, "items", 200)
}

func TestSyncResourceStopsAtMaxPages(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":%s,"next_cursor":"next-%d","has_more":true}`, syncTestItemsJSON(fmt.Sprintf("page-%d", requests), 100), requests)
	}))
	defer server.Close()

	db := syncTestOpenStore(t)
	defer db.Close()
	result := syncResource(syncTestClient(server.URL), db, "items", 0, false, 1, false)
	if result.Err != nil || result.Warn != nil {
		t.Fatalf("syncResource result Err = %v Warn = %v", result.Err, result.Warn)
	}
	if requests != 1 {
		t.Fatalf("maxPages requests = %d, want 1", requests)
	}
	if result.Count != 100 {
		t.Fatalf("maxPages count = %d, want 100", result.Count)
	}
	syncTestAssertStoreCount(t, db, "items", 100)
}

func TestSyncResourceStoresSingleObject(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"single","title":"One object"}`)
	}))
	defer server.Close()

	db := syncTestOpenStore(t)
	defer db.Close()
	result := syncResource(syncTestClient(server.URL), db, "items", 0, false, 0, false)
	if result.Err != nil || result.Warn != nil {
		t.Fatalf("syncResource result Err = %v Warn = %v", result.Err, result.Warn)
	}
	if result.Count != 1 {
		t.Fatalf("single-object count = %d, want 1", result.Count)
	}
	syncTestAssertStoreCount(t, db, "items", 1)
}

func TestSyncResourceAccessDeniedReturnsWarning(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden by resource ACL", http.StatusForbidden)
	}))
	defer server.Close()

	db := syncTestOpenStore(t)
	defer db.Close()
	result := syncResource(syncTestClient(server.URL), db, "items", 0, false, 0, false)
	if result.Err != nil {
		t.Fatalf("access denied Err = %v, want nil", result.Err)
	}
	if result.Warn == nil {
		t.Fatal("access denied Warn = nil, want warning")
	}
	if result.Count != 0 {
		t.Fatalf("access denied count = %d, want 0", result.Count)
	}
	syncTestAssertStoreCount(t, db, "items", 0)
}

func syncTestWithHumanFriendly(t *testing.T, value bool) {
	t.Helper()
	old := humanFriendly
	humanFriendly = value
	t.Cleanup(func() { humanFriendly = old })
}

func syncTestClient(baseURL string) *client.Client {
	c := client.New(&config.Config{BaseURL: baseURL}, 5*time.Second, 0)
	c.BaseURL = baseURL
	c.NoCache = true
	return c
}

func syncTestOpenStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("store.OpenWithContext error = %v", err)
	}
	return db
}

func syncTestAssertStoreCount(t *testing.T, db *store.Store, resource string, want int) {
	t.Helper()
	got, err := db.Count(resource)
	if err != nil {
		t.Fatalf("db.Count(%q) error = %v", resource, err)
	}
	if got != want {
		t.Fatalf("db.Count(%q) = %d, want %d", resource, got, want)
	}
}

func syncTestItemsJSON(prefix string, n int) string {
	items := make([]map[string]string, n)
	for i := range items {
		items[i] = map[string]string{
			"id":    fmt.Sprintf("%s-%03d", prefix, i),
			"title": fmt.Sprintf("%s item %03d", prefix, i),
		}
	}
	b, err := json.Marshal(items)
	if err != nil {
		panic(err)
	}
	return string(b)
}
