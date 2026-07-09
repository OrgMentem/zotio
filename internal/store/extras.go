// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrateExtras runs after the generated store migrations and before the
// schema-version stamp. It is the canonical place for novel-feature auxiliary
// tables that need to live in the local store.
//
// Edit this file when adding tables for novel commands. Keep migrations
// idempotent with CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT EXISTS so
// every store open can safely re-run them.
func (s *Store) migrateExtras(ctx context.Context, conn *sql.Conn) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS creator_orcids (
			item_key TEXT NOT NULL,
			creator_index INTEGER NOT NULL,
			name_hash TEXT NOT NULL,
			orcid TEXT NOT NULL,
			source TEXT NOT NULL,
			captured_at DATETIME NOT NULL,
			PRIMARY KEY(item_key, creator_index, source)
		)`,
		`CREATE INDEX IF NOT EXISTS creator_orcids_name_hash_idx ON creator_orcids(name_hash)`,
		`CREATE INDEX IF NOT EXISTS creator_orcids_orcid_idx ON creator_orcids(orcid)`,
	}
	for _, m := range migrations {
		if _, err := conn.ExecContext(ctx, m); err != nil {
			return fmt.Errorf("extra migration failed: %w", err)
		}
	}
	return nil
}
