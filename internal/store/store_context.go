// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"time"
)

// QueryContext executes a raw SQL query with cancellation and returns the rows.
// Used by agent-facing tools that must not outlive their request context.
func (s *Store) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.queryContextWithBusyRetry(ctx, query, args...)
}

func (s *Store) queryContextWithBusyRetry(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	var rows *sql.Rows
	deadline := time.Now().Add(migrationLockTimeout)
	err := retryOnBusy(ctx, deadline, "querying local store", func() error {
		var err error
		rows, err = s.db.QueryContext(ctx, query, args...)
		return err
	})
	return rows, err
}
