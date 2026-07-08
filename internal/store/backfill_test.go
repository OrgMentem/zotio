// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Exercises indexed-column value backfill for rows that predate
// the parent_key/item_type/etc. columns.

package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestBackfillIndexedColumnValues(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")

	s, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Simulate rows inserted before the indexed columns existed (NULL columns),
	// bypassing the upsert path that would populate them.
	if _, err := s.DB().Exec(
		`INSERT INTO resources (id, resource_type, data, item_type, parent_key, annotation_color, item_date) VALUES (?, 'items', ?, NULL, NULL, NULL, NULL)`,
		"OLD1", `{"key":"OLD1","data":{"itemType":"book","parentItem":"PAR","dateModified":"2020-05-05"}}`,
	); err != nil {
		t.Fatalf("seed OLD1: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO resources (id, resource_type, data, item_type, parent_key, annotation_color, item_date) VALUES (?, 'collections', ?, NULL, NULL, NULL, NULL)`,
		"COL1", `{"key":"COL1","data":{"name":"Mine"}}`,
	); err != nil {
		t.Fatalf("seed COL1: %v", err)
	}
	_ = s.Close()

	// Reopen: migrate() runs the value backfill.
	s2, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	var it, pk, dt string
	if err := s2.DB().QueryRow(`SELECT item_type, parent_key, item_date FROM resources WHERE id='OLD1'`).Scan(&it, &pk, &dt); err != nil {
		t.Fatalf("read OLD1: %v", err)
	}
	if it != "book" || pk != "PAR" || dt != "2020-05-05" {
		t.Errorf("OLD1 backfill = (%q,%q,%q), want (book,PAR,2020-05-05)", it, pk, dt)
	}

	// A type-less row is set to '' (not NULL) so the IS NULL guard never
	// reprocesses it.
	var colType *string
	if err := s2.DB().QueryRow(`SELECT item_type FROM resources WHERE id='COL1'`).Scan(&colType); err != nil {
		t.Fatalf("read COL1: %v", err)
	}
	if colType == nil || *colType != "" {
		t.Errorf("COL1 item_type = %v, want empty string (not NULL)", colType)
	}

	var nulls int
	if err := s2.DB().QueryRow(`SELECT COUNT(*) FROM resources WHERE item_type IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nulls != 0 {
		t.Errorf("rows with NULL item_type after backfill = %d, want 0", nulls)
	}
}
