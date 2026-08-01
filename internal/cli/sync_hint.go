// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

type syncHintState struct {
	hasState   bool
	lastSynced time.Time
}

func hintIfStale(cmd *cobra.Command, db *store.Store, resourceType string, maxAge time.Duration) bool {
	if cmd == nil || db == nil || maxAge <= 0 {
		return false
	}
	state, err := readSyncHintStateContext(cmd.Context(), db, resourceType)
	if err != nil || !state.hasState {
		return false
	}
	age := time.Since(state.lastSynced)
	if age <= maxAge {
		return false
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "hint: local store data is %s old, older than --max-age=%s. Run 'zotio sync' to refresh.\n", syncHintRoundAge(age), maxAge)
	return true
}

func readSyncHintStateContext(ctx context.Context, db *store.Store, resourceType string) (syncHintState, error) {
	if db == nil {
		return syncHintState{}, nil
	}
	query := `SELECT last_synced_at FROM sync_state`
	args := []any{}
	if strings.TrimSpace(resourceType) != "" {
		query += ` WHERE resource_type = ?`
		args = append(args, resourceType)
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

func syncHintRoundAge(age time.Duration) time.Duration {
	if age < time.Minute {
		return age.Round(time.Second)
	}
	return age.Round(time.Minute)
}
