// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// readOnlyReadinessTimeout is deliberately much shorter than the writer's
// migration-lock budget. Readers never migrate; they only bridge the small
// interval in which a concurrent writer has an uncommitted first schema or
// upgrade transaction.
const readOnlyReadinessTimeout = time.Second

var errReadOnlySchemaNotReady = errors.New("local store schema is not ready")

type schemaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type requiredSchemaTable struct {
	name    string
	columns []string
}

// requiredReadOnlySchema names the stable core shape that local and MCP reads
// rely on. StoreSchemaVersion is the migration's canonical version gate; the
// column probe catches incomplete/legacy schemas even if an earlier binary
// stamped an inaccurate version.
var requiredReadOnlySchema = []requiredSchemaTable{
	{
		name: "resources",
		columns: []string{
			"id", "resource_type", "data", "parent_key", "item_type",
			"annotation_color", "item_date", "synced_at", "updated_at",
		},
	},
	// sync_state is intentionally not an open-time requirement. Most local
	// reads need only resources/FTS, while archiveStatus reports sync_state
	// corruption per resource instead of disguising a damaged table as an
	// uninitialized store. PRAGMA user_version still catches old migrations.
	{name: "resources_fts", columns: []string{"id", "resource_type", "content"}},
}

// waitForReadOnlyReadiness waits only for a writer's transactional schema
// publication. It does not execute DDL, run migrations, or otherwise write
// through the read-only handle.
func (s *Store) waitForReadOnlyReadiness(ctx context.Context) error {
	deadline := time.Now().Add(readOnlyReadinessTimeout)
	backoff := migrationLockBackoffMin

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for local store readiness: %w", err)
		}

		err := s.readOnlySchemaReady(ctx)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("waiting for local store readiness: %w", ctxErr)
		}
		if !isReadOnlySchemaTransition(err) {
			return fmt.Errorf("checking read-only store readiness: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("local store schema did not become ready within %s; run zotio sync to initialize or migrate it: %w", readOnlyReadinessTimeout, err)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for local store readiness: %w", ctx.Err())
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, migrationLockBackoffMax)
	}
}

func (s *Store) readOnlySchemaReady(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	if version > StoreSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d; upgrade the CLI binary", version, StoreSchemaVersion)
	}
	if version < StoreSchemaVersion {
		return fmt.Errorf("%w: database schema version %d is older than required version %d", errReadOnlySchemaNotReady, version, StoreSchemaVersion)
	}

	for _, table := range requiredReadOnlySchema {
		for _, column := range table.columns {
			hasColumn, err := tableHasColumn(ctx, s.db, table.name, column)
			if err != nil {
				return fmt.Errorf("checking required column %s.%s: %w", table.name, column, err)
			}
			if !hasColumn {
				return fmt.Errorf("%w: required column %s.%s is missing", errReadOnlySchemaNotReady, table.name, column)
			}
		}
	}
	return nil
}

func isReadOnlySchemaTransition(err error) bool {
	if errors.Is(err, errReadOnlySchemaNotReady) || isSQLiteBusy(err) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such table") ||
		strings.Contains(message, "no such column") ||
		strings.Contains(message, "database schema is locked")
}
