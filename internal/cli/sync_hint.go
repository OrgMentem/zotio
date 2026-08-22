// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"zotio/internal/store"
)

type syncHintState struct {
	hasState   bool
	lastSynced time.Time
}

func readSyncHintStateContext(ctx context.Context, db *store.Store, resourceType string) (syncHintState, error) {
	if db == nil {
		return syncHintState{}, nil
	}
	query := `SELECT last_synced_at FROM sync_state`
	args := []any{}
	if trimmed := strings.TrimSpace(resourceType); trimmed != "" {
		query += ` WHERE resource_type = ?`
		args = append(args, trimmed)
	} else {
		query += ` WHERE last_synced_at IS NOT NULL`
	}
	query += ` ORDER BY last_synced_at ASC LIMIT 1`

	var lastSynced sql.NullTime
	err := db.QueryRowContext(ctx, query, args...).Scan(&lastSynced)
	if err == nil {
		if !lastSynced.Valid {
			return syncHintState{}, nil
		}
		return syncHintState{
			hasState:   true,
			lastSynced: lastSynced.Time,
		}, nil
	}
	if errors.Is(err, sql.ErrNoRows) || syncHintMissingTable(err) {
		return syncHintState{}, nil
	}
	return syncHintState{}, err
}

func readSyncHintState(db *store.Store, resourceType string) (syncHintState, error) {
	return readSyncHintStateContext(context.Background(), db, resourceType)
}

func syncHintMissingTable(err error) bool {
	for err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}
