// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// reversibility tests.

package mutation

import (
	"strings"
	"testing"
)

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

func TestInverseOpsRefusesCommittedConflict(t *testing.T) {
	entry := JournalEntry{Ops: []JournalOp{{
		ID: "create", Kind: "import_create", Status: "conflict",
		Changes: []Change{{Field: "item", Add: "Ambiguous Paper"}},
		Reason: map[string]any{
			"committed": true, "title": "Ambiguous Paper",
			"session": "session-1", "connector_key": "connector-1",
			"attachment_marker": "zotio-write-connector-1", "message": "two stored files matched",
		},
	}}}
	inverse, refused := InverseOps(entry)
	if len(inverse) != 0 || len(refused) != 1 {
		t.Fatalf("inverse=%+v refused=%+v, want one explicit refusal", inverse, refused)
	}
	reason := refused[0].Reason
	for _, want := range []string{"Ambiguous Paper", "session-1", "connector-1", "zotio-write-connector-1", "two stored files matched"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("refusal = %q, want %q", reason, want)
		}
	}
}
func TestInverseOpsTrashesCreatedItem(t *testing.T) {
	entry := JournalEntry{Ops: []JournalOp{{
		ID: "create", Key: "REALKEY1", Kind: "item_create", Status: "applied",
		Changes: []Change{{Field: "source", Add: "manual"}, {Field: "item", Add: map[string]any{"itemType": "book"}}},
	}}}
	inverse, refused := InverseOps(entry)
	if len(refused) != 0 {
		t.Fatalf("create refusals = %+v, want none", refused)
	}
	if len(inverse) != 1 || inverse[0].Key != "REALKEY1" || inverse[0].Kind != "undo.item_create" {
		t.Fatalf("create inverse = %+v, want undo.item_create on REALKEY1", inverse)
	}
	if len(inverse[0].Changes) != 1 || inverse[0].Changes[0].Field != "deleted" {
		t.Fatalf("create inverse changes = %+v, want deleted trash change", inverse[0].Changes)
	}
}

// An import run's creates trash like any create, and the resolver PDF attached
// to a trashed parent is covered by that trash rather than refused. A create
// whose key was never confirmed stays refused, and so does its attachment.
func TestInverseOpsTrashesImportedItems(t *testing.T) {
	entry := JournalEntry{Ops: []JournalOp{
		{ID: "import.apply:001:create", Key: "IMPORT01", Kind: "import_create", Status: "applied",
			Changes: []Change{{Field: "item", Add: "Paper One"}, {Field: "collection", Add: "COLL0001"}}},
		{ID: "import.apply:001:resolver-pdf", Key: "IMPORT01", Kind: "attachment_create", Status: "failed",
			Changes: []Change{{Field: "attachment", Add: map[string]any{"source": "resolver"}}},
			Reason:  map[string]any{"committed": true, "key": "IMPORT01", "message": "resolver PDF failed"}},
		{ID: "import.apply:002:create", Key: "IMPORT02", Kind: "import_create", Status: "applied",
			Changes: []Change{{Field: "item", Add: "Paper Two"}}},
		{ID: "import.apply:003:create", Kind: "import_create", Status: "applied",
			Changes: []Change{{Field: "item", Add: "Paper Three"}}},
		{ID: "import.apply:003:resolver-pdf", Key: "Paper Three", Kind: "attachment_create", Status: "applied",
			Changes: []Change{{Field: "attachment", Add: map[string]any{"source": "resolver"}}}},
	}}
	inverse, refused := InverseOps(entry)
	if len(inverse) != 2 || inverse[0].Key != "IMPORT01" || inverse[1].Key != "IMPORT02" {
		t.Fatalf("inverse = %+v, want trash of IMPORT01 and IMPORT02", inverse)
	}
	for _, inv := range inverse {
		if inv.Kind != "undo.item_create" || len(inv.Changes) != 1 || inv.Changes[0].Field != "deleted" {
			t.Fatalf("inverse %+v, want undo.item_create trash", inv)
		}
	}
	if len(refused) != 2 || refused[0].OpID != "import.apply:003:create" || refused[1].OpID != "import.apply:003:resolver-pdf" {
		t.Fatalf("refused = %+v, want the unkeyed create and its attachment", refused)
	}
}

// An op that recorded its own inverse is reversed from that record, even though
// its planned field change (a DOI overwrite) is not invertible on its own.
func TestInverseOpsReplaysRecordedRevert(t *testing.T) {
	revert := []Change{
		{Field: "DOI", Add: "", Remove: "10.1/x"},
		{Field: "extra_line", Remove: "zotio: DOI added via CrossRef on 2026-10-12"},
	}
	entry := JournalEntry{Ops: []JournalOp{{
		ID: "doi", Key: "K1", Kind: "missing_doi", Status: "applied",
		Changes: []Change{{Field: "DOI", Add: "10.1/x"}},
		Revert:  revert,
	}}}
	inverse, refused := InverseOps(entry)
	if len(refused) != 0 {
		t.Fatalf("refused = %+v, want none: the op recorded how to undo itself", refused)
	}
	if len(inverse) != 1 || inverse[0].Key != "K1" || inverse[0].Kind != "undo.missing_doi" || inverse[0].ID != "undo:doi" {
		t.Fatalf("inverse = %+v, want undo.missing_doi on K1", inverse)
	}
	if len(inverse[0].Changes) != 2 || inverse[0].Changes[0] != revert[0] || inverse[0].Changes[1] != revert[1] {
		t.Fatalf("inverse changes = %+v, want the recorded revert verbatim", inverse[0].Changes)
	}
}
