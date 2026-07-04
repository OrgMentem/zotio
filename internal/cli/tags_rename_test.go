// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean tag-rename-pagination): cover multi-page tag rename candidate fetches.

package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

type tagRenamePageRequest struct {
	Start int
	Limit int
}

func TestListTagRenameUpdatesWalksMultiplePages(t *testing.T) {
	items := []string{
		`{"key":"K0","version":10,"data":{"key":"K0","tags":[{"tag":"old","type":0}]}}`,
		`{"key":"K1","version":11,"data":{"key":"K1","tags":[{"tag":"old","type":0}]}}`,
		`{"key":"K2","version":12,"data":{"key":"K2","tags":[{"tag":"old","type":0}]}}`,
		`{"key":"K3","version":13,"data":{"key":"K3","tags":[{"tag":"old","type":0}]}}`,
	}
	var requests []tagRenamePageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/users/0/items" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("tag"); got != "old" {
			t.Fatalf("tag query = %q, want old", got)
		}
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil {
			t.Fatalf("limit query = %q: %v", r.URL.Query().Get("limit"), err)
		}
		start, err := strconv.Atoi(r.URL.Query().Get("start"))
		if err != nil {
			t.Fatalf("start query = %q: %v", r.URL.Query().Get("start"), err)
		}
		requests = append(requests, tagRenamePageRequest{Start: start, Limit: limit})
		end := start + limit
		if start > len(items) {
			start = len(items)
		}
		if end > len(items) {
			end = len(items)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("["))
		for i, item := range items[start:end] {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			_, _ = w.Write([]byte(item))
		}
		_, _ = w.Write([]byte("]"))
	}))
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL + "/users/0"}, 5*time.Second, 0)
	c.NoCache = true
	updates, err := listTagRenameUpdates(c, "old", "new", 2)
	if err != nil {
		t.Fatalf("listTagRenameUpdates: %v", err)
	}

	wantRequests := []tagRenamePageRequest{{Start: 0, Limit: 2}, {Start: 2, Limit: 2}, {Start: 4, Limit: 2}}
	if len(requests) != len(wantRequests) {
		t.Fatalf("requests = %+v, want %+v", requests, wantRequests)
	}
	for i := range wantRequests {
		if requests[i] != wantRequests[i] {
			t.Fatalf("request %d = %+v, want %+v", i, requests[i], wantRequests[i])
		}
	}
	if len(updates) != len(items) {
		t.Fatalf("updates = %d, want %d", len(updates), len(items))
	}
	for i, update := range updates {
		if wantKey := "K" + strconv.Itoa(i); update.key != wantKey {
			t.Fatalf("update %d key = %q, want %q", i, update.key, wantKey)
		}
		raw, err := json.Marshal(update.tags)
		if err != nil {
			t.Fatalf("marshal tags: %v", err)
		}
		if string(raw) != `[{"tag":"new","type":0}]` {
			t.Fatalf("update %d tags = %s, want renamed tag only", i, raw)
		}
	}
}
