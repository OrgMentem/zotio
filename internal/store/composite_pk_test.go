// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Exercises the resources composite (resource_type, id) primary key:
// cross-type id coexistence and legacy id-only-PK rebuild.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
)

func resourcesPKColumnCount(t *testing.T, s *Store) int {
	t.Helper()
	rows, err := s.DB().Query(`PRAGMA table_info(resources)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	pk := 0
	for rows.Next() {
		var cid, notnull, pkpos int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pkpos); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if pkpos > 0 {
			pk++
		}
	}
	return pk
}

// TestUpsert_CompositePKAllowsSameIDAcrossTypes proves the composite
// (resource_type, id) PK: a tag named "annotation" and the "annotation" itemType
// share an id but must coexist as two rows. Under the old id-only PK the second
// upsert would overwrite the first.
func TestUpsert_CompositePKAllowsSameIDAcrossTypes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if err := s.Upsert("tags", "annotation", json.RawMessage(`{"tag":"annotation"}`)); err != nil {
		t.Fatalf("upsert tag: %v", err)
	}
	if err := s.Upsert("schema", "annotation", json.RawMessage(`{"itemType":"annotation"}`)); err != nil {
		t.Fatalf("upsert itemType: %v", err)
	}

	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM resources WHERE id = 'annotation'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("rows with id=annotation = %d, want 2 (composite PK must keep both)", count)
	}
}

// TestMigrate_RebuildsLegacyResourcesIDPK verifies a database whose resources
// table was created by an older binary with an id-only PK is upgraded in place
// to the composite PK, preserving existing rows, so ON CONFLICT(resource_type,
// id) upserts work afterward.
func TestMigrate_RebuildsLegacyResourcesIDPK(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE resources (
		id TEXT PRIMARY KEY,
		resource_type TEXT NOT NULL,
		data JSON NOT NULL,
		parent_key TEXT,
		item_type TEXT,
		annotation_color TEXT,
		item_date TEXT,
		synced_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		raw.Close()
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO resources (id, resource_type, data) VALUES ('K1','items','{"key":"K1"}')`); err != nil {
		raw.Close()
		t.Fatalf("seed: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 1`); err != nil {
		raw.Close()
		t.Fatalf("stamp: %v", err)
	}
	raw.Close()

	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open upgraded: %v", err)
	}
	defer s.Close()

	if got := resourcesPKColumnCount(t, s); got != 2 {
		t.Fatalf("resources PK columns = %d, want 2 (composite)", got)
	}

	var preserved int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM resources WHERE id = 'K1' AND resource_type = 'items'`).Scan(&preserved); err != nil {
		t.Fatalf("count preserved: %v", err)
	}
	if preserved != 1 {
		t.Fatalf("seeded row preserved = %d, want 1", preserved)
	}

	// Cross-type same id now works post-rebuild.
	if err := s.Upsert("tags", "K1", json.RawMessage(`{"tag":"K1"}`)); err != nil {
		t.Fatalf("post-rebuild cross-type upsert: %v", err)
	}
	var total int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM resources WHERE id = 'K1'`).Scan(&total); err != nil {
		t.Fatalf("count total: %v", err)
	}
	if total != 2 {
		t.Fatalf("rows id=K1 = %d, want 2", total)
	}
}
