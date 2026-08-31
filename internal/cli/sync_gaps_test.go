// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
	"zotio/internal/store"
)

func TestSyncResourceVersionBasedIncremental(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	// One server for the whole sequence: version cursors are per-plane, so
	// changing the base URL between passes would (correctly) discard the
	// checkpoint and is not what this test is about.
	var sinceSeen []string
	version := "100"
	body := `[{"id":"a"},{"id":"b"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinceSeen = append(sinceSeen, r.URL.Query().Get("since"))
		w.Header().Set("Last-Modified-Version", version)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	// First sync: no checkpoint yet, and the reported version is stored.
	if res := syncResource(context.Background(), syncTestClient(srv.URL), db, "items", 0, false, 0, false); res.Err != nil {
		t.Fatalf("first sync error: %v", res.Err)
	}
	if sinceSeen[0] != "" {
		t.Errorf("first sync sent since=%q, want empty (no checkpoint yet)", sinceSeen[0])
	}
	if v, _, err := db.StoredLibraryVersion("items"); err != nil || v != 100 {
		t.Fatalf("stored library version = %d (err %v), want 100", v, err)
	}

	// Second sync: the stored checkpoint is sent as an integer `since`.
	version, body = "150", `[{"id":"c"}]`
	if res := syncResource(context.Background(), syncTestClient(srv.URL), db, "items", 0, false, 0, false); res.Err != nil {
		t.Fatalf("second sync error: %v", res.Err)
	}
	if sinceSeen[1] != "100" {
		t.Errorf("second sync sent since=%q, want \"100\" (stored checkpoint)", sinceSeen[1])
	}

	// An explicit --since version overrides the stored checkpoint.
	body = `[{"id":"d"}]`
	if res := syncResource(context.Background(), syncTestClient(srv.URL), db, "items", 4521, false, 0, false); res.Err != nil {
		t.Fatalf("third sync error: %v", res.Err)
	}
	if sinceSeen[2] != "4521" {
		t.Errorf("explicit --since sent since=%q, want \"4521\"", sinceSeen[2])
	}
}

// A checkpoint is only meaningful against the plane that issued it. Replaying a
// web-API version against the local desktop API matched nothing and froze the
// mirror indefinitely, so a plane change must fall back to a full pass.
func TestSyncResourceDiscardsForeignPlaneCheckpoint(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "12689")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"a"}]`))
	}))
	defer first.Close()
	if res := syncResource(context.Background(), syncTestClient(first.URL), db, "items", 0, false, 0, false); res.Err != nil {
		t.Fatalf("first sync error: %v", res.Err)
	}

	var since string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		since = r.URL.Query().Get("since")
		w.Header().Set("Last-Modified-Version", "71")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"b"}]`))
	}))
	defer second.Close()
	if res := syncResource(context.Background(), syncTestClient(second.URL), db, "items", 0, false, 0, false); res.Err != nil {
		t.Fatalf("second sync error: %v", res.Err)
	}
	if since != "" {
		t.Errorf("sent since=%q to a different plane; want a full pass", since)
	}
	// The new plane's much smaller version must replace the old one, not lose to
	// the monotonic guard.
	if v, _, err := db.StoredLibraryVersion("items"); err != nil || v != 71 {
		t.Fatalf("stored version = %d (err %v), want 71 from the current plane", v, err)
	}
}
func TestSyncResourceDiscardsCursorFromAnotherPlane(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":%s,"next_cursor":"100","has_more":true}`, syncTestItemsJSON("first", 100))
	}))
	defer first.Close()
	if res := syncResource(context.Background(), syncTestClient(first.URL), db, "items", 0, false, 1, false); res.Err != nil {
		t.Fatalf("first capped sync error: %v", res.Err)
	}

	var start, since string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start = r.URL.Query().Get("start")
		since = r.URL.Query().Get("since")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"SECOND","data":{"key":"SECOND","itemType":"book"}}]`))
	}))
	defer second.Close()
	if res := syncResource(context.Background(), syncTestClient(second.URL), db, "items", 0, false, 0, false); res.Err != nil {
		t.Fatalf("second-plane sync error: %v", res.Err)
	}
	if start != "0" || since != "" {
		t.Fatalf("second-plane query start=%q since=%q, want page zero without a foreign filter", start, since)
	}
}

func TestSyncResourceDoesNotResumePartialFullPassAsIncremental(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if len(queries) == 1 {
			fmt.Fprintf(w, `{"data":%s,"next_cursor":"100","has_more":true}`, syncTestItemsJSON("full", 100))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	client := syncTestClient(server.URL)
	if err := db.SaveLibraryVersion("items", client.Plane(), 40); err != nil {
		t.Fatalf("seed library version: %v", err)
	}

	first := syncResource(context.Background(), client, db, "items", 0, true, 1, false)
	if first.Err == nil || !strings.Contains(first.Err.Error(), "full sync incomplete") {
		t.Fatalf("partial full result = %+v, want a hard incomplete error", first)
	}
	if cursor, _, _, err := db.GetSyncState("items"); err != nil || cursor != "" {
		t.Fatalf("cursor after partial full pass = %q, %v; want none", cursor, err)
	}

	if second := syncResource(context.Background(), client, db, "items", 0, false, 0, false); second.Err != nil {
		t.Fatalf("incremental sync error: %v", second.Err)
	}
	if len(queries) != 2 {
		t.Fatalf("queries = %v, want two requests", queries)
	}
	secondQuery, err := url.ParseQuery(queries[1])
	if err != nil {
		t.Fatalf("parse second query: %v", err)
	}
	if got := secondQuery.Get("start"); got != "0" {
		t.Fatalf("incremental start = %q, want 0 after a partial full pass", got)
	}
	if got := secondQuery.Get("since"); got != "40" {
		t.Fatalf("incremental since = %q, want the prior complete checkpoint 40", got)
	}
}

func TestSyncFullFailureExitsNonZeroWithSuccessfulResource(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	fastRetryBackoff(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users/0/items":
			_, _ = w.Write([]byte(`[{"key":"ITEM1","version":1,"data":{"key":"ITEM1","itemType":"book"}}]`))
		case "/users/0/collections":
			http.Error(w, "simulated full-sync failure", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cmd := newSyncCmd(&rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		timeout:    time.Second,
	})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--full", "--resources", "items,collections", "--db", filepath.Join(t.TempDir(), "sync.db")})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "incomplete during full sync") {
		t.Fatalf("full sync error = %v, want non-zero incomplete result when one resource succeeds", err)
	}
}

func TestSyncFullAccessWarningExitsNonZeroWithSuccessfulResource(t *testing.T) {
	syncTestWithHumanFriendly(t, false)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users/0/items":
			_, _ = w.Write([]byte(`[{"key":"ITEM1","version":1,"data":{"key":"ITEM1","itemType":"book"}}]`))
		case "/users/0/collections":
			http.Error(w, "simulated access denial", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cmd := newSyncCmd(&rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		timeout:    time.Second,
	})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--full", "--resources", "items,collections", "--db", filepath.Join(t.TempDir(), "sync.db")})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "incomplete during full sync") {
		t.Fatalf("full sync error = %v, want non-zero incomplete result for an access warning", err)
	}
}

// The cursor fix alone is not enough: row upserts are version-monotonic, so a
// row still carrying a web-API version (11973) rejects every local-plane row
// (version 65) as "older" and never refreshes, freezing the mirror even during a
// full pass. A plane change must reset those versions.
func TestSyncRefreshesRowsHoldingForeignPlaneVersions(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	// Seed the mirror as an earlier web-plane sync left it: a high version and
	// the now-outdated tag casing.
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":11973,"data":{"key":"K1","itemType":"journalArticle","tags":[{"tag":"depression","type":0}]}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.SaveLibraryVersion("items", "https://api.zotero.org/users/5847066", 12689); err != nil {
		t.Fatalf("seed foreign checkpoint: %v", err)
	}

	// The local plane reports the same item with its own small version.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "65")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1","version":65,"data":{"key":"K1","itemType":"journalArticle","tags":[{"tag":"Depression","type":0}]}}]`))
	}))
	defer srv.Close()

	if res := syncResource(context.Background(), syncTestClient(srv.URL), db, "items", 0, false, 1, false); res.Err != nil {
		t.Fatalf("sync error: %v", res.Err)
	}

	rows, err := (localQueryStore{db}).QueryRaw(
		`SELECT json_extract(data,'$.data.tags[0].tag') AS tag FROM resources WHERE resource_type='items' AND id='K1'`)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read back: rows=%v err=%v", rows, err)
	}
	if got := sqlStringValue(rows[0]["tag"]); got != "Depression" {
		t.Fatalf("mirrored tag = %q, want Depression: the row was frozen by a foreign-plane version", got)
	}
}

// The local desktop API omits Last-Modified-Version on /items. Stamping the
// cursor's plane only when a version arrived left cursor_source NULL forever for
// exactly that resource, so every sync re-detected a "plane change" and re-wiped
// the stored row versions — permanently voiding the version-monotonic upsert
// guard and rewriting the whole table on every run instead of once.
func TestSyncConvergesPlaneWithoutVersionHeader(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	// No Last-Modified-Version header, and no version on the rows.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1","data":{"key":"K1","itemType":"journalArticle"}}]`))
	}))
	defer srv.Close()

	client := syncTestClient(srv.URL)
	if res := syncResource(context.Background(), client, db, "items", 0, false, 1, false); res.Err != nil {
		t.Fatalf("first sync error: %v", res.Err)
	}

	// After one pass the plane must be recorded, even though the version is 0.
	changed, err := db.PlaneChanged("items", client.Plane())
	if err != nil {
		t.Fatalf("PlaneChanged: %v", err)
	}
	if changed {
		t.Fatal("plane still reads as changed after a full pass: every sync would re-wipe stored row versions")
	}
	if _, source, serr := db.StoredLibraryVersion("items"); serr != nil || source != client.Plane() {
		t.Fatalf("cursor_source = %q (err %v), want the plane that just synced", source, serr)
	}
}

func TestSyncFullTopLevelAliasDoesNotReapChildRows(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"PARENT","version":1,"data":{"key":"PARENT","itemType":"book"}}`),
		json.RawMessage(`{"key":"CHILD","version":1,"data":{"key":"CHILD","parentItem":"PARENT","itemType":"attachment"}}`),
	}); err != nil {
		t.Fatalf("seed parent and child rows: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/items/top" && r.URL.Path != "/items" {
			t.Errorf("server path = %q, want /items/top or /items", r.URL.Path)
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"PARENT","version":2,"data":{"key":"PARENT","itemType":"book"}}]`))
	}))
	defer server.Close()

	client := syncTestClient(server.URL)
	if res := syncResource(context.Background(), client, db, "items-top", 0, true, 0, false); res.Err != nil {
		t.Fatalf("full items-top sync error: %v", res.Err)
	}
	ids, err := db.ResourceIDs("items")
	if err != nil {
		t.Fatalf("items ResourceIDs after alias sync: %v", err)
	}
	if !ids["CHILD"] {
		t.Fatal("full items-top sync reaped a child row from the canonical items store")
	}

	if res := syncResource(context.Background(), client, db, "items", 0, true, 0, false); res.Err != nil {
		t.Fatalf("full items sync error: %v", res.Err)
	}
	ids, err = db.ResourceIDs("items")
	if err != nil {
		t.Fatalf("items ResourceIDs after canonical sync: %v", err)
	}
	if ids["CHILD"] {
		t.Fatal("full items sync did not reap the child row missing from the complete response")
	}
	if !ids["PARENT"] {
		t.Fatal("full items sync reaped the parent row returned by the complete response")
	}
}

func TestSyncUsesBodyVersionWhenHeaderIsMissing(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	var sinceSeen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinceSeen = append(sinceSeen, r.URL.Query().Get("since"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"key":"A","version":12,"data":{"key":"A","itemType":"book"}},
			{"key":"B","version":19,"data":{"key":"B","itemType":"book"}}
		]`))
	}))
	defer server.Close()

	client := syncTestClient(server.URL)
	if res := syncResource(context.Background(), client, db, "items", 0, false, 0, false); res.Err != nil {
		t.Fatalf("first sync error: %v", res.Err)
	}
	if v, _, err := db.StoredLibraryVersion("items"); err != nil || v != 19 {
		t.Fatalf("stored library version = %d (err %v), want page maximum 19", v, err)
	}

	if res := syncResource(context.Background(), client, db, "items", 0, false, 0, false); res.Err != nil {
		t.Fatalf("second sync error: %v", res.Err)
	}
	if len(sinceSeen) != 2 {
		t.Fatalf("request count = %d, want 2", len(sinceSeen))
	}
	if sinceSeen[0] != "" {
		t.Errorf("first sync sent since=%q, want empty", sinceSeen[0])
	}
	if sinceSeen[1] != "19" {
		t.Errorf("second sync sent since=%q, want stored body version 19", sinceSeen[1])
	}
}

// N4-3: sync --full did not reap items-trash, so the X-3 phantom returned. A
// genuinely empty trash (0 entries on both planes) completed a full pass with
// zero seen keys, and the sweep's old len(seenKeys) > 0 guard treated that as
// "the fetch may have failed" and skipped reaping — leaving a row with no
// corresponding item on either plane unreapable by any command, forever.
func TestSyncReapsPhantomFromAGenuinelyEmptyResource(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	// A trashed item's mirror row survives after the underlying item is gone
	// from both planes — exactly the WH3JEEWH scenario.
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		json.RawMessage(`{"key":"PHANTOM","version":1,"data":{"key":"PHANTOM","itemType":"book"}}`),
	}); err != nil {
		t.Fatalf("seed phantom trash row: %v", err)
	}

	// The plane genuinely reports an empty trash — not a fetch failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	if res := syncResource(context.Background(), syncTestClient(srv.URL), db, "items-trash", 0, true, 0, false); res.Err != nil {
		t.Fatalf("sync error: %v", res.Err)
	}

	ids, err := db.ResourceIDs("items-trash")
	if err != nil {
		t.Fatalf("ResourceIDs: %v", err)
	}
	if ids["PHANTOM"] {
		t.Fatal("phantom items-trash row survived a full sync of a genuinely empty trash")
	}
}

// A request failure must never be misread as "the resource is empty": nothing
// may be swept on an incomplete pass.
func TestSyncDoesNotSweepOnAFailedPass(t *testing.T) {
	fastRetryBackoff(t)
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"SURVIVES","version":1,"data":{"key":"SURVIVES","itemType":"book"}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	res := syncResource(context.Background(), syncTestClient(srv.URL), db, "items", 0, true, 0, false)
	if res.Err == nil {
		t.Fatal("expected a sync error from the 500, got none")
	}

	ids, err := db.ResourceIDs("items")
	if err != nil {
		t.Fatalf("ResourceIDs: %v", err)
	}
	if !ids["SURVIVES"] {
		t.Fatal("a failed pass reaped a row it never should have touched")
	}
}

func TestSyncPageExtractionHelpers(t *testing.T) {
	items, cursor, hasMore, isPage, err := extractPageItemsWithError(json.RawMessage(`[{"id":"a"},{"id":"b"}]`), "after")
	if len(items) != 2 || cursor != "" || hasMore || !isPage || err != nil {
		t.Fatalf("bare array extraction = len %d cursor %q hasMore %v isPage %v err %v, want len 2 empty false true nil", len(items), cursor, hasMore, isPage, err)
	}

	items, cursor, hasMore, isPage, err = extractPageItemsWithError(json.RawMessage(`{"data":[{"id":"a"}],"next_cursor":"n1","has_more":true}`), "after")
	if len(items) != 1 || cursor != "n1" || !hasMore || !isPage || err != nil {
		t.Fatalf("data envelope extraction = len %d cursor %q hasMore %v isPage %v err %v, want len 1 n1 true true nil", len(items), cursor, hasMore, isPage, err)
	}

	items, cursor, hasMore, isPage, err = extractPageItemsWithError(json.RawMessage(`{"widgets":[{"id":"a"}],"response_metadata":{"next_cursor":"nested"}}`), "after")
	if len(items) != 1 || cursor != "nested" || !hasMore || !isPage || err != nil {
		t.Fatalf("fallback envelope extraction = len %d cursor %q hasMore %v isPage %v err %v, want len 1 nested true true nil", len(items), cursor, hasMore, isPage, err)
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

func TestPageMaxObjectVersion(t *testing.T) {
	tests := []struct {
		name string
		data string
		want int
	}{
		{
			name: "top-level version",
			data: `[{"version":17}]`,
			want: 17,
		},
		{
			name: "nested-only data version",
			data: `[{"data":{"version":23}}]`,
			want: 23,
		},
		{
			name: "mixed versions",
			data: `[{"version":5},{"data":{"version":31}},{"version":19,"data":{"version":7}}]`,
			want: 31,
		},
		{
			name: "empty array",
			data: `[]`,
			want: 0,
		},
		{
			name: "non-array object",
			data: `{"version":41}`,
			want: 0,
		},
		{
			name: "malformed JSON",
			data: `[{"version":41}`,
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pageMaxObjectVersion(json.RawMessage(tt.data)); got != tt.want {
				t.Fatalf("pageMaxObjectVersion(%s) = %d, want %d", tt.data, got, tt.want)
			}
		})
	}
}

func TestNormalizeSyncResources(t *testing.T) {
	defaults := defaultSyncResources()
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "items only adds trash",
			in:   []string{"items"},
			want: []string{"items", "items-trash"},
		},
		{
			name: "items with trash stays ordered",
			in:   []string{"items", "items-trash"},
			want: []string{"items", "items-trash"},
		},
		{
			name: "duplicate inputs do not duplicate work",
			in:   []string{"collections", "items", "items", "items-trash", "items-trash", "collections"},
			want: []string{"collections", "items", "items-trash"},
		},
		{
			name: "trash only remains independent",
			in:   []string{"items-trash"},
			want: []string{"items-trash"},
		},
		{
			name: "unrelated resources stay unchanged",
			in:   []string{"collections", "tags"},
			want: []string{"collections", "tags"},
		},
		{
			name: "defaults stay unchanged",
			in:   defaults,
			want: defaults,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeSyncResources(tt.in); !slices.Equal(got, tt.want) {
				t.Fatalf("normalizeSyncResources(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
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

	// Zotero tags use a composite (name,type) identity; global schema lists use
	// domain-name keys that are not in the generic ID fallback list.
	idCases := []struct {
		resource string
		obj      map[string]any
		want     string
	}{
		{"tags", map[string]any{"tag": "foo"}, "3:foo:0"},
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

// schema sync must request global Zotero endpoints and
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
	result := syncResource(context.Background(), syncTestClient(server.URL+"/users/0"), db, "schema", 0, false, 0, false)
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
	result := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, false, 0, false)
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
	result := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, false, 0, false)
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
	result := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, false, 1, false)
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
	result := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, false, 0, false)
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
	result := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, false, 0, false)
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

func TestSyncResourceErrorEventEscapesControlCharacters(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	wantErr := "failed with backslash \\\nraw escape \x1b byte"
	db := syncTestOpenStore(t)
	defer db.Close()

	var output bytes.Buffer
	ctx := context.WithValue(context.Background(), syncEventWriterContextKey{}, &output)
	result := syncResource(ctx, syncTestErrorClient{err: fmt.Errorf("%s", wantErr)}, db, "items", 0, false, 0, false)
	if result.Err == nil {
		t.Fatal("syncResource Err = nil, want error")
	}
	lines := bytes.Split(bytes.TrimSuffix(output.Bytes(), []byte("\n")), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("stdout lines = %d (%q), want sync_start and one sync_error line", len(lines), bytes.Join(lines, []byte("|")))
	}

	var event struct {
		Event    string `json:"event"`
		Resource string `json:"resource"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(lines[1], &event); err != nil {
		t.Fatalf("sync_error line is not valid JSON: %v; line=%q", err, lines[1])
	}
	if event.Event != "sync_error" || event.Resource != "items" || event.Error != wantErr {
		t.Fatalf("sync_error event = %#v, want event sync_error resource items error %q", event, wantErr)
	}
}

func TestProcessDequeuedSyncResourceStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := make(chan syncResult, 1)
	called := false

	if processDequeuedSyncResource(ctx, "items", results, func(resource string) syncResult {
		called = true
		return syncResult{Resource: resource, Count: 1}
	}) {
		t.Fatal("processDequeuedSyncResource returned true for canceled context")
	}
	if called {
		t.Fatal("sync function was called after context cancellation")
	}
	// The resource is already out of the work channel, so this call is its only
	// chance to appear in the result set. Dropping it made a cancelled sync
	// report fewer resources than it was given, which reads as "never asked
	// for" rather than "not attempted".
	select {
	case result := <-results:
		if result.Resource != "items" {
			t.Fatalf("cancellation result = %#v, want resource items", result)
		}
		if !errors.Is(result.Err, context.Canceled) {
			t.Fatalf("cancellation result error = %v, want context.Canceled", result.Err)
		}
		if result.Count != 0 {
			t.Fatalf("cancellation result count = %d, want 0", result.Count)
		}
	default:
		t.Fatal("cancellation produced no result; the resource would vanish from the report")
	}

	activeResults := make(chan syncResult, 1)
	if !processDequeuedSyncResource(context.Background(), "items", activeResults, func(resource string) syncResult {
		return syncResult{Resource: resource, Count: 1}
	}) {
		t.Fatal("processDequeuedSyncResource returned false for active context")
	}
	if got := <-activeResults; got.Resource != "items" || got.Count != 1 {
		t.Fatalf("active sync result = %#v, want items count 1", got)
	}
}

type syncTestErrorClient struct {
	err error
}

func (c syncTestErrorClient) GetWithVersion(string, map[string]string) (json.RawMessage, int, error) {
	return nil, 0, c.err
}

func (c syncTestErrorClient) GetWithVersionContext(context.Context, string, map[string]string) (json.RawMessage, int, error) {
	return nil, 0, c.err
}

func (c syncTestErrorClient) RateLimit() float64 {
	return 0
}

func (c syncTestErrorClient) Plane() string {
	return "test://error-client"
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
