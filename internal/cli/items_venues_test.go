// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean bugfix): cover venue year extraction from meta.parsedDate when
// data.date is freeform and would otherwise yield garbage year columns.

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

func TestQueryItemVenuesUsesParsedDateForYear(t *testing.T) {
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	items := []json.RawMessage{
		json.RawMessage(`{"key":"V1","version":1,"data":{"key":"V1","itemType":"journalArticle","title":"Venue Paper","publicationTitle":"Journal of Dates","date":"01/2024"},"meta":{"parsedDate":"2024-01"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}

	rows, err := queryItemVenues(localQueryStore{db}, "", 0)
	if err != nil {
		t.Fatalf("queryItemVenues: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one venue", rows)
	}
	if got := sqlStringValue(rows[0]["min_year"]); got != "2024" {
		t.Fatalf("min_year = %q, want 2024 (rows=%v)", got, rows)
	}
	if got := sqlStringValue(rows[0]["max_year"]); got != "2024" {
		t.Fatalf("max_year = %q, want 2024 (rows=%v)", got, rows)
	}
}
