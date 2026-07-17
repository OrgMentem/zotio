// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Exercises version-monotonic write-through so out-of-order or concurrent
// same-resource writes cannot regress the local mirror to an older Zotero
// version, and the checkpoint never moves backward.

package store

import (
	"encoding/json"
	"testing"
)

func itemPayload(key string, version int, title string) json.RawMessage {
	obj := map[string]any{
		"key":     key,
		"version": version,
		"data": map[string]any{
			"key":      key,
			"itemType": "journalArticle",
			"title":    title,
		},
	}
	b, _ := json.Marshal(obj)
	return b
}

func getVersionTitle(t *testing.T, s *Store, key string) (int, string) {
	t.Helper()
	raw, err := s.Get("items", key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	if raw == nil {
		t.Fatalf("get %s: no row", key)
	}
	var obj struct {
		Version int `json:"version"`
		Data    struct {
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	return obj.Version, obj.Data.Title
}

func TestUpsertRejectsOlderVersion(t *testing.T) {
	s := queryTestStore(t)

	if err := s.Upsert("items", "A", itemPayload("A", 3, "Newertoken")); err != nil {
		t.Fatalf("upsert v3: %v", err)
	}
	// An out-of-order older write must not clobber the newer row.
	if err := s.Upsert("items", "A", itemPayload("A", 2, "Oldertoken")); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}

	if v, title := getVersionTitle(t, s, "A"); v != 3 || title != "Newertoken" {
		t.Fatalf("row regressed: got version=%d title=%q, want version=3 title=Newertoken", v, title)
	}

	// FTS must stay consistent with the retained row: the newer token matches,
	// the rejected older token does not.
	if rows, err := s.Search("Newertoken", 10); err != nil || len(rows) != 1 {
		t.Fatalf("search Newertoken: rows=%d err=%v, want 1 row", len(rows), err)
	}
	if rows, err := s.Search("Oldertoken", 10); err != nil || len(rows) != 0 {
		t.Fatalf("search Oldertoken: rows=%d err=%v, want 0 rows (FTS must not reflect rejected write)", len(rows), err)
	}
}

func TestUpsertAcceptsEqualAndNewerVersion(t *testing.T) {
	s := queryTestStore(t)

	if err := s.Upsert("items", "A", itemPayload("A", 1, "Firsttoken")); err != nil {
		t.Fatalf("upsert v1: %v", err)
	}
	// Equal version still updates (preserves the prior always-overwrite contract
	// for idempotent re-syncs of unchanged items).
	if err := s.Upsert("items", "A", itemPayload("A", 1, "Equaltoken")); err != nil {
		t.Fatalf("upsert v1 again: %v", err)
	}
	if v, title := getVersionTitle(t, s, "A"); v != 1 || title != "Equaltoken" {
		t.Fatalf("equal-version write not applied: got version=%d title=%q", v, title)
	}
	// A newer version updates.
	if err := s.Upsert("items", "A", itemPayload("A", 5, "Newesttoken")); err != nil {
		t.Fatalf("upsert v5: %v", err)
	}
	if v, title := getVersionTitle(t, s, "A"); v != 5 || title != "Newesttoken" {
		t.Fatalf("newer-version write not applied: got version=%d title=%q", v, title)
	}
	if rows, err := s.Search("Equaltoken", 10); err != nil || len(rows) != 0 {
		t.Fatalf("search Equaltoken after v5: rows=%d err=%v, want 0 (FTS should reflect newest)", len(rows), err)
	}
	if rows, err := s.Search("Newesttoken", 10); err != nil || len(rows) != 1 {
		t.Fatalf("search Newesttoken: rows=%d err=%v, want 1", len(rows), err)
	}
}

func TestUpsertVersionlessAlwaysUpdates(t *testing.T) {
	s := queryTestStore(t)

	// A payload with no top-level version must retain the prior overwrite
	// behavior so resource types that omit versions are never frozen.
	versionless := func(title string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{
			"key":  "A",
			"data": map[string]any{"key": "A", "itemType": "journalArticle", "title": title},
		})
		return b
	}
	if err := s.Upsert("items", "A", versionless("Alphatoken")); err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	if err := s.Upsert("items", "A", versionless("Betatoken")); err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	if _, title := getVersionTitle(t, s, "A"); title != "Betatoken" {
		t.Fatalf("versionless write not applied: got title=%q, want Betatoken", title)
	}
}

func TestSaveLibraryVersionMonotonic(t *testing.T) {
	s := queryTestStore(t)

	if err := s.SaveLibraryVersion("items", 5); err != nil {
		t.Fatalf("save 5: %v", err)
	}
	// A slower run completing with an older checkpoint must not regress it.
	if err := s.SaveLibraryVersion("items", 3); err != nil {
		t.Fatalf("save 3: %v", err)
	}
	if v, err := s.GetLibraryVersion("items"); err != nil || v != 5 {
		t.Fatalf("after regress attempt: got %d err=%v, want 5", v, err)
	}
	// A newer checkpoint still advances.
	if err := s.SaveLibraryVersion("items", 7); err != nil {
		t.Fatalf("save 7: %v", err)
	}
	if v, err := s.GetLibraryVersion("items"); err != nil || v != 7 {
		t.Fatalf("after advance: got %d err=%v, want 7", v, err)
	}
}
