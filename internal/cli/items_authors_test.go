// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestQueryItemAuthorsCountsDistinctFilteredItems pins the observable contract
// of queryItemAuthors: it counts DISTINCT items rather than creator rows, it
// excludes attachment/note/annotation child rows, it groups creator identities
// (including single-field organization names) per creatorType, it honors the
// --type and --collection filters, it orders by item count descending, and it
// truncates with --top.
//
// The fixture gives every counted group a distinct COL1 count (5, 4, 3, 2, 1)
// so the descending order is fully determined for the collection-filtered and
// type-filtered queries. The unfiltered query has a genuine tie at 2, so it is
// compared as a set plus a monotonic-order check.
func TestQueryItemAuthorsCountsDistinctFilteredItems(t *testing.T) {
	items := []json.RawMessage{
		// I1 lists Curie TWICE. COUNT(DISTINCT i.id) must credit this item
		// once; COUNT(*) would credit it twice.
		json.RawMessage(`{"key":"I1","version":1,"data":{"key":"I1","itemType":"journalArticle","title":"I1","collections":["COL1"],"creators":[` +
			`{"creatorType":"author","firstName":"Marie","lastName":"Curie"},` +
			`{"creatorType":"author","firstName":"Marie","lastName":"Curie"},` +
			`{"creatorType":"author","firstName":"Niels","lastName":"Bohr"},` +
			`{"creatorType":"author","name":"World Health Organization"},` +
			`{"creatorType":"editor","firstName":"Otto","lastName":"Hahn"}]}}`),
		json.RawMessage(`{"key":"I2","version":1,"data":{"key":"I2","itemType":"journalArticle","title":"I2","collections":["COL1"],"creators":[` +
			`{"creatorType":"author","firstName":"Marie","lastName":"Curie"},` +
			`{"creatorType":"author","firstName":"Niels","lastName":"Bohr"},` +
			`{"creatorType":"author","name":"World Health Organization"},` +
			`{"creatorType":"editor","firstName":"Otto","lastName":"Hahn"}]}}`),
		json.RawMessage(`{"key":"I3","version":1,"data":{"key":"I3","itemType":"journalArticle","title":"I3","collections":["COL1"],"creators":[` +
			`{"creatorType":"author","firstName":"Marie","lastName":"Curie"},` +
			`{"creatorType":"author","firstName":"Niels","lastName":"Bohr"}]}}`),
		// Same person as both author and editor: creator_type is part of the
		// GROUP BY, so these are two distinct groups.
		json.RawMessage(`{"key":"I4","version":1,"data":{"key":"I4","itemType":"book","title":"I4","collections":["COL1"],"creators":[` +
			`{"creatorType":"author","firstName":"Marie","lastName":"Curie"},` +
			`{"creatorType":"editor","firstName":"Marie","lastName":"Curie"}]}}`),
		json.RawMessage(`{"key":"I7","version":1,"data":{"key":"I7","itemType":"conferencePaper","title":"I7","collections":["COL1"],"creators":[` +
			`{"creatorType":"author","firstName":"Marie","lastName":"Curie"},` +
			`{"creatorType":"author","firstName":"Niels","lastName":"Bohr"},` +
			`{"creatorType":"editor","firstName":"Otto","lastName":"Hahn"}]}}`),
		// An eligible item with no creators key at all: json_each over a
		// missing array must contribute no group.
		json.RawMessage(`{"key":"I9","version":1,"data":{"key":"I9","itemType":"journalArticle","title":"I9","collections":["COL1"]}}`),

		// Second collection: only reachable when --collection is COL2 or unset.
		json.RawMessage(`{"key":"I5","version":1,"data":{"key":"I5","itemType":"journalArticle","title":"I5","collections":["COL2"],"creators":[` +
			`{"creatorType":"author","firstName":"Max","lastName":"Planck"}]}}`),
		json.RawMessage(`{"key":"I6","version":1,"data":{"key":"I6","itemType":"book","title":"I6","collections":["COL2"],"creators":[` +
			`{"creatorType":"author","firstName":"Max","lastName":"Planck"},` +
			`{"creatorType":"author","firstName":"Marie","lastName":"Curie"}]}}`),

		// Five child rows in COL1 sharing one creator. If the itemType
		// exclusion regressed, "Sidecar, Sam" would appear with count 5 and
		// outrank every group except Curie.
		json.RawMessage(`{"key":"CA1","version":1,"data":{"key":"CA1","itemType":"attachment","parentItem":"I1","contentType":"application/pdf","collections":["COL1"],"creators":[` +
			`{"creatorType":"author","firstName":"Sam","lastName":"Sidecar"}]}}`),
		json.RawMessage(`{"key":"CA2","version":1,"data":{"key":"CA2","itemType":"attachment","parentItem":"I2","contentType":"application/pdf","collections":["COL1"],"creators":[` +
			`{"creatorType":"author","firstName":"Sam","lastName":"Sidecar"}]}}`),
		json.RawMessage(`{"key":"CN1","version":1,"data":{"key":"CN1","itemType":"note","parentItem":"I1","note":"n1","collections":["COL1"],"creators":[` +
			`{"creatorType":"author","firstName":"Sam","lastName":"Sidecar"}]}}`),
		json.RawMessage(`{"key":"CN2","version":1,"data":{"key":"CN2","itemType":"note","parentItem":"I2","note":"n2","collections":["COL1"],"creators":[` +
			`{"creatorType":"author","firstName":"Sam","lastName":"Sidecar"}]}}`),
		json.RawMessage(`{"key":"CX1","version":1,"data":{"key":"CX1","itemType":"annotation","parentItem":"CA1","annotationText":"a1","collections":["COL1"],"creators":[` +
			`{"creatorType":"author","firstName":"Sam","lastName":"Sidecar"}]}}`),
	}
	db := seedStoreWithItems(t, items)

	run := func(t *testing.T, creatorType, collection string, top int) []itemAuthorRow {
		t.Helper()
		rows, err := queryItemAuthors(db, creatorType, collection, top)
		if err != nil {
			t.Fatalf("queryItemAuthors(type=%q, collection=%q, top=%d): %v", creatorType, collection, top, err)
		}
		out, err := normalizeItemAuthorRows(rows)
		if err != nil {
			t.Fatalf("normalizeItemAuthorRows(type=%q, collection=%q, top=%d): %v", creatorType, collection, top, err)
		}
		return out
	}
	assertRows := func(t *testing.T, label string, got, want []itemAuthorRow) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s returned %d groups %+v, want %d groups %+v", label, len(got), got, len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s group %d = %+v, want %+v (full result %+v)", label, i, got[i], want[i], got)
			}
		}
	}

	// Whole library, no LIMIT. Planck and the WHO both land on 2 items, which
	// ORDER BY item_count DESC does not disambiguate, so compare as a set.
	all := run(t, "", "", 0)
	wantSet := map[string]int64{
		"Curie, Marie|author":              6, // I1 (listed twice), I2, I3, I4, I7, I6
		"Bohr, Niels|author":               4, // I1, I2, I3, I7
		"Hahn, Otto|editor":                3, // I1, I2, I7
		"Planck, Max|author":               2, // I5, I6
		"World Health Organization|author": 2, // I1, I2
		"Curie, Marie|editor":              1, // I4
	}
	gotSet := make(map[string]int64, len(all))
	for _, r := range all {
		key := r.DisplayName + "|" + r.CreatorType
		if _, dup := gotSet[key]; dup {
			t.Fatalf("creator group %q appeared twice in %+v; GROUP BY must collapse it", key, all)
		}
		gotSet[key] = r.ItemCount
	}
	for key, want := range wantSet {
		got, ok := gotSet[key]
		if !ok {
			t.Fatalf("creator group %q missing from %+v", key, all)
		}
		if got != want {
			t.Fatalf("creator group %q item_count = %d, want %d (COUNT(DISTINCT i.id), not creator rows); full result %+v", key, got, want, all)
		}
	}
	for key := range gotSet {
		if _, ok := wantSet[key]; !ok {
			t.Fatalf("unexpected creator group %q in %+v", key, all)
		}
	}
	for _, r := range all {
		if r.DisplayName == "Sidecar, Sam" {
			t.Fatalf("child-item creator %q leaked into %+v; attachment/note/annotation rows must be excluded", r.DisplayName, all)
		}
	}
	if len(all) == 0 {
		t.Fatal("unfiltered query returned no groups")
	}
	wantTop := itemAuthorRow{DisplayName: "Curie, Marie", CreatorType: "author", ItemCount: 6}
	if all[0] != wantTop {
		t.Fatalf("unfiltered first group = %+v, want %+v (ORDER BY item_count DESC)", all[0], wantTop)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ItemCount > all[i-1].ItemCount {
			t.Fatalf("unfiltered group %d count %d > group %d count %d, want non-increasing order; full result %+v",
				i, all[i].ItemCount, i-1, all[i-1].ItemCount, all)
		}
	}

	// Collection filter. COL1 counts are all distinct, so the order is exact.
	assertRows(t, `collection="COL1"`, run(t, "", "COL1", 0), []itemAuthorRow{
		{DisplayName: "Curie, Marie", CreatorType: "author", ItemCount: 5},
		{DisplayName: "Bohr, Niels", CreatorType: "author", ItemCount: 4},
		{DisplayName: "Hahn, Otto", CreatorType: "editor", ItemCount: 3},
		{DisplayName: "World Health Organization", CreatorType: "author", ItemCount: 2},
		{DisplayName: "Curie, Marie", CreatorType: "editor", ItemCount: 1},
	})
	assertRows(t, `collection="COL2"`, run(t, "", "COL2", 0), []itemAuthorRow{
		{DisplayName: "Planck, Max", CreatorType: "author", ItemCount: 2},
		{DisplayName: "Curie, Marie", CreatorType: "author", ItemCount: 1},
	})

	// Creator-type filter alone: authors must disappear entirely.
	assertRows(t, `type="editor"`, run(t, "editor", "", 0), []itemAuthorRow{
		{DisplayName: "Hahn, Otto", CreatorType: "editor", ItemCount: 3},
		{DisplayName: "Curie, Marie", CreatorType: "editor", ItemCount: 1},
	})

	// Both filters together.
	assertRows(t, `type="author" collection="COL2"`, run(t, "author", "COL2", 0), []itemAuthorRow{
		{DisplayName: "Planck, Max", CreatorType: "author", ItemCount: 2},
		{DisplayName: "Curie, Marie", CreatorType: "author", ItemCount: 1},
	})

	// --top truncates. Both cut points sit on a strict count boundary, so the
	// retained rows are deterministic.
	assertRows(t, "top=1", run(t, "", "", 1), []itemAuthorRow{
		{DisplayName: "Curie, Marie", CreatorType: "author", ItemCount: 6},
	})
	assertRows(t, `collection="COL1" top=2`, run(t, "", "COL1", 2), []itemAuthorRow{
		{DisplayName: "Curie, Marie", CreatorType: "author", ItemCount: 5},
		{DisplayName: "Bohr, Niels", CreatorType: "author", ItemCount: 4},
	})
}

// TestNormalizeItemAuthorRowsRejectsInvalidCount pins the error branch: a
// non-numeric item_count must surface as an error naming the creator, never as
// a silent zero.
func TestNormalizeItemAuthorRowsRejectsInvalidCount(t *testing.T) {
	rows := []map[string]any{
		{"last_name": "Curie", "first_name": "Marie", "name": "", "creator_type": "author", "item_count": int64(3)},
		{"last_name": "Bohr", "first_name": "Niels", "name": "", "creator_type": "author", "item_count": "not-a-number"},
	}
	got, err := normalizeItemAuthorRows(rows)
	if err == nil {
		t.Fatalf("normalizeItemAuthorRows = %+v, nil; want an error for a non-numeric item_count", got)
	}
	if got != nil {
		t.Fatalf("normalizeItemAuthorRows returned %+v alongside the error, want nil rows", got)
	}
	if !strings.Contains(err.Error(), "Bohr, Niels") {
		t.Fatalf("error = %q, want it to name the offending creator %q", err.Error(), "Bohr, Niels")
	}
}

// TestNormalizeItemAuthorRowsDisplayNames covers both Zotero creator name
// shapes that the normalizer must render: the two-field firstName/lastName
// person and the single-field organization name.
func TestNormalizeItemAuthorRowsDisplayNames(t *testing.T) {
	rows := []map[string]any{
		{"last_name": "Curie", "first_name": "Marie", "name": "", "creator_type": "author", "item_count": int64(4)},
		{"last_name": "", "first_name": "", "name": "World Health Organization", "creator_type": "author", "item_count": int64(2)},
		{"last_name": "Bohr", "first_name": "", "name": "", "creator_type": "editor", "item_count": int64(1)},
		// SQLite hands back NULL for a missing column value; sqlText maps it
		// to "" so the mononym still renders.
		{"last_name": nil, "first_name": "Ada", "name": nil, "creator_type": nil, "item_count": float64(7)},
	}
	got, err := normalizeItemAuthorRows(rows)
	if err != nil {
		t.Fatalf("normalizeItemAuthorRows: %v", err)
	}
	want := []itemAuthorRow{
		{DisplayName: "Curie, Marie", CreatorType: "author", ItemCount: 4},
		{DisplayName: "World Health Organization", CreatorType: "author", ItemCount: 2},
		{DisplayName: "Bohr", CreatorType: "editor", ItemCount: 1},
		{DisplayName: "Ada", CreatorType: "", ItemCount: 7},
	}
	if len(got) != len(want) {
		t.Fatalf("normalizeItemAuthorRows returned %d rows %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
