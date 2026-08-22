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

func TestQueryWithBusyRetryContext_CancelledAbortsPromptly(t *testing.T) {
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
	dbPath := filepath.Join(t.TempDir(), "cancel_items.db")
	holder, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.Exec(`CREATE TABLE resources (id TEXT, resource_type TEXT, data TEXT, parent_key TEXT, item_type TEXT, updated_at INTEGER)`); err != nil {
		t.Fatalf("create resources: %v", err)
	}
	if _, err := holder.Exec(`INSERT INTO resources (id, resource_type, data) VALUES ('A','items','{"key":"A"}')`); err != nil {
		t.Fatalf("seed: %v", err)
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

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = s.QueryItemsContext(ctx, ItemQuery{})
	elapsed := time.Since(start)
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("QueryItemsContext cancellation not honored: elapsed %v", elapsed)
	}
	if err == nil || (!errors.Is(err, context.Canceled) && !strings.Contains(strings.ToLower(err.Error()), "canceled")) {
		t.Fatalf("expected context.Canceled from QueryItemsContext, got %v", err)
	}
}

func TestQueryItems_RetriesBusyInsteadOfFailingHard(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "busy_items.db")
	holder, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.Exec(`CREATE TABLE resources (id TEXT, resource_type TEXT, data TEXT, parent_key TEXT, item_type TEXT, updated_at INTEGER)`); err != nil {
		t.Fatalf("create resources: %v", err)
	}
	// Seed two items so result order is deterministic.
	if _, err := holder.Exec(`INSERT INTO resources (id, resource_type, data, item_type) VALUES ('A','items','{"key":"A","data":{"title":"Alpha"}}','journalArticle'), ('B','items','{"key":"B","data":{"title":"Beta"}}','book')`); err != nil {
		t.Fatalf("seed: %v", err)
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

	go func() {
		time.Sleep(30 * time.Millisecond)
		if _, err := holder.Exec(`COMMIT`); err != nil {
			t.Logf("commit release: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := s.QueryItemsContext(ctx, ItemQuery{Limit: 10})
	if err != nil {
		t.Fatalf("QueryItems did not survive WAL contention (should have retried): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("QueryItems rows = %d, want 2", len(rows))
	}
	// Also verify context-aware variant retries.
	// Need to re-acquire lock for second call: holder after COMMIT is not in transaction.
	if _, err := holder.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatalf("re-acquire exclusive: %v", err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		if _, err := holder.Exec(`COMMIT`); err != nil {
			t.Logf("commit2: %v", err)
		}
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	rows2, err := s.QueryItemsContext(ctx2, ItemQuery{Limit: 10})
	if err != nil {
		t.Fatalf("QueryItemsContext did not retry: %v", err)
	}
	if len(rows2) != 2 {
		t.Fatalf("QueryItemsContext rows = %d, want 2", len(rows2))
	}
}

func TestQueryTrash_RetriesBusyInsteadOfFailingHard(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "busy_trash.db")
	holder, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.Exec(`CREATE TABLE resources (id TEXT, resource_type TEXT, data TEXT, parent_key TEXT, item_type TEXT, updated_at INTEGER)`); err != nil {
		t.Fatalf("create resources: %v", err)
	}
	if _, err := holder.Exec(`INSERT INTO resources (id, resource_type, data) VALUES ('T1','items-trash','{"key":"T1","data":{"dateModified":"2020-01-02"}}'), ('T2','items-trash','{"key":"T2","data":{"dateModified":"2020-01-03"}}')`); err != nil {
		t.Fatalf("seed: %v", err)
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

	go func() {
		time.Sleep(30 * time.Millisecond)
		if _, err := holder.Exec(`COMMIT`); err != nil {
			t.Logf("commit: %v", err)
		}
	}()
	ctxTrash, cancelTrash := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelTrash()
	rows, err := s.QueryTrashContext(ctxTrash, TrashQuery{Limit: 10})
	if err != nil {
		t.Fatalf("QueryTrash did not retry busy: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("QueryTrash rows = %d, want 2", len(rows))
	}

	// Context variant also retries.
	if _, err := holder.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		if _, err := holder.Exec(`COMMIT`); err != nil {
			t.Logf("commit2: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows2, err := s.QueryTrashContext(ctx, TrashQuery{Limit: 10})
	if err != nil {
		t.Fatalf("QueryTrashContext did not retry: %v", err)
	}
	if len(rows2) != 2 {
		t.Fatalf("QueryTrashContext rows = %d, want 2", len(rows2))
	}
}

func TestQuerySimilarityCandidates_RetriesBusyInsteadOfFailingHard(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "busy_sim.db")
	holder, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.Exec(`CREATE TABLE resources (id TEXT, resource_type TEXT, data TEXT, parent_key TEXT, item_type TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := holder.Exec(`INSERT INTO resources (id, resource_type, data, parent_key, item_type) VALUES ('A','items','{"key":"A"}','','book'), ('B','items','{"key":"B"}','','journalArticle')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := holder.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _, _ = holder.Exec(`ROLLBACK`) })

	queryDB, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open queryDB: %v", err)
	}
	queryDB.SetMaxOpenConns(1)
	s := &Store{db: queryDB}
	t.Cleanup(func() { _ = s.Close() })

	go func() {
		time.Sleep(30 * time.Millisecond)
		if _, err := holder.Exec(`COMMIT`); err != nil {
			t.Logf("commit: %v", err)
		}
	}()

	ctxSim, cancelSim := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelSim()
	cands, err := s.QuerySimilarityCandidatesContext(ctxSim)
	if err != nil {
		t.Fatalf("QuerySimilarityCandidates did not retry: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("cands = %d, want 2", len(cands))
	}
}
