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

func nestedItemPayload(key string, version int, title string) json.RawMessage {
	obj := map[string]any{
		"key": key,
		"data": map[string]any{
			"key":      key,
			"version":  version,
			"itemType": "journalArticle",
			"title":    title,
		},
	}
	b, _ := json.Marshal(obj)
	return b
}

func getEffectiveVersionTitle(t *testing.T, s *Store, key string) (int, string) {
	t.Helper()
	raw, err := s.Get("items", key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	if raw == nil {
		t.Fatalf("get %s: no row", key)
	}
	var obj struct {
		Version *int `json:"version"`
		Data    struct {
			Version *int   `json:"version"`
			Title   string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	if obj.Version != nil {
		return *obj.Version, obj.Data.Title
	}
	if obj.Data.Version != nil {
		return *obj.Data.Version, obj.Data.Title
	}
	return 0, obj.Data.Title
}

func TestUpsertNestedVersionMonotonic(t *testing.T) {
	t.Run("nested-only", func(t *testing.T) {
		s := queryTestStore(t)
		const key = "NESTED"

		if err := s.Upsert("items", key, nestedItemPayload(key, 5, "Nestedfive")); err != nil {
			t.Fatalf("upsert nested v5: %v", err)
		}
		if err := s.Upsert("items", key, nestedItemPayload(key, 4, "Nestedfour")); err != nil {
			t.Fatalf("upsert nested v4: %v", err)
		}
		if version, title := getEffectiveVersionTitle(t, s, key); version != 5 || title != "Nestedfive" {
			t.Fatalf("lower nested write clobbered row: got version=%d title=%q, want version=5 title=Nestedfive", version, title)
		}

		if err := s.Upsert("items", key, nestedItemPayload(key, 5, "Nestedequal")); err != nil {
			t.Fatalf("upsert nested equal v5: %v", err)
		}
		if version, title := getEffectiveVersionTitle(t, s, key); version != 5 || title != "Nestedequal" {
			t.Fatalf("equal nested write not applied: got version=%d title=%q, want version=5 title=Nestedequal", version, title)
		}

		if err := s.Upsert("items", key, nestedItemPayload(key, 6, "Nestedhigher")); err != nil {
			t.Fatalf("upsert nested v6: %v", err)
		}
		if version, title := getEffectiveVersionTitle(t, s, key); version != 6 || title != "Nestedhigher" {
			t.Fatalf("higher nested write not applied: got version=%d title=%q, want version=6 title=Nestedhigher", version, title)
		}
	})

	t.Run("stored nested incoming top-level", func(t *testing.T) {
		s := queryTestStore(t)
		const key = "MIXED"

		if err := s.Upsert("items", key, nestedItemPayload(key, 5, "Nestedstored")); err != nil {
			t.Fatalf("upsert nested stored v5: %v", err)
		}
		if err := s.Upsert("items", key, itemPayload(key, 4, "Toplevelfour")); err != nil {
			t.Fatalf("upsert top-level v4: %v", err)
		}
		if version, title := getEffectiveVersionTitle(t, s, key); version != 5 || title != "Nestedstored" {
			t.Fatalf("lower top-level write clobbered nested row: got version=%d title=%q, want version=5 title=Nestedstored", version, title)
		}
		if err := s.Upsert("items", key, itemPayload(key, 6, "Toplevelsix")); err != nil {
			t.Fatalf("upsert top-level v6: %v", err)
		}
		if version, title := getEffectiveVersionTitle(t, s, key); version != 6 || title != "Toplevelsix" {
			t.Fatalf("higher top-level write not applied to nested row: got version=%d title=%q, want version=6 title=Toplevelsix", version, title)
		}
	})
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
	const local = "http://localhost:23119/api/users/0"

	if err := s.SaveLibraryVersion("items", local, 5); err != nil {
		t.Fatalf("save 5: %v", err)
	}
	// A slower run completing with an older checkpoint must not regress it.
	if err := s.SaveLibraryVersion("items", local, 3); err != nil {
		t.Fatalf("save 3: %v", err)
	}
	if v, err := s.GetLibraryVersion("items", local); err != nil || v != 5 {
		t.Fatalf("after regress attempt: got %d err=%v, want 5", v, err)
	}
	// A newer checkpoint still advances.
	if err := s.SaveLibraryVersion("items", local, 7); err != nil {
		t.Fatalf("save 7: %v", err)
	}
	if v, err := s.GetLibraryVersion("items", local); err != nil || v != 7 {
		t.Fatalf("after advance: got %d err=%v, want 7", v, err)
	}
}

// Version numbers are per-plane. A web-API checkpoint (12689) must not outrank a
// local one (71) via the monotonic guard: `?since=12689` against the local plane
// matches nothing, which froze incremental sync permanently.
func TestLibraryVersionIsPerPlane(t *testing.T) {
	s := queryTestStore(t)
	const local = "http://localhost:23119/api/users/0"
	const web = "https://api.zotero.org/users/5847066"

	if err := s.SaveLibraryVersion("items", web, 12689); err != nil {
		t.Fatalf("save web checkpoint: %v", err)
	}
	// The local plane must not inherit the web cursor.
	if v, err := s.GetLibraryVersion("items", local); err != nil || v != 0 {
		t.Fatalf("local cursor = %d err=%v, want 0: a foreign cursor must force a full pass", v, err)
	}
	// Recording a much smaller local version must win, not lose to MAX().
	if err := s.SaveLibraryVersion("items", local, 71); err != nil {
		t.Fatalf("save local checkpoint: %v", err)
	}
	if v, err := s.GetLibraryVersion("items", local); err != nil || v != 71 {
		t.Fatalf("local cursor = %d err=%v, want 71", v, err)
	}
	// Switching back is symmetric: the web cursor was replaced, not retained.
	if v, err := s.GetLibraryVersion("items", web); err != nil || v != 0 {
		t.Fatalf("web cursor = %d err=%v, want 0 after the plane changed", v, err)
	}
	// Status reporting still sees the stored value and names its plane.
	v, source, err := s.StoredLibraryVersion("items")
	if err != nil || v != 71 || source != local {
		t.Fatalf("StoredLibraryVersion = %d, %q, %v; want 71 from the local plane", v, source, err)
	}
}

// A checkpoint written by a build that recorded no plane must not be replayed
// against a plane it may not belong to.
func TestLibraryVersionWithoutSourceForcesFullPass(t *testing.T) {
	s := queryTestStore(t)
	if _, err := s.DB().Exec(
		`INSERT INTO sync_state (resource_type, library_version) VALUES ('items', 12689)`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if v, err := s.GetLibraryVersion("items", "http://localhost:23119/api/users/0"); err != nil || v != 0 {
		t.Fatalf("legacy cursor = %d err=%v, want 0", v, err)
	}
}
