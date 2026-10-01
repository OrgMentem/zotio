// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fsSeedRankedFulltext gives three parents PDF text that FTS ranks
// STRONG > MID > WEAK for "needle", plus a parent with blank text and one with
// none.
func fsSeedRankedFulltext(t *testing.T) *Store {
	t.Helper()
	s := queryTestStore(t)
	if _, _, err := s.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"STRONG","data":{"key":"STRONG","itemType":"journalArticle","title":"Strong"}}`),
		json.RawMessage(`{"key":"MID","data":{"key":"MID","itemType":"journalArticle","title":"Mid"}}`),
		json.RawMessage(`{"key":"WEAK","data":{"key":"WEAK","itemType":"journalArticle","title":"Weak"}}`),
		json.RawMessage(`{"key":"BLANK","data":{"key":"BLANK","itemType":"journalArticle","title":"Blank"}}`),
		json.RawMessage(`{"key":"NONE","data":{"key":"NONE","itemType":"journalArticle","title":"None"}}`),
		json.RawMessage(`{"key":"ASTRONG","data":{"key":"ASTRONG","itemType":"attachment","parentItem":"STRONG"}}`),
		json.RawMessage(`{"key":"AMID","data":{"key":"AMID","itemType":"attachment","parentItem":"MID"}}`),
		json.RawMessage(`{"key":"AWEAK","data":{"key":"AWEAK","itemType":"attachment","parentItem":"WEAK"}}`),
		json.RawMessage(`{"key":"ABLANK","data":{"key":"ABLANK","itemType":"attachment","parentItem":"BLANK"}}`),
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if _, err := s.UpsertKeyed("fulltext", []string{"ASTRONG", "AMID", "AWEAK", "ABLANK"}, []json.RawMessage{
		json.RawMessage(`{"content":"needle needle needle"}`),
		json.RawMessage(`{"content":"needle needle in a slightly longer body of text"}`),
		json.RawMessage(`{"content":"one needle hidden among a much longer passage of unrelated words about methods, sampling, results and discussion"}`),
		json.RawMessage(`{"content":"   "}`),
	}); err != nil {
		t.Fatalf("seed full text: %v", err)
	}
	return s
}

func fsFulltextKeys(t *testing.T, s *Store, q FulltextSearch) string {
	t.Helper()
	got, err := s.SearchFulltextContext(context.Background(), q)
	if err != nil {
		t.Fatalf("SearchFulltextContext(%+v): %v", q, err)
	}
	keys := make([]string, 0, len(got))
	for _, r := range got {
		keys = append(keys, r.ItemKey)
	}
	return strings.Join(keys, ",")
}

func TestFsSearchFulltextParentKeysFilterBeforeLimit(t *testing.T) {
	s := fsSeedRankedFulltext(t)

	if got := fsFulltextKeys(t, s, FulltextSearch{Query: "needle"}); got != "STRONG,MID,WEAK" {
		t.Fatalf("unfiltered = %s, want STRONG,MID,WEAK (fixture rank order)", got)
	}
	if got := fsFulltextKeys(t, s, FulltextSearch{Query: "needle", Limit: 1}); got != "STRONG" {
		t.Fatalf("unfiltered limit 1 = %s, want STRONG", got)
	}
	// STRONG ranks first overall; outside the cohort it must not take the
	// only slot.
	if got := fsFulltextKeys(t, s, FulltextSearch{Query: "needle", Limit: 1, ParentKeys: []string{"MID", "WEAK"}}); got != "MID" {
		t.Fatalf("cohort limit 1 = %s, want MID", got)
	}
	if got := fsFulltextKeys(t, s, FulltextSearch{Query: "needle", Limit: -1, ParentKeys: []string{"WEAK", "MID"}}); got != "MID,WEAK" {
		t.Fatalf("cohort unlimited = %s, want MID,WEAK in rank order", got)
	}
	if got := fsFulltextKeys(t, s, FulltextSearch{Query: "needle", ParentKeys: []string{}}); got != "" {
		t.Fatalf("empty cohort = %s, want no hits", got)
	}
}

func TestFsFulltextIndexedParents(t *testing.T) {
	s := fsSeedRankedFulltext(t)

	got, err := s.FulltextIndexedParentsContext(context.Background(), []string{"STRONG", "WEAK", "BLANK", "NONE", "MISSING"})
	if err != nil {
		t.Fatalf("FulltextIndexedParentsContext: %v", err)
	}
	if len(got) != 2 || !got["STRONG"] || !got["WEAK"] {
		t.Fatalf("indexed = %v, want STRONG and WEAK only (blank text and no text are not searchable)", got)
	}
	empty, err := s.FulltextIndexedParentsContext(context.Background(), nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("no keys = %v, %v; want an empty set", empty, err)
	}
}
