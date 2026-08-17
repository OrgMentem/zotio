// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Venue year extraction should use meta.parsedDate when data.date is freeform and
// would otherwise yield garbage year columns.

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

func TestQueryItemVenuesUsesParsedDateForYear(t *testing.T) {
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
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

func TestQueryItemVenuesFreeformDatesYearsExcludeUndatable(t *testing.T) {
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Four items in the same venue with the exact freeform dates from the spec.
	// "April 2023" must not produce "Apri" via SUBSTR(...,1,4); "n.d." must be
	// excluded (no year) rather than contributing "n.d." to min/max.
	items := []json.RawMessage{
		json.RawMessage(`{"key":"F1","version":1,"data":{"key":"F1","itemType":"journalArticle","title":"T1","publicationTitle":"Journal of Freeform","date":"2023-04"}}`),
		json.RawMessage(`{"key":"F2","version":1,"data":{"key":"F2","itemType":"journalArticle","title":"T2","publicationTitle":"Journal of Freeform","date":"April 2023"}}`),
		json.RawMessage(`{"key":"F3","version":1,"data":{"key":"F3","itemType":"journalArticle","title":"T3","publicationTitle":"Journal of Freeform","date":"n.d."}}`),
		json.RawMessage(`{"key":"F4","version":1,"data":{"key":"F4","itemType":"journalArticle","title":"T4","publicationTitle":"Journal of Freeform","date":"2023"}}`),
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
	row := rows[0]
	if c := sqlIntValue(row["count"]); c != 4 {
		t.Fatalf("count = %d, want 4 (row=%v)", c, row)
	}
	if got := sqlStringValue(row["min_year"]); got != "2023" {
		t.Fatalf("min_year = %q, want 2023 (row=%v)", got, row)
	}
	if got := sqlStringValue(row["max_year"]); got != "2023" {
		t.Fatalf("max_year = %q, want 2023 (row=%v)", got, row)
	}
	// Guard explicitly against the pre-fix SUBSTR garbage values.
	for _, bad := range []string{"Apri", "n.d."} {
		if sqlStringValue(row["min_year"]) == bad || sqlStringValue(row["max_year"]) == bad {
			t.Fatalf("year contains bare SUBSTR garbage %q (row=%v)", bad, row)
		}
	}
	// Venue populated only with undatable rows must yield NULL years, not garbage.
	t.Run("undatable_only_yields_nil", func(t *testing.T) {
		savedGroup2 := activeGroupIDLocked()
		setActiveGroupID("")
		t.Cleanup(func() { setActiveGroupID(savedGroup2) })
		t.Setenv("HOME", t.TempDir())
		t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
		db2, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = db2.Close() })
		undatable := []json.RawMessage{
			json.RawMessage(`{"key":"U1","version":1,"data":{"key":"U1","itemType":"journalArticle","title":"U1","publicationTitle":"Undatable Venue","date":"n.d."}}`),
			json.RawMessage(`{"key":"U2","version":1,"data":{"key":"U2","itemType":"journalArticle","title":"U2","publicationTitle":"Undatable Venue","date":"forthcoming"}}`),
		}
		if _, _, err := db2.UpsertBatch("items", undatable); err != nil {
			t.Fatalf("seed undatable: %v", err)
		}
		rows2, err := queryItemVenues(localQueryStore{db2}, "", 0)
		if err != nil {
			t.Fatalf("queryItemVenues: %v", err)
		}
		if len(rows2) != 1 {
			t.Fatalf("rows2 = %v, want one venue", rows2)
		}
		if rows2[0]["min_year"] != nil {
			t.Fatalf("min_year = %v, want nil for undatable-only venue (rows=%v)", rows2[0]["min_year"], rows2)
		}
		if rows2[0]["max_year"] != nil {
			t.Fatalf("max_year = %v, want nil for undatable-only venue (rows=%v)", rows2[0]["max_year"], rows2)
		}
	})
}

func TestQueryItemVenuesDeterministicItemType(t *testing.T) {
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Same venue via publisher fallback, two different itemTypes.
	// The old query selected a bare json_extract(data,'$.data.itemType') under
	// GROUP BY venue, so SQLite picked an arbitrary row's type. The fix must be
	// deterministic (modal, lexicographically smallest on tie => "book").
	sharedVenue := "Shared Venue"
	items := []json.RawMessage{
		json.RawMessage(`{"key":"M1","version":1,"data":{"key":"M1","itemType":"journalArticle","title":"T1","publisher":"` + sharedVenue + `","date":"2020"}}`),
		json.RawMessage(`{"key":"M2","version":1,"data":{"key":"M2","itemType":"book","title":"T2","publisher":"` + sharedVenue + `","date":"2021"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}

	for iter := 0; iter < 3; iter++ {
		rows, err := queryItemVenues(localQueryStore{db}, "", 0)
		if err != nil {
			t.Fatalf("queryItemVenues iter %d: %v", iter, err)
		}
		if len(rows) != 1 {
			t.Fatalf("iter %d: rows = %v, want one venue", iter, rows)
		}
		got := sqlStringValue(rows[0]["item_type"])
		// "book" < "journalArticle" lexicographically, so on a 1-1 tie the
		// deterministic choice must be "book".
		if got != "book" {
			t.Fatalf("iter %d: item_type = %q, want deterministic \"book\" (rows=%v)", iter, got, rows)
		}
	}

	// Modal case: 2 journalArticle + 1 book => modal is journalArticle.
	t.Run("modal_wins", func(t *testing.T) {
		savedGroup2 := activeGroupIDLocked()
		setActiveGroupID("")
		t.Cleanup(func() { setActiveGroupID(savedGroup2) })
		t.Setenv("HOME", t.TempDir())
		t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
		db2, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = db2.Close() })
		more := []json.RawMessage{
			json.RawMessage(`{"key":"N1","version":1,"data":{"key":"N1","itemType":"journalArticle","title":"N1","publisher":"Modal Venue","date":"2020"}}`),
			json.RawMessage(`{"key":"N2","version":1,"data":{"key":"N2","itemType":"journalArticle","title":"N2","publisher":"Modal Venue","date":"2021"}}`),
			json.RawMessage(`{"key":"N3","version":1,"data":{"key":"N3","itemType":"book","title":"N3","publisher":"Modal Venue","date":"2022"}}`),
		}
		if _, _, err := db2.UpsertBatch("items", more); err != nil {
			t.Fatalf("seed modal: %v", err)
		}
		rows, err := queryItemVenues(localQueryStore{db2}, "", 0)
		if err != nil {
			t.Fatalf("queryItemVenues modal: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("modal rows = %v, want one", rows)
		}
		if got := sqlStringValue(rows[0]["item_type"]); got != "journalArticle" {
			t.Fatalf("modal item_type = %q, want journalArticle (rows=%v)", got, rows)
		}
	})
}
