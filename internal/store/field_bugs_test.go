// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean field-bugs): regression tests for user-reported MCP/search/archive edge cases.

package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestFTSMatchQueryPreservesBooleanSyntax(t *testing.T) {
	cases := map[string]string{
		`automation OR algorithm`:         `"automation" OR "algorithm"`,
		`trust AND (automation OR "AI")`:  `"trust" AND ( "automation" OR "AI" )`,
		`trust AND`:                       `"trust"`,
		`OR`:                              `"OR"`,
		`trust (automation)`:              `"trust" AND ( "automation" )`,
		`"trust in automation" OR robots`: `"trust in automation" OR "robots"`,
	}
	for input, want := range cases {
		if got := ftsMatchQuery(input); got != want {
			t.Fatalf("ftsMatchQuery(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSearchExecutesBooleanORAndParentheses(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	fixtures := map[string]json.RawMessage{
		"A": json.RawMessage(`{"key":"A","data":{"title":"trust automation"}}`),
		"B": json.RawMessage(`{"key":"B","data":{"title":"algorithm accountability"}}`),
		"C": json.RawMessage(`{"key":"C","data":{"title":"gardening"}}`),
	}
	for key, body := range fixtures {
		if err := s.Upsert("items", key, body); err != nil {
			t.Fatalf("upsert %s: %v", key, err)
		}
	}

	got, err := s.Search("automation OR algorithm", 10)
	if err != nil {
		t.Fatalf("OR search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("OR search len = %d, want 2", len(got))
	}

	got, err = s.Search(`trust AND (automation OR "AI")`, 10)
	if err != nil {
		t.Fatalf("parenthesized search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("parenthesized search len = %d, want 1", len(got))
	}
}

func TestSearchReturnsEmptySliceInsteadOfNil(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	got, err := s.Search("definitely-not-present", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got == nil {
		t.Fatalf("Search returned nil slice; MCP marshals nil as null")
	}
	if len(got) != 0 {
		t.Fatalf("Search len = %d, want 0", len(got))
	}
}

func TestMigratePurgesTopAliasResourceResidue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Upsert("items-top", "A", json.RawMessage(`{"key":"A","data":{"title":"stale"}}`)); err != nil {
		t.Fatalf("upsert items-top: %v", err)
	}
	if err := s.Upsert("collections-top", "C", json.RawMessage(`{"key":"C","data":{"name":"stale"}}`)); err != nil {
		t.Fatalf("upsert collections-top: %v", err)
	}
	if err := s.SaveSyncState("items-top", "100", 100); err != nil {
		t.Fatalf("save items-top sync state: %v", err)
	}
	if err := s.SaveSyncState("collections-top", "100", 3); err != nil {
		t.Fatalf("save collections-top sync state: %v", err)
	}
	if _, err := s.DB().Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatalf("downgrade user_version: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s, err = OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	for _, resourceType := range []string{"items-top", "collections-top"} {
		var count int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM resources WHERE resource_type = ?`, resourceType).Scan(&count); err != nil {
			t.Fatalf("count resources %s: %v", resourceType, err)
		}
		if count != 0 {
			t.Fatalf("resources %s count = %d, want 0", resourceType, count)
		}
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sync_state WHERE resource_type = ?`, resourceType).Scan(&count); err != nil {
			t.Fatalf("count sync_state %s: %v", resourceType, err)
		}
		if count != 0 {
			t.Fatalf("sync_state %s count = %d, want 0", resourceType, count)
		}
	}
}
