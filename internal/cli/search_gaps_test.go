// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean test-gaps f6yb): covers search result extraction, empty filtering, and command API/local paths.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"zotero-pp-cli/internal/store"
)

func TestSearchIsNilOrEmpty(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{name: "nil", raw: nil, want: true},
		{name: "null", raw: json.RawMessage(`null`), want: true},
		{name: "empty array", raw: json.RawMessage(`[]`), want: true},
		{name: "empty object", raw: json.RawMessage(`{}`), want: true},
		{name: "whitespace", raw: json.RawMessage(`   `), want: true},
		{name: "blank title", raw: json.RawMessage(`{"title":" \t "}`), want: true},
		{name: "non-empty title", raw: json.RawMessage(`{"title":"Zotero paper"}`), want: false},
		{name: "numeric id", raw: json.RawMessage(`{"id":42}`), want: false},
		{name: "nested document slug", raw: json.RawMessage(`{"score":0.9,"document":{"slug":"paper-1"}}`), want: false},
		{name: "score only", raw: json.RawMessage(`{"score":0}`), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNilOrEmpty(tt.raw); got != tt.want {
				t.Fatalf("isNilOrEmpty(%s) = %v, want %v", string(tt.raw), got, tt.want)
			}
		})
	}
}

func TestSearchExtractSearchResultsShapes(t *testing.T) {
	tests := []struct {
		name      string
		data      json.RawMessage
		wantCount int
		wantFirst string
	}{
		{
			name:      "bare array",
			data:      json.RawMessage(`[{"id":"a"},{"id":"b"}]`),
			wantCount: 2,
			wantFirst: `{"id":"a"}`,
		},
		{
			name:      "data envelope",
			data:      json.RawMessage(`{"data":[{"id":"data-1"}]}`),
			wantCount: 1,
			wantFirst: `{"id":"data-1"}`,
		},
		{
			name:      "results envelope",
			data:      json.RawMessage(`{"results":[{"id":"results-1"}]}`),
			wantCount: 1,
			wantFirst: `{"id":"results-1"}`,
		},
		{
			name:      "items envelope",
			data:      json.RawMessage(`{"items":[{"id":"items-1"}]}`),
			wantCount: 1,
			wantFirst: `{"id":"items-1"}`,
		},
		{
			name:      "records envelope",
			data:      json.RawMessage(`{"records":[{"id":"records-1"}]}`),
			wantCount: 1,
			wantFirst: `{"id":"records-1"}`,
		},
		{
			name:      "entries envelope",
			data:      json.RawMessage(`{"entries":[{"id":"entries-1"}]}`),
			wantCount: 1,
			wantFirst: `{"id":"entries-1"}`,
		},
		{
			name:      "single object",
			data:      json.RawMessage(`{"id":"single"}`),
			wantCount: 1,
			wantFirst: `{"id":"single"}`,
		},
		{
			name:      "empty array",
			data:      json.RawMessage(`[]`),
			wantCount: 0,
		},
		{
			name:      "garbage falls back to single raw item",
			data:      json.RawMessage(`not-json`),
			wantCount: 1,
			wantFirst: `not-json`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSearchResults(tt.data)
			if len(got) != tt.wantCount {
				t.Fatalf("len(extractSearchResults(%s)) = %d, want %d", string(tt.data), len(got), tt.wantCount)
			}
			if tt.wantCount > 0 && string(got[0]) != tt.wantFirst {
				t.Fatalf("first result = %s, want %s", string(got[0]), tt.wantFirst)
			}
		})
	}
}

func TestSearchCommandLiveFiltersEmptyResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/items" {
			t.Fatalf("path = %q, want /items", r.URL.Path)
		}
		if got := r.URL.Query().Get("q"); got != "paper" {
			t.Fatalf("q = %q, want paper", got)
		}
		if got := r.URL.Query().Get("qmode"); got != "everything" {
			t.Fatalf("qmode = %q, want everything", got)
		}
		if got := r.URL.Query().Get("limit"); got != "10" {
			t.Fatalf("limit = %q, want 10", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{}, {"key":"ITEM1","version":1,"data":{"title":"Paper One","itemType":"journalArticle"}}, {"document":{"slug":"wrapped-paper"}}]}`))
	}))
	defer server.Close()

	t.Setenv("ZOTERO_BASE_URL", server.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	flags := &rootFlags{asJSON: true, dataSource: "live", noCache: true, timeout: time.Second, rateLimit: 0}
	cmd := newSearchCmd(flags)
	cmd.SetArgs([]string{"paper", "--limit", "10"})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		Results []json.RawMessage `json:"results"`
		Meta    struct {
			Source string `json:"source"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output %q: %v", stdout.String(), err)
	}
	if got.Meta.Source != "live" {
		t.Fatalf("meta.source = %q, want live", got.Meta.Source)
	}
	if len(got.Results) != 2 {
		t.Fatalf("filtered result count = %d, want 2; output %s", len(got.Results), stdout.String())
	}
	// Compare semantically; the command emits indented JSON, so exact-byte
	// matches against compact strings are brittle.
	var r0 map[string]any
	if err := json.Unmarshal(got.Results[0], &r0); err != nil {
		t.Fatalf("unmarshal first result %q: %v", string(got.Results[0]), err)
	}
	if r0["key"] != "ITEM1" {
		t.Fatalf("first result fields = %+v", r0)
	}
	var r1 struct {
		Document struct {
			Slug string `json:"slug"`
		} `json:"document"`
	}
	if err := json.Unmarshal(got.Results[1], &r1); err != nil {
		t.Fatalf("unmarshal second result %q: %v", string(got.Results[1]), err)
	}
	if r1.Document.Slug != "wrapped-paper" {
		t.Fatalf("second result = %s", string(got.Results[1]))
	}
}

func TestSearchCommandAutoFallsBackToLocalOnNetworkError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "search.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Upsert("papers", "local-1", json.RawMessage(`{"id":"local-1","title":"Local Paper","abstract":"contains fallbackneedle"}`)); err != nil {
		t.Fatalf("upsert search fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("closed server should not receive requests")
	}))
	baseURL := server.URL
	server.Close()

	t.Setenv("ZOTERO_BASE_URL", baseURL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: 50 * time.Millisecond, rateLimit: 0}
	cmd := newSearchCmd(flags)
	cmd.SetArgs([]string{"fallbackneedle", "--type", "papers", "--db", dbPath, "--limit", "5"})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v; stderr %s", err, stderr.String())
	}

	var got struct {
		Results []json.RawMessage `json:"results"`
		Meta    struct {
			Source string `json:"source"`
			Reason string `json:"reason"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output %q: %v", stdout.String(), err)
	}
	if got.Meta.Source != "local" || got.Meta.Reason != "api_unreachable" {
		t.Fatalf("meta = %+v, want local/api_unreachable", got.Meta)
	}
	if len(got.Results) != 1 {
		t.Fatalf("fallback result count = %d, want 1; output %s", len(got.Results), stdout.String())
	}
	// Compare semantically: the local-store path emits indented JSON while the
	// live path is compact, so an exact-byte match is brittle.
	var fr map[string]any
	if err := json.Unmarshal(got.Results[0], &fr); err != nil {
		t.Fatalf("unmarshal fallback result %q: %v", string(got.Results[0]), err)
	}
	if fr["id"] != "local-1" || fr["title"] != "Local Paper" || fr["abstract"] != "contains fallbackneedle" {
		t.Fatalf("fallback result fields = %+v", fr)
	}
}
