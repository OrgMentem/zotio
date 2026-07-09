// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Exercises the local item query planner and FTS document.

package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func queryTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestQueryContextPreCanceledContextReturnsError(t *testing.T) {
	s := queryTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rows, err := s.QueryContext(ctx, "SELECT 1")
	if rows != nil {
		_ = rows.Close()
	}
	if err == nil {
		t.Fatalf("QueryContext with canceled context returned nil error")
	}
}

func itemKeys(t *testing.T, rows []json.RawMessage) []string {
	t.Helper()
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		var o map[string]any
		if err := json.Unmarshal(r, &o); err != nil {
			t.Fatalf("decode row: %v", err)
		}
		keys = append(keys, o["key"].(string))
	}
	return keys
}

func seedItems(t *testing.T, s *Store) {
	t.Helper()
	items := []json.RawMessage{
		json.RawMessage(`{"key":"A","version":1,"data":{"key":"A","itemType":"journalArticle","title":"Beta","date":"2020","collections":["COL1"],"tags":[{"tag":"ml"}],"creators":[{"lastName":"Zhang"}]}}`),
		json.RawMessage(`{"key":"B","version":1,"data":{"key":"B","itemType":"journalArticle","title":"Alpha","date":"2022","collections":["COL1","COL2"],"tags":[{"tag":"nlp"}],"abstractNote":"about transformers"}}`),
		json.RawMessage(`{"key":"C","version":1,"data":{"key":"C","itemType":"book","title":"Gamma","date":"2019","collections":["COL2"],"tags":[{"tag":"ml"}]}}`),
		json.RawMessage(`{"key":"D","version":1,"data":{"key":"D","itemType":"attachment","title":"scan.pdf","parentItem":"A"}}`),
	}
	if _, _, err := s.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func seedParentScopedItems(t *testing.T, s *Store) {
	t.Helper()
	items := []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"book","title":"Parent One"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"book","title":"Parent Two"}}`),
		json.RawMessage(`{"key":"C1","version":1,"data":{"key":"C1","itemType":"attachment","title":"Alpha Attachment","parentItem":"P1"}}`),
		json.RawMessage(`{"key":"C2","version":1,"data":{"key":"C2","itemType":"note","title":"Beta Note","parentItem":"P1"}}`),
		json.RawMessage(`{"key":"C3","version":1,"data":{"key":"C3","itemType":"note","title":"Other Parent Note","parentItem":"P2"}}`),
		json.RawMessage(`{"key":"T1","version":1,"data":{"key":"T1","itemType":"journalArticle","title":"Top Level"}}`),
	}
	if _, _, err := s.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed parent-scoped items: %v", err)
	}
}

func TestQueryItems_ParentAndItemType(t *testing.T) {
	s := queryTestStore(t)
	seedParentScopedItems(t, s)

	got, err := s.QueryItems(ItemQuery{Parent: "P1", Sort: "title", Direction: "asc"})
	if err != nil {
		t.Fatalf("QueryItems parent: %v", err)
	}
	if keys := itemKeys(t, got); len(keys) != 2 || keys[0] != "C1" || keys[1] != "C2" {
		t.Fatalf("parent=P1 keys = %v, want [C1 C2]", keys)
	}

	got, err = s.QueryItems(ItemQuery{Parent: "P1", ItemType: "note", Sort: "title", Direction: "asc"})
	if err != nil {
		t.Fatalf("QueryItems parent+itemType: %v", err)
	}
	if keys := itemKeys(t, got); len(keys) != 1 || keys[0] != "C2" {
		t.Fatalf("parent=P1 itemType=note keys = %v, want [C2]", keys)
	}
}

func TestQueryItems_ItemTypeAndSort(t *testing.T) {
	s := queryTestStore(t)
	seedItems(t, s)

	// itemType filter + title ascending.
	got, err := s.QueryItems(ItemQuery{ItemType: "journalArticle", Sort: "title", Direction: "asc"})
	if err != nil {
		t.Fatalf("QueryItems: %v", err)
	}
	if keys := itemKeys(t, got); len(keys) != 2 || keys[0] != "B" || keys[1] != "A" {
		t.Fatalf("title asc keys = %v, want [B A]", keys)
	}

	// date descending across all items.
	got, err = s.QueryItems(ItemQuery{Sort: "date", Direction: "desc"})
	if err != nil {
		t.Fatalf("QueryItems: %v", err)
	}
	// D (attachment) has no date -> NULL sorts last under DESC; B(2022) C? wait books too
	keys := itemKeys(t, got)
	if len(keys) != 4 || keys[0] != "B" || keys[1] != "A" || keys[2] != "C" {
		t.Fatalf("date desc keys = %v, want B,A,C first", keys)
	}
}

func TestQueryItems_TagCollectionTopLimit(t *testing.T) {
	s := queryTestStore(t)
	seedItems(t, s)

	// Tag membership.
	got, _ := s.QueryItems(ItemQuery{Tag: "ml", Sort: "title", Direction: "asc"})
	if keys := itemKeys(t, got); len(keys) != 2 || keys[0] != "A" || keys[1] != "C" {
		t.Fatalf("tag=ml keys = %v, want [A C]", keys)
	}

	// Collection membership.
	got, _ = s.QueryItems(ItemQuery{Collection: "COL2", Sort: "title", Direction: "asc"})
	if keys := itemKeys(t, got); len(keys) != 2 || keys[0] != "B" || keys[1] != "C" {
		t.Fatalf("collection=COL2 keys = %v, want [B C]", keys)
	}

	// Top-only excludes the attachment child D.
	got, _ = s.QueryItems(ItemQuery{TopOnly: true})
	for _, k := range itemKeys(t, got) {
		if k == "D" {
			t.Fatalf("TopOnly returned child item D: %v", itemKeys(t, got))
		}
	}
	if len(got) != 3 {
		t.Fatalf("TopOnly count = %d, want 3", len(got))
	}

	// Limit + start pagination over title-asc order [B, A, C, D].
	got, _ = s.QueryItems(ItemQuery{Sort: "title", Direction: "asc", Limit: 2, Start: 1})
	if keys := itemKeys(t, got); len(keys) != 2 || keys[0] != "A" || keys[1] != "C" {
		t.Fatalf("limit/start keys = %v, want [A C]", keys)
	}
}

func TestQueryItems_QuickSearch(t *testing.T) {
	s := queryTestStore(t)
	seedItems(t, s)

	// Quick search hits the curated FTS doc (abstractNote of B).
	got, err := s.QueryItems(ItemQuery{Query: "transformers"})
	if err != nil {
		t.Fatalf("QueryItems q: %v", err)
	}
	if keys := itemKeys(t, got); len(keys) != 1 || keys[0] != "B" {
		t.Fatalf("q=transformers keys = %v, want [B]", keys)
	}

	// Creator last name is indexed in the document.
	got, _ = s.QueryItems(ItemQuery{Query: "Zhang"})
	if keys := itemKeys(t, got); len(keys) != 1 || keys[0] != "A" {
		t.Fatalf("q=Zhang keys = %v, want [A]", keys)
	}

	// No matches is an empty result, not an error.
	got, err = s.QueryItems(ItemQuery{Query: "nonexistentterm"})
	if err != nil {
		t.Fatalf("QueryItems empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("q=nonexistentterm count = %d, want 0", len(got))
	}
}

func TestBuildSearchDocument(t *testing.T) {
	doc := buildSearchDocument("items", json.RawMessage(`{"key":"K1","data":{"title":"Deep Learning","creators":[{"lastName":"LeCun"}],"tags":[{"tag":"ai"}],"abstractNote":"neural nets"}}`))
	for _, want := range []string{"K1", "Deep Learning", "LeCun", "ai", "neural nets"} {
		if !contains(doc, want) {
			t.Errorf("item doc %q missing %q", doc, want)
		}
	}
	// Non-item resources keep raw JSON indexing.
	raw := `{"id":"x","field":"value"}`
	if got := buildSearchDocument("papers", json.RawMessage(raw)); got != raw {
		t.Errorf("non-item doc = %q, want raw JSON", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
