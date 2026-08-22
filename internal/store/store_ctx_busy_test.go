// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// busyContendedStore creates a holder connection with busy_timeout(0),
// creates the table, seeds it, holds BEGIN EXCLUSIVE, and opens a second
// Store connection with busy_timeout(0). The caller owns the 30ms COMMIT
// goroutine and the query. Shared by the retry table and the
// cancellation tests where the query Store does not need mode=ro.
func busyContendedStore(t *testing.T, dbFile, createSQL, seedSQL string) (*Store, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), dbFile)
	holder, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.Exec(createSQL); err != nil {
		t.Fatalf("create: %v", err)
	}
	if seedSQL != "" {
		if _, err := holder.Exec(seedSQL); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if _, err := holder.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatalf("begin exclusive: %v", err)
	}
	t.Cleanup(func() { _, _ = holder.Exec(`ROLLBACK`) })

	queryDB, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open queryDB: %v", err)
	}
	queryDB.SetMaxOpenConns(1)
	s := &Store{db: queryDB}
	t.Cleanup(func() { _ = s.Close() })
	return s, holder
}

func TestQueryWithBusyRetryContext_CancelledAbortsPromptly(t *testing.T) {
	// This case needs mode=ro on the query connection, unlike the retry
	// table, so keep its holder setup inline rather than forcing the
	// shared helper to take a mode flag.
	dbPath := filepath.Join(t.TempDir(), "cancel_query.db")
	holder, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.Exec(`CREATE TABLE entries (value TEXT)`); err != nil {
		t.Fatalf("create entries: %v", err)
	}
	if _, err := holder.Exec(`INSERT INTO entries (value) VALUES ('x')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := holder.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatalf("begin exclusive: %v", err)
	}
	t.Cleanup(func() { _, _ = holder.Exec(`ROLLBACK`) })

	queryDB, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open queryDB: %v", err)
	}
	queryDB.SetMaxOpenConns(1)
	s := &Store{db: queryDB}
	t.Cleanup(func() { _ = s.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = s.queryWithBusyRetryContext(ctx, `SELECT value FROM entries`)
	elapsed := time.Since(start)
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("cancellation not honored promptly: elapsed %v, want <1.5s (would be %v with Background)", elapsed, migrationLockTimeout)
	}
	if err == nil {
		t.Fatalf("expected context.Canceled, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(strings.ToLower(err.Error()), "canceled") && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestQueryItems_CancellationViaContextAbortsPromptly(t *testing.T) {
	s, _ := busyContendedStore(t, "cancel_items.db",
		`CREATE TABLE resources (id TEXT, resource_type TEXT, data TEXT, parent_key TEXT, item_type TEXT, updated_at INTEGER)`,
		`INSERT INTO resources (id, resource_type, data) VALUES ('A','items','{"key":"A"}')`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := s.QueryItemsContext(ctx, ItemQuery{})
	elapsed := time.Since(start)
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("QueryItemsContext cancellation not honored: elapsed %v", elapsed)
	}
	if err == nil || (!errors.Is(err, context.Canceled) && !strings.Contains(strings.ToLower(err.Error()), "canceled")) {
		t.Fatalf("expected context.Canceled from QueryItemsContext, got %v", err)
	}
}

func TestQuery_RetriesBusyInsteadOfFailingHard(t *testing.T) {
	cases := []struct {
		name      string
		dbFile    string
		createSQL string
		seedSQL   string
		query     func(context.Context, *Store) (int, error)
	}{
		{
			name:      "items",
			dbFile:    "busy_items.db",
			createSQL: `CREATE TABLE resources (id TEXT, resource_type TEXT, data TEXT, parent_key TEXT, item_type TEXT, updated_at INTEGER)`,
			seedSQL:   `INSERT INTO resources (id, resource_type, data, item_type) VALUES ('A','items','{"key":"A","data":{"title":"Alpha"}}','journalArticle'), ('B','items','{"key":"B","data":{"title":"Beta"}}','book')`,
			query: func(ctx context.Context, s *Store) (int, error) {
				rows, err := s.QueryItemsContext(ctx, ItemQuery{Limit: 10})
				return len(rows), err
			},
		},
		{
			name:      "trash",
			dbFile:    "busy_trash.db",
			createSQL: `CREATE TABLE resources (id TEXT, resource_type TEXT, data TEXT, parent_key TEXT, item_type TEXT, updated_at INTEGER)`,
			seedSQL:   `INSERT INTO resources (id, resource_type, data) VALUES ('T1','items-trash','{"key":"T1","data":{"dateModified":"2020-01-02"}}'), ('T2','items-trash','{"key":"T2","data":{"dateModified":"2020-01-03"}}')`,
			query: func(ctx context.Context, s *Store) (int, error) {
				rows, err := s.QueryTrashContext(ctx, TrashQuery{Limit: 10})
				return len(rows), err
			},
		},
		{
			name:      "similarity_candidates",
			dbFile:    "busy_sim.db",
			createSQL: `CREATE TABLE resources (id TEXT, resource_type TEXT, data TEXT, parent_key TEXT, item_type TEXT)`,
			seedSQL:   `INSERT INTO resources (id, resource_type, data, parent_key, item_type) VALUES ('A','items','{"key":"A"}','','book'), ('B','items','{"key":"B"}','','journalArticle')`,
			query: func(ctx context.Context, s *Store) (int, error) {
				cands, err := s.QuerySimilarityCandidatesContext(ctx)
				return len(cands), err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, holder := busyContendedStore(t, tc.dbFile, tc.createSQL, tc.seedSQL)

			go func() {
				time.Sleep(30 * time.Millisecond)
				if _, err := holder.Exec(`COMMIT`); err != nil {
					t.Logf("commit release: %v", err)
				}
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			n, err := tc.query(ctx, s)
			if err != nil {
				t.Fatalf("%s did not survive busy contention (should have retried): %v", tc.name, err)
			}
			if n != 2 {
				t.Fatalf("%s rows = %d, want 2", tc.name, n)
			}
		})
	}
}
