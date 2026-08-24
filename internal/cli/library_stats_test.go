// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"
)

func TestQueryLibraryItemsAddedGroupsCiteableItems(t *testing.T) {
	db, err := store.OpenWithContext(t.Context(), filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"A","data":{"key":"A","itemType":"journalArticle","dateAdded":"2026-08-24T01:00:00Z"}}`),
		json.RawMessage(`{"key":"B","data":{"key":"B","itemType":"book","dateAdded":"2026-08-01T02:00:00Z"}}`),
		json.RawMessage(`{"key":"C","data":{"key":"C","itemType":"report","dateAdded":"2026-07-31T23:00:00Z"}}`),
		json.RawMessage(`{"key":"ATT","data":{"key":"ATT","itemType":"attachment","parentItem":"A","dateAdded":"2026-08-24T01:01:00Z"}}`),
		json.RawMessage(`{"key":"NO-DATE","data":{"key":"NO-DATE","itemType":"document"}}`),
	}); err != nil {
		t.Fatalf("seed statistics items: %v", err)
	}
	queryDB := localQueryStore{db}

	months, err := queryLibraryItemsAdded(queryDB, "month")
	if err != nil {
		t.Fatalf("query monthly intake: %v", err)
	}
	if len(months) != 2 || months[0] != (libraryAddedCount{Period: "2026-08", Count: 2}) || months[1] != (libraryAddedCount{Period: "2026-07", Count: 1}) {
		t.Fatalf("monthly intake = %+v", months)
	}

	years, err := queryLibraryItemsAdded(queryDB, "year")
	if err != nil {
		t.Fatalf("query yearly intake: %v", err)
	}
	if len(years) != 1 || years[0] != (libraryAddedCount{Period: "2026", Count: 3}) {
		t.Fatalf("yearly intake = %+v", years)
	}
}

func TestQueryLibraryStatsIncludesRequestedIntakeBucket(t *testing.T) {
	db, err := store.OpenWithContext(t.Context(), filepath.Join(t.TempDir(), "stats-envelope.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"A","data":{"key":"A","itemType":"journalArticle","date":"2025","dateAdded":"2026-08-24T01:00:00Z"}}`),
	}); err != nil {
		t.Fatalf("seed statistics item: %v", err)
	}

	stats, err := queryLibraryStats(localQueryStore{db}, 10, 20, "month")
	if err != nil {
		t.Fatalf("query library stats: %v", err)
	}
	if stats.AddedBy != "month" || len(stats.ItemsAdded) != 1 || stats.ItemsAdded[0].Period != "2026-08" {
		t.Fatalf("stats intake = added_by %q rows %+v", stats.AddedBy, stats.ItemsAdded)
	}
}

func TestLibraryStatsRejectsUnknownAddedBy(t *testing.T) {
	cmd := newLibraryStatsCmd(&rootFlags{})
	cmd.SetArgs([]string{"--added-by", "week"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--added-by must be month or year") {
		t.Fatalf("Execute() error = %v", err)
	}
}
