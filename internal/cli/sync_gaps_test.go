// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean test-gaps pg9a): Covers sync helpers and paginated syncResource behavior.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"zotero-pp-cli/internal/client"
	"zotero-pp-cli/internal/config"
	"zotero-pp-cli/internal/store"
)

func TestSyncParseSinceDuration(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{name: "days", in: "7d", want: 7 * 24 * time.Hour},
		{name: "hours", in: "24h", want: 24 * time.Hour},
		{name: "minutes", in: "30m", want: 30 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := time.Now()
			got, err := parseSinceDuration(tt.in)
			after := time.Now()
			if err != nil {
				t.Fatalf("parseSinceDuration(%q) error = %v", tt.in, err)
			}
			lower := before.Add(-tt.want).Add(-100 * time.Millisecond)
			upper := after.Add(-tt.want).Add(100 * time.Millisecond)
			if got.Before(lower) || got.After(upper) {
				t.Fatalf("parseSinceDuration(%q) = %s, want between %s and %s", tt.in, got, lower, upper)
			}
		})
	}

	for _, in := range []string{"", "7", "1y", "d", " 4 h "} {
		t.Run("invalid/"+strconv.Quote(in), func(t *testing.T) {
			if got, err := parseSinceDuration(in); err == nil {
				t.Fatalf("parseSinceDuration(%q) = %s, nil error", in, got)
			}
		})
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
	if defaults.cursorParam != "after" || defaults.limitParam != "limit" || defaults.limit != 100 {
		t.Fatalf("determinePaginationDefaults = %+v, want after/limit/100", defaults)
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
		switch r.URL.Query().Get("after") {
		case "":
			fmt.Fprintf(w, `{"data":%s,"next_cursor":"page-2","has_more":true}`, syncTestItemsJSON("first", 100))
		case "page-2":
			fmt.Fprint(w, `{"data":[{"id":"last"}],"has_more":false}`)
		default:
			t.Errorf("unexpected after cursor %q", r.URL.Query().Get("after"))
			http.Error(w, "unexpected cursor", http.StatusBadRequest)
			return
		}
	}))
	defer server.Close()

	db := syncTestOpenStore(t)
	defer db.Close()
	result := syncResource(syncTestClient(server.URL), db, "items", "", false, 0, false)
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
	result := syncResource(syncTestClient(server.URL), db, "items", "", false, 0, false)
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
	result := syncResource(syncTestClient(server.URL), db, "items", "", false, 1, false)
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
	result := syncResource(syncTestClient(server.URL), db, "items", "", false, 0, false)
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
	result := syncResource(syncTestClient(server.URL), db, "items", "", false, 0, false)
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
	db, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("store.Open error = %v", err)
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
