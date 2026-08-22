// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"zotio/internal/store"
)

func newSyncHintTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestReadSyncHintStateContext_BackdatedReportsCorrectAge(t *testing.T) {
	db := newSyncHintTestStore(t)
	seed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if _, err := db.DB().Exec(
		`INSERT INTO sync_state(resource_type, last_synced_at, total_count) VALUES (?, ?, ?)`,
		"issues", seed, 1,
	); err != nil {
		t.Fatalf("seed sync_state: %v", err)
	}

	state, err := readSyncHintStateContext(context.Background(), db, "")
	if err != nil {
		t.Fatalf("readSyncHintStateContext = %v, want nil", err)
	}
	if !state.hasState {
		t.Fatalf("hasState = false, want true")
	}
	// Allow small clock skew between seed and query.
	if d := state.lastSynced.Sub(seed); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("lastSynced = %v, want %v (delta %v)", state.lastSynced, seed, d)
	}
	age := time.Since(state.lastSynced)
	if age < 90*time.Minute || age > 150*time.Minute {
		t.Fatalf("age = %v, want ~2h", age)
	}

	// Also via typed resource.
	typed, err := readSyncHintStateContext(context.Background(), db, "issues")
	if err != nil {
		t.Fatalf("typed read = %v, want nil", err)
	}
	if !typed.hasState {
		t.Fatalf("typed hasState = false, want true")
	}
	if d := typed.lastSynced.Sub(seed); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("typed lastSynced = %v, want %v", typed.lastSynced, seed)
	}

	// readSyncHintState wrapper must match.
	wrapped, err := readSyncHintState(db, "issues")
	if err != nil {
		t.Fatalf("readSyncHintState = %v, want nil", err)
	}
	if !wrapped.hasState || !wrapped.lastSynced.Equal(typed.lastSynced) {
		t.Fatalf("readSyncHintState = %+v, want %+v", wrapped, typed)
	}
}

func TestReadSyncHintStateContext_NullTimestampIgnored(t *testing.T) {
	t.Run("all resources ignores null row", func(t *testing.T) {
		db := newSyncHintTestStore(t)
		now := time.Now()
		valid := now.Add(-2 * time.Hour)
		for _, row := range []struct {
			resource string
			syncedAt any
		}{
			{"users", nil},
			{"issues", valid},
		} {
			if _, err := db.DB().Exec(
				`INSERT INTO sync_state(resource_type, last_synced_at, total_count) VALUES (?, ?, ?)`,
				row.resource, row.syncedAt, 1,
			); err != nil {
				t.Fatalf("seed %s sync_state: %v", row.resource, err)
			}
		}
		state, err := readSyncHintStateContext(context.Background(), db, "")
		if err != nil {
			t.Fatalf("readSyncHintStateContext = %v, want nil", err)
		}
		if !state.hasState {
			t.Fatalf("hasState = false, want true (valid row should be returned)")
		}
		if d := state.lastSynced.Sub(valid.Truncate(time.Second)); d < -5*time.Second || d > 5*time.Second {
			t.Fatalf("lastSynced = %v, want %v", state.lastSynced, valid)
		}
	})

	t.Run("only null returns zero state", func(t *testing.T) {
		db := newSyncHintTestStore(t)
		if _, err := db.DB().Exec(
			`INSERT INTO sync_state(resource_type, last_synced_at, total_count) VALUES (?, ?, ?)`,
			"users", nil, 1,
		); err != nil {
			t.Fatalf("seed sync_state: %v", err)
		}
		state, err := readSyncHintStateContext(context.Background(), db, "")
		if err != nil {
			t.Fatalf("readSyncHintStateContext = %v, want nil", err)
		}
		if state.hasState {
			t.Fatalf("hasState = true, want false for only-null table (got %v)", state.lastSynced)
		}
		if !state.lastSynced.IsZero() {
			t.Fatalf("lastSynced = %v, want zero", state.lastSynced)
		}
	})

	t.Run("typed resource with null timestamp is zero state", func(t *testing.T) {
		db := newSyncHintTestStore(t)
		if _, err := db.DB().Exec(
			`INSERT INTO sync_state(resource_type, last_synced_at, total_count) VALUES (?, ?, ?)`,
			"users", nil, 1,
		); err != nil {
			t.Fatalf("seed sync_state: %v", err)
		}
		// Typed query does not filter NULL, but Scan sees NullTime.Valid false
		// and the function returns zero state with nil error.
		state, err := readSyncHintStateContext(context.Background(), db, "users")
		if err != nil {
			t.Fatalf("readSyncHintStateContext = %v, want nil", err)
		}
		if state.hasState {
			t.Fatalf("hasState = true, want false for NULL timestamp (got %v)", state.lastSynced)
		}
	})
}

func TestReadSyncHintStateContext_ResourceFilter(t *testing.T) {
	db := newSyncHintTestStore(t)
	now := time.Now()
	seedUsers := now.Add(-5 * time.Minute).Truncate(time.Second)
	seedIssues := now.Add(-2 * time.Hour).Truncate(time.Second)
	for _, row := range []struct {
		resource string
		syncedAt time.Time
	}{
		{"users", seedUsers},
		{"issues", seedIssues},
	} {
		if _, err := db.DB().Exec(
			`INSERT INTO sync_state(resource_type, last_synced_at, total_count) VALUES (?, ?, ?)`,
			row.resource, row.syncedAt, 1,
		); err != nil {
			t.Fatalf("seed %s sync_state: %v", row.resource, err)
		}
	}

	usersState, err := readSyncHintStateContext(context.Background(), db, "users")
	if err != nil {
		t.Fatalf("users read = %v, want nil", err)
	}
	if !usersState.hasState {
		t.Fatalf("users hasState = false, want true")
	}
	if d := usersState.lastSynced.Sub(seedUsers); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("users lastSynced = %v, want %v", usersState.lastSynced, seedUsers)
	}

	issuesState, err := readSyncHintStateContext(context.Background(), db, "issues")
	if err != nil {
		t.Fatalf("issues read = %v, want nil", err)
	}
	if !issuesState.hasState {
		t.Fatalf("issues hasState = false, want true")
	}
	if d := issuesState.lastSynced.Sub(seedIssues); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("issues lastSynced = %v, want %v", issuesState.lastSynced, seedIssues)
	}

	// users is fresh (-5m), issues is stale (-2h): they must differ.
	if !issuesState.lastSynced.Before(usersState.lastSynced) {
		t.Fatalf("issues %v not before users %v, resource filter returned wrong row", issuesState.lastSynced, usersState.lastSynced)
	}

	// TrimSpace branch: resourceType with surrounding whitespace still matches.
	trimmed, err := readSyncHintStateContext(context.Background(), db, "  users  ")
	if err != nil {
		t.Fatalf("trimmed read = %v, want nil", err)
	}
	if !trimmed.hasState || !trimmed.lastSynced.Equal(usersState.lastSynced) {
		t.Fatalf("trimmed resource read = %+v, want %+v", trimmed, usersState)
	}

	// Empty/whitespace resourceType uses the all-resource oldest query.
	allState, err := readSyncHintStateContext(context.Background(), db, "   ")
	if err != nil {
		t.Fatalf("all read = %v, want nil", err)
	}
	if !allState.hasState {
		t.Fatalf("all hasState = false, want true")
	}
	// Oldest across both is issues.
	if d := allState.lastSynced.Sub(seedIssues); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("all lastSynced = %v, want oldest %v", allState.lastSynced, seedIssues)
	}
}

func TestReadSyncHintState_NilDB(t *testing.T) {
	state, err := readSyncHintStateContext(context.Background(), nil, "")
	if err != nil {
		t.Fatalf("nil db err = %v, want nil", err)
	}
	if state.hasState || !state.lastSynced.IsZero() {
		t.Fatalf("nil db state = %+v, want zero", state)
	}
	state2, err := readSyncHintState(nil, "issues")
	if err != nil {
		t.Fatalf("nil db readSyncHintState err = %v, want nil", err)
	}
	if state2.hasState || !state2.lastSynced.IsZero() {
		t.Fatalf("nil db state2 = %+v, want zero", state2)
	}
}

func TestSyncHintMissingTable_Guard(t *testing.T) {
	t.Run("syncHintMissingTable detects no such table", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
			want bool
		}{
			{name: "direct", err: errors.New("no such table: sync_state"), want: true},
			{name: "wrapped", err: errors.New("querying local store: no such table: sync_state"), want: true},
			{name: "double wrapped", err: errors.Join(errors.New("outer"), errors.New("no such table: sync_state")), want: true},
			{name: "other error", err: errors.New("no such column: foo"), want: false},
			{name: "nil", err: nil, want: false},
			{name: "wrapped ErrNoRows", err: sql.ErrNoRows, want: false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := syncHintMissingTable(tc.err)
				if got != tc.want {
					t.Fatalf("syncHintMissingTable(%v) = %v, want %v", tc.err, got, tc.want)
				}
			})
		}
	})

	t.Run("readSyncHintStateContext missing table returns zero state nil error", func(t *testing.T) {
		// Build a raw SQLite file with NO sync_state table. Every existing
		// path used store.OpenWithContext which migrates and creates the
		// table, so the guard was never exercised.
		dir := t.TempDir()
		rawPath := filepath.Join(dir, "raw.db")
		rawDB, err := sql.Open("sqlite", rawPath)
		if err != nil {
			t.Fatalf("sql.Open raw: %v", err)
		}
		if _, err := rawDB.Exec(`CREATE TABLE dummy(id TEXT PRIMARY KEY)`); err != nil {
			t.Fatalf("create dummy: %v", err)
		}
		if err := rawDB.Close(); err != nil {
			t.Fatalf("close raw: %v", err)
		}
		s, err := store.OpenReadOnlyDiagnosticContext(context.Background(), rawPath)
		if err != nil {
			t.Fatalf("OpenReadOnlyDiagnosticContext: %v", err)
		}
		t.Cleanup(func() { s.Close() })

		for _, resource := range []string{"", "users", "issues"} {
			state, err := readSyncHintStateContext(context.Background(), s, resource)
			if err != nil {
				t.Fatalf("resource %q err = %v, want nil (missing table must be swallowed)", resource, err)
			}
			if state.hasState || !state.lastSynced.IsZero() {
				t.Fatalf("resource %q state = %+v, want zero (missing table)", resource, state)
			}
		}
		// Also via wrapper.
		state, err := readSyncHintState(s, "")
		if err != nil {
			t.Fatalf("readSyncHintState missing table err = %v, want nil", err)
		}
		if state.hasState {
			t.Fatalf("readSyncHintState hasState = true, want false")
		}
	})

	t.Run("readSyncHintStateContext missing table via dropped table", func(t *testing.T) {
		db := newSyncHintTestStore(t)
		if _, err := db.DB().Exec(`DROP TABLE sync_state`); err != nil {
			t.Fatalf("drop sync_state: %v", err)
		}
		state, err := readSyncHintStateContext(context.Background(), db, "")
		if err != nil {
			t.Fatalf("dropped table err = %v, want nil", err)
		}
		if state.hasState {
			t.Fatalf("dropped table hasState = true, want false")
		}
	})
}
