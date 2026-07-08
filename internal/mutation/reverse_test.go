// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// reversibility tests.

package mutation

import "testing"

func TestInvertChange(t *testing.T) {
	if got, ok := InvertChange(Change{Field: "tags", Add: "ml"}); !ok || got.Remove != "ml" || got.Add != nil {
		t.Errorf("invert tag add = (%+v,%v), want remove ml", got, ok)
	}
	if got, ok := InvertChange(Change{Field: "collections", Remove: "C1"}); !ok || got.Add != "C1" {
		t.Errorf("invert collection remove = (%+v,%v), want add C1", got, ok)
	}
	if _, ok := InvertChange(Change{Field: "collections", Add: []string{"X"}}); ok {
		t.Error("bulk []string membership add (a duplicate-merge change) should be irreversible")
	}
	for _, f := range []string{"DOI", "abstractNote", "tag", "deleted", "url", "attachment"} {
		if _, ok := InvertChange(Change{Field: f, Add: "x"}); ok {
			t.Errorf("field %q should be irreversible", f)
		}
	}
}

func TestInverseOps(t *testing.T) {
	entry := JournalEntry{Ops: []JournalOp{
		{ID: "t1", Key: "K1", Kind: "tag_add", Status: "applied", Changes: []Change{{Field: "tags", Add: "ml"}}},
		{ID: "c1", Key: "K2", Kind: "collection_add", Status: "applied", Changes: []Change{{Field: "collections", Add: "COLL"}}},
		{ID: "n1", Key: "K3", Kind: "tag_add", Status: "no_op", Changes: []Change{{Field: "tags", Add: "skip"}}},
		{ID: "m1", Key: "K4", Kind: "duplicate_merge", Status: "applied", Changes: []Change{{Field: "collections", Add: "X"}, {Field: "deleted", Add: 1}}},
		{ID: "e1", Key: "K5", Kind: "missing_doi", Status: "applied", Changes: []Change{{Field: "DOI", Add: "10/x"}}},
		{ID: "m2", Key: "K6", Kind: "duplicate_merge", Status: "applied", Changes: []Change{{Field: "collections", Add: []string{"Z"}}}},
	}}

	inverse, refused := InverseOps(entry)

	if len(inverse) != 2 {
		t.Fatalf("inverse ops = %d, want 2 (tag_add, collection_add)", len(inverse))
	}
	if inverse[0].Key != "K1" || inverse[0].Kind != "undo.tag_add" || inverse[0].Changes[0].Remove != "ml" {
		t.Errorf("inverse[0] = %+v, want remove ml on K1", inverse[0])
	}
	if inverse[1].Changes[0].Remove != "COLL" {
		t.Errorf("inverse[1] = %+v, want remove COLL", inverse[1])
	}

	// duplicate_merge (has a 'deleted' change) and missing_doi (DOI overwrite) are refused;
	// the no_op is silently skipped (it changed nothing).
	if len(refused) != 3 {
		t.Fatalf("refused = %+v, want 3 (merge w/ deleted, missing_doi, already-trashed merge)", refused)
	}
	refusedKeys := map[string]bool{}
	for _, r := range refused {
		refusedKeys[r.Key] = true
	}
	if !refusedKeys["K4"] || !refusedKeys["K5"] || !refusedKeys["K6"] {
		t.Errorf("refused keys = %v, want K4, K5, K6", refusedKeys)
	}
	for _, op := range inverse {
		if op.Key == "K6" {
			t.Error("already-trashed merge (K6, []string membership add) must not be inverted")
		}
	}
}
