// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

func TestFindRecentlyAddedItemKey(t *testing.T) {
	floor := time.Date(2026, time.August, 22, 10, 0, 0, 0, time.UTC)
	after := floor.Add(time.Second).Format(time.RFC3339)
	before := floor.Add(-time.Second).Format(time.RFC3339)

	tests := []struct {
		name    string
		entries []map[string]any
		wantKey string
		wantN   int
	}{
		{
			name: "same title and type is ambiguous",
			entries: []map[string]any{
				{"key": "A", "title": "Paper", "itemType": "journalArticle", "dateAdded": after},
				{"key": "B", "title": "Paper", "itemType": "journalArticle", "dateAdded": after},
			},
			wantN: 2,
		},
		{
			name: "one recent match returns its key",
			entries: []map[string]any{
				{"key": "A", "title": "Paper", "itemType": "journalArticle", "dateAdded": after},
			},
			wantKey: "A",
			wantN:   1,
		},
		{
			name: "old same title does not match",
			entries: []map[string]any{
				{"key": "OLD", "title": "Paper", "itemType": "journalArticle", "dateAdded": before},
			},
			wantN: 0,
		},
		{
			name: "different item type does not match",
			entries: []map[string]any{
				{"key": "BOOK", "title": "Paper", "itemType": "book", "dateAdded": after},
			},
			wantN: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotQuery map[string][]string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/users/0/items/top" {
					t.Fatalf("request = %s %s, want GET /users/0/items/top", r.Method, r.URL.Path)
				}
				gotQuery = r.URL.Query()
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(tt.entries); err != nil {
					t.Fatalf("encode response: %v", err)
				}
			}))
			t.Cleanup(srv.Close)

			c := client.New(&config.Config{BaseURL: srv.URL + "/users/0"}, time.Second, 0)
			key, matched, err := findRecentlyAddedItemKey(c, "Paper", "journalArticle", floor)
			if err != nil {
				t.Fatalf("findRecentlyAddedItemKey: %v", err)
			}
			if key != tt.wantKey {
				t.Fatalf("key = %q, want %q", key, tt.wantKey)
			}
			if matched != tt.wantN {
				t.Fatalf("matched = %d, want %d", matched, tt.wantN)
			}
			wantQuery := map[string][]string{
				"sort":      {"dateAdded"},
				"direction": {"desc"},
				"limit":     {"50"},
			}
			if !reflect.DeepEqual(gotQuery, wantQuery) {
				t.Fatalf("query = %v, want %v", gotQuery, wantQuery)
			}
		})
	}
}
