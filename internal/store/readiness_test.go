// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func createCurrentSchemaInTransaction(t *testing.T, tx *sql.Tx) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE resources (
			id TEXT NOT NULL,
			resource_type TEXT NOT NULL,
			data JSON NOT NULL,
			parent_key TEXT,
			item_type TEXT,
			annotation_color TEXT,
			item_date TEXT,
			synced_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (resource_type, id)
		)`,
		`CREATE TABLE sync_state (
			resource_type TEXT PRIMARY KEY,
			last_cursor TEXT,
			last_synced_at DATETIME,
			total_count INTEGER DEFAULT 0,
			library_version INTEGER DEFAULT 0
		)`,
		`CREATE VIRTUAL TABLE resources_fts USING fts5(id, resource_type, content, tokenize='porter unicode61')`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			t.Fatalf("prepare uncommitted current schema: %v", err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, StoreSchemaVersion)); err != nil {
		t.Fatalf("stamp uncommitted current schema: %v", err)
	}
}

func TestOpenReadOnlyContext_WaitsForCommittedSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	writer, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer writer.Close()

	tx, err := writer.Begin()
	if err != nil {
		t.Fatalf("begin migration transaction: %v", err)
	}
	createCurrentSchemaInTransaction(t, tx)

	type openResult struct {
		store *Store
		err   error
	}
	result := make(chan openResult, 1)
	go func() {
		s, err := OpenReadOnlyContext(context.Background(), dbPath)
		result <- openResult{store: s, err: err}
	}()

	select {
	case opened := <-result:
		if opened.store != nil {
			_ = opened.store.Close()
		}
		t.Fatalf("read-only open returned before migration committed: %v", opened.err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit migration transaction: %v", err)
	}

	select {
	case opened := <-result:
		if opened.err != nil {
			t.Fatalf("read-only open after migration commit: %v", opened.err)
		}
		defer opened.store.Close()
		if _, err := opened.store.SchemaVersion(); err != nil {
			t.Fatalf("read schema version: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read-only open did not finish after migration committed")
	}
}

func TestOpenReadOnlyContext_CancellationStopsReadinessWait(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`PRAGMA user_version = 0`); err != nil {
		legacy.Close()
		t.Fatalf("set legacy version: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(25*time.Millisecond, cancel)
	start := time.Now()
	_, err = OpenReadOnlyContext(ctx, dbPath)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenReadOnlyContext error = %v, want context.Canceled", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("readiness wait ignored cancellation for %s", elapsed)
	}
}

func TestOpenReadOnlyContext_OldSchemaHasMigrationRemediation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`PRAGMA user_version = 0`); err != nil {
		legacy.Close()
		t.Fatalf("set legacy version: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	start := time.Now()
	_, err = OpenReadOnlyContext(context.Background(), dbPath)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("OpenReadOnlyContext succeeded for an old schema")
	}
	if !strings.Contains(err.Error(), "run zotio sync") {
		t.Fatalf("old-schema error lacks migration remediation: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("old-schema readiness wait was not bounded: %s", elapsed)
	}
}

func TestOpenReadOnlyContext_ReadySchemaReturnsImmediately(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ready.db")
	writer, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create current schema: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	start := time.Now()
	reader, err := OpenReadOnlyContext(context.Background(), dbPath)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("open ready schema: %v", err)
	}
	defer reader.Close()
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ready schema open took %s", elapsed)
	}
}

func TestOpenReadOnlyContext_ReadinessDeadlineInterruptsExclusiveLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "locked.db")
	ready, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create current schema: %v", err)
	}
	if err := ready.Close(); err != nil {
		t.Fatalf("close current schema: %v", err)
	}

	writer, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatalf("set rollback journal: %v", err)
	}
	if _, err := writer.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatalf("take exclusive lock: %v", err)
	}
	defer writer.Exec(`ROLLBACK`)

	start := time.Now()
	_, err = OpenReadOnlyContext(context.Background(), dbPath)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("OpenReadOnlyContext succeeded while schema was exclusively locked")
	}
	if !strings.Contains(err.Error(), "run zotio sync") {
		t.Fatalf("exclusive-lock error lacks readiness remediation: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("readiness deadline did not interrupt SQLite lock wait: %s", elapsed)
	}
}
