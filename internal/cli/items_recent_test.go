// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

// recentPageRequest is one /items page fetch as the fixture saw it. The
// paginator's correctness lives in the requests it issues (start offsets, page
// size, sort order, server-side type filter) as much as in the rows it keeps,
// so every request is recorded and asserted on.
type recentPageRequest struct {
	path      string
	start     string
	limit     string
	itemType  string
	sort      string
	direction string
}

// recentItemsFixture serves staged /items page bodies in request order and
// records every request. A request beyond the staged bodies is answered with an
// empty array so the walk terminates instead of hanging: the recorded request
// sequence is then what proves the paginator asked for a page it must not have
// asked for.
type recentItemsFixture struct {
	mu       sync.Mutex
	bodies   []string
	requests []recentPageRequest
}

func (fx *recentItemsFixture) record(r *http.Request) int {
	query := r.URL.Query()
	fx.mu.Lock()
	defer fx.mu.Unlock()
	index := len(fx.requests)
	fx.requests = append(fx.requests, recentPageRequest{
		path:      r.URL.Path,
		start:     query.Get("start"),
		limit:     query.Get("limit"),
		itemType:  query.Get("itemType"),
		sort:      query.Get("sort"),
		direction: query.Get("direction"),
	})
	return index
}

func (fx *recentItemsFixture) recorded() []recentPageRequest {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return append([]recentPageRequest(nil), fx.requests...)
}

// newRecentItemsFixture starts the fixture and returns a client plus flags
// aimed at it. dataSource "live" is the flag that sends resolveRead down its
// remote branch, so the fixture really answers every page fetch and no local
// store or write-through cache is consulted.
func newRecentItemsFixture(t *testing.T, bodies ...string) (*recentItemsFixture, *client.Client, *rootFlags) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	fx := &recentItemsFixture{bodies: bodies}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := fx.record(r)
		body := "[]"
		if index < len(fx.bodies) {
			body = fx.bodies[index]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c := client.New(&config.Config{BaseURL: srv.URL + "/users/0"}, 5*time.Second, 0)
	c.NoCache = true
	return fx, c, &rootFlags{dataSource: "live", timeout: 5 * time.Second}
}

// recentRow is one fixture item before it is rendered into a page body.
type recentRow struct {
	key       string
	itemType  string
	dateAdded string
}

// recentItemsBody renders rows in the real Zotero envelope shape, with the
// fields nested under "data", which is the shape jsonStringFieldFromMap reads.
func recentItemsBody(t *testing.T, rows []recentRow) string {
	t.Helper()
	payload := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		payload = append(payload, map[string]any{
			"key":     row.key,
			"version": 1,
			"data": map[string]any{
				"key":       row.key,
				"itemType":  row.itemType,
				"dateAdded": row.dateAdded,
				"title":     "Title " + row.key,
			},
		})
	}
	out, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal fixture page: %v", err)
	}
	return string(out)
}

// recentItemsRows generates count rows with RFC3339 dateAdded values, so a
// full page never has to be written out by hand. dateAt must return strictly
// descending times, because that is the order the paginator asks the server
// for and the order its day cutoff depends on.
func recentItemsRows(prefix string, count int, itemTypeAt func(i int) string, dateAt func(i int) time.Time) []recentRow {
	rows := make([]recentRow, 0, count)
	for i := range count {
		rows = append(rows, recentRow{
			key:       fmt.Sprintf("%s%03d", prefix, i),
			itemType:  itemTypeAt(i),
			dateAdded: dateAt(i).UTC().Format(time.RFC3339),
		})
	}
	return rows
}

func recentItemsRowKeys(rows []recentRow) []string {
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.key)
	}
	return keys
}

func recentItemsResultKeys(t *testing.T, data json.RawMessage) []string {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("unmarshal fetchRecentItems result: %v (body %s)", err, data)
	}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		key, _ := row["key"].(string)
		keys = append(keys, key)
	}
	return keys
}

// TestFetchRecentItemsPaginator walks the fetchRecentItems paginator over
// staged /items pages and pins all four of its exits, plus the query it sends.
// The four exits are: a short page ends the loop, the requested limit counts
// accepted rows, the day cutoff returns at the first old row, and a bad page
// body fails as "parsing items response". Every case also asserts the request
// sequence, so a change that keeps the returned rows right while fetching the
// wrong pages still fails.
func TestFetchRecentItemsPaginator(t *testing.T) {
	now := time.Now().UTC()
	journalArticle := func(int) string { return "journalArticle" }
	// Alternating types feed the client-side filter rows it must reject.
	alternatingType := func(i int) string {
		if i%2 == 0 {
			return "journalArticle"
		}
		return "attachment"
	}
	minutesAgo := func(i int) time.Time { return now.Add(-time.Duration(i+1) * time.Minute) }

	// A full first page (exactly the production page size of 100) followed by a
	// short page: the paginator must ask for the second page at start=100 and
	// keep rows from both.
	firstPage := recentItemsRows("PGA", 100, journalArticle, minutesAgo)
	secondPage := recentItemsRows("PGB", 5, journalArticle, func(i int) time.Time {
		return now.Add(-time.Duration(101+i) * time.Minute)
	})

	// A full page of 100 rows where only the first three are inside a 7 day
	// window. The rest are 8 days old or older, so the descending sort lets the
	// cutoff return at row four. Any later page request means the early return
	// became a skip.
	cutoffPage := recentItemsRows("CUT", 100, journalArticle, func(i int) time.Time {
		if i < 3 {
			return now.Add(-time.Duration(i+1) * time.Hour)
		}
		return now.Add(-8*24*time.Hour - time.Duration(i)*time.Hour)
	})

	cases := []struct {
		name string
		// bodies are served one per request, in order.
		bodies        []string
		limit         int
		days          int
		itemType      string
		wantKeys      []string
		wantStarts    []string
		wantTypeParam string
		wantErr       string
	}{
		{
			// A full page must not end the walk; the short second page must.
			name:       "full page is followed by the next page at start 100",
			bodies:     []string{recentItemsBody(t, firstPage), recentItemsBody(t, secondPage)},
			limit:      0,
			wantKeys:   append(recentItemsRowKeys(firstPage), recentItemsRowKeys(secondPage)...),
			wantStarts: []string{"0", "100"},
		},
		{
			// limit 6 against a full page where every second row is rejected by
			// the client-side type filter. Six accepted rows must come back. A
			// limit applied to fetched rows instead would stop after six fetched
			// rows and return only three.
			name:          "limit counts accepted rows not fetched rows",
			bodies:        []string{recentItemsBody(t, recentItemsRows("LIM", 100, alternatingType, minutesAgo))},
			limit:         6,
			itemType:      "journalArticle",
			wantKeys:      []string{"LIM000", "LIM002", "LIM004", "LIM006", "LIM008", "LIM010"},
			wantStarts:    []string{"0"},
			wantTypeParam: "journalArticle",
		},
		{
			// The type reaches the server as a query parameter AND a wrong-type
			// row the fixture serves anyway is still dropped client-side.
			name: "wrong type row is dropped even though the server was asked to filter",
			bodies: []string{recentItemsBody(t, []recentRow{
				{key: "TYPJ1", itemType: "journalArticle", dateAdded: now.Add(-time.Hour).Format(time.RFC3339)},
				{key: "TYPA1", itemType: "attachment", dateAdded: now.Add(-2 * time.Hour).Format(time.RFC3339)},
				{key: "TYPJ2", itemType: "journalArticle", dateAdded: now.Add(-3 * time.Hour).Format(time.RFC3339)},
			})},
			limit:         0,
			itemType:      "journalArticle",
			wantKeys:      []string{"TYPJ1", "TYPJ2"},
			wantStarts:    []string{"0"},
			wantTypeParam: "journalArticle",
		},
		{
			// limit 0 removes the limit exit, so only the day cutoff can stop
			// this walk. It must return at the first old row and never request
			// the page after this full one.
			name:       "day cutoff returns at the first old row and asks for no further page",
			bodies:     []string{recentItemsBody(t, cutoffPage)},
			limit:      0,
			days:       7,
			wantKeys:   []string{"CUT000", "CUT001", "CUT002"},
			wantStarts: []string{"0"},
		},
		{
			// An unparseable dateAdded is skipped, and the walk continues to the
			// rows behind it instead of stopping there.
			name: "unparseable dateAdded skips the row without ending the walk",
			bodies: []string{recentItemsBody(t, []recentRow{
				{key: "DTOKA", itemType: "journalArticle", dateAdded: now.Add(-time.Hour).Format(time.RFC3339)},
				{key: "DTBAD", itemType: "journalArticle", dateAdded: "yesterday afternoon"},
				{key: "DTOKB", itemType: "journalArticle", dateAdded: now.Add(-3 * time.Hour).Format(time.RFC3339)},
			})},
			limit:      0,
			days:       7,
			wantKeys:   []string{"DTOKA", "DTOKB"},
			wantStarts: []string{"0"},
		},
		{
			name:       "page body that is not an array fails as a parse error",
			bodies:     []string{`{"error":"not an array"}`},
			limit:      0,
			wantStarts: []string{"0"},
			wantErr:    "parsing items response",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx, c, flags := newRecentItemsFixture(t, tc.bodies...)

			data, prov, err := fetchRecentItems(context.Background(), c, flags, tc.limit, tc.days, tc.itemType)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("fetchRecentItems error = %v, want error containing %q", err, tc.wantErr)
				}
				if data != nil {
					t.Fatalf("fetchRecentItems data = %s, want nil on error", data)
				}
			} else {
				if err != nil {
					t.Fatalf("fetchRecentItems: %v", err)
				}
				if prov.Source != "live" {
					t.Fatalf("provenance source = %q, want live: the fixture must be the data source", prov.Source)
				}
				gotKeys := recentItemsResultKeys(t, data)
				if !slices.Equal(gotKeys, tc.wantKeys) {
					t.Fatalf("returned keys = %v (%d rows), want %v (%d rows)", gotKeys, len(gotKeys), tc.wantKeys, len(tc.wantKeys))
				}
			}

			requests := fx.recorded()
			gotStarts := make([]string, 0, len(requests))
			for _, req := range requests {
				gotStarts = append(gotStarts, req.start)
			}
			if !slices.Equal(gotStarts, tc.wantStarts) {
				t.Fatalf("request start sequence = %v, want %v", gotStarts, tc.wantStarts)
			}
			for i, req := range requests {
				if req.path != "/users/0/items" {
					t.Fatalf("request %d path = %q, want /users/0/items", i, req.path)
				}
				// The cutoff exit is only correct while the server returns
				// newest first, so the sort must never drift silently.
				if req.sort != "dateAdded" || req.direction != "desc" {
					t.Fatalf("request %d sort/direction = %q/%q, want dateAdded/desc: the day cutoff assumes newest first", i, req.sort, req.direction)
				}
				if req.limit != "100" {
					t.Fatalf("request %d limit = %q, want 100 (the production page size)", i, req.limit)
				}
				if req.itemType != tc.wantTypeParam {
					t.Fatalf("request %d itemType = %q, want %q", i, req.itemType, tc.wantTypeParam)
				}
			}
		})
	}
}
