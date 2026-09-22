// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestStoreGuardedDBSerializesWrites proves the guarded ad-hoc handle shares
// the single-writer lock: a raw-handle write racing a batch upsert lands
// without SQLITE_BUSY and neither write is lost.
func TestStoreGuardedDBSerializesWrites(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	items := []json.RawMessage{
		json.RawMessage(`{"id": "B1"}`),
		json.RawMessage(`{"id": "B2"}`),
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, _, err := s.UpsertBatch("guarded", items); err != nil {
			errCh <- err
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := s.DB().ExecContext(context.Background(),
			`INSERT INTO sync_state (resource_type, last_cursor, total_count) VALUES (?, ?, ?)
			 ON CONFLICT(resource_type) DO UPDATE SET last_cursor = excluded.last_cursor`,
			"guarded", "cursor-1", 2,
		); err != nil {
			errCh <- err
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("guarded write failed: %v", err)
	}

	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM resources WHERE resource_type = 'guarded'`).Scan(&count); err != nil {
		t.Fatalf("count resources: %v", err)
	}
	if count != 2 {
		t.Fatalf("resources = %d, want 2", count)
	}
	cursor, _, _, _, err := s.GetSyncResumeState("guarded")
	if err != nil {
		t.Fatalf("read sync state: %v", err)
	}
	if cursor != "cursor-1" {
		t.Fatalf("cursor = %q, want %q", cursor, "cursor-1")
	}
}

// TestStoreUpsertBatchContextCancelledLeavesStoreConsistent blocks the writer
// lock, cancels mid-wait, and asserts the batch returns the context error
// with nothing half-written.
func TestStoreUpsertBatchContextCancelledLeavesStoreConsistent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	releaseHolder, err := s.acquireWrite(context.Background())
	if err != nil {
		t.Fatalf("hold write lock: %v", err)
	}
	defer releaseHolder()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := s.UpsertBatchContext(ctx, "cancelled",
			[]json.RawMessage{json.RawMessage(`{"id": "C1"}`)})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("UpsertBatchContext err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UpsertBatchContext did not return after cancellation")
	}

	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM resources WHERE resource_type = 'cancelled'`).Scan(&count); err != nil {
		t.Fatalf("count resources: %v", err)
	}
	if count != 0 {
		t.Fatalf("cancelled batch left %d rows, want 0", count)
	}
}

// TestStoreSaveSyncStateContextCancelledFailsFast asserts a pre-cancelled
// context never reaches SQLite and leaves the checkpoint untouched.
func TestStoreSaveSyncStateContextCancelledFailsFast(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.SaveSyncStateContext(ctx, "cancelled", "cursor-x", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveSyncStateContext err = %v, want context.Canceled", err)
	}
	cursor, _, _, _, err := s.GetSyncResumeState("cancelled")
	if err != nil {
		t.Fatalf("read sync state: %v", err)
	}
	if cursor != "" {
		t.Fatalf("cursor = %q after cancelled save, want empty", cursor)
	}
}

// TestSearchContextsHonorCancelledContext pins the CLI fix: both FTS search
// paths take the request context, so a Ctrl+C during a large local read
// aborts instead of running to completion. A pre-cancelled context must
// fail fast with the context error rather than returning rows.
func TestSearchContextsHonorCancelledContext(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if _, _, err := s.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"CANCEL1","version":1,"data":{"key":"CANCEL1","itemType":"journalArticle","title":"cancelneedle paper"}}`),
	}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.SearchContext(ctx, "cancelneedle", 25); err == nil {
		t.Errorf("SearchContext with cancelled context returned nil error")
	}
	if _, err := s.SearchByTypeContext(ctx, "cancelneedle", "items", 25); err == nil {
		t.Errorf("SearchByTypeContext with cancelled context returned nil error")
	}
}

// TestGuardedDBReadMethodsRefuseWrites pins the ADR-0005 read contract: the
// four GuardedDB read methods only accept read statements, so a
// DELETE/UPDATE/INSERT (including INSERT ... RETURNING, which SQLite
// executes through a Query call) cannot bypass the single-writer lock. The
// comment- and case-smuggled forms prove the gate strips the same leading
// noise SQLite itself ignores before parsing.
func TestGuardedDBReadMethodsRefuseWrites(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if _, err := s.DB().Exec(`INSERT INTO resources (id, resource_type, data) VALUES ('seed', 'thing', '{}')`); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	writes := []string{
		`DELETE FROM resources WHERE id = 'seed'`,
		`UPDATE resources SET resource_type = 'hijacked' WHERE id = 'seed'`,
		`INSERT INTO resources (id, resource_type, data) VALUES ('evil', 'thing', '{}')`,
		`DELETE FROM resources WHERE id = 'seed' RETURNING id`,
		`-- setup` + "\n" + `DELETE FROM resources WHERE id = 'seed'`,
		`/* setup */ DELETE FROM resources WHERE id = 'seed'`,
		`delete from resources where id = 'seed'`,
	}
	// NOTE: WITH stays on the read allowlist so SELECT-form CTEs keep
	// working (the assignment's allowlist is SELECT, WITH, PRAGMA,
	// EXPLAIN). A CTE-wrapped write (WITH ... DELETE) therefore passes
	// this gate and is stopped one layer down by mode=ro on the
	// read-only production opens — the same split the MCP sql tool's
	// validateReadOnlyQuery documents — so it is not asserted here.
	for _, stmt := range writes {
		if _, err := s.DB().Query(stmt); err == nil {
			t.Errorf("Query(%q) succeeded, want refusal", stmt)
		}
		if _, err := s.DB().QueryContext(context.Background(), stmt); err == nil {
			t.Errorf("QueryContext(%q) succeeded, want refusal", stmt)
		}
		if err := s.DB().QueryRow(stmt).Scan(new(int)); err == nil {
			t.Errorf("QueryRow(%q) scan succeeded, want refusal", stmt)
		}
		if err := s.DB().QueryRowContext(context.Background(), stmt).Scan(new(int)); err == nil {
			t.Errorf("QueryRowContext(%q) scan succeeded, want refusal", stmt)
		}
	}
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM resources WHERE id = 'seed'`).Scan(&count); err != nil {
		t.Fatalf("seed SELECT failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("seed rows = %d, want 1 (no refused write may have landed)", count)
	}
}

// TestGuardedDBReadMethodsAcceptReadForms pins the allowlist side: plain
// SELECT, comment- and whitespace-prefixed SELECT, CTE-form SELECT, PRAGMA
// and EXPLAIN all still read through the guarded handle.
func TestGuardedDBReadMethodsAcceptReadForms(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if _, err := s.DB().Exec(`INSERT INTO resources (id, resource_type, data) VALUES ('seed', 'thing', '{}')`); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	reads := []struct {
		name string
		stmt string
		scan bool
		want int
	}{
		{"plain select", `SELECT COUNT(*) FROM resources`, true, 1},
		{"leading comment select", "  \n-- leading comment\n" + `SELECT COUNT(*) FROM resources`, true, 1},
		{"leading block select", `/* leading block */ SELECT COUNT(*) FROM resources`, true, 1},
		{"cte select", `WITH r AS (SELECT id FROM resources WHERE id = 'seed') SELECT COUNT(*) FROM r`, true, 1},
		{"pragma", `PRAGMA table_info(resources)`, false, 0},
		{"explain", `EXPLAIN SELECT COUNT(*) FROM resources`, false, 0},
	}
	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			rows, err := s.DB().Query(r.stmt)
			if err != nil {
				t.Fatalf("Query = %v, want success", err)
			}
			rows.Close()
			rows, err = s.DB().QueryContext(context.Background(), r.stmt)
			if err != nil {
				t.Fatalf("QueryContext = %v, want success", err)
			}
			rows.Close()
			if !r.scan {
				return
			}
			var n int
			if err := s.DB().QueryRow(r.stmt).Scan(&n); err != nil {
				t.Fatalf("QueryRow scan = %v, want success", err)
			}
			if n != r.want {
				t.Fatalf("QueryRow count = %d, want %d", n, r.want)
			}
			if err := s.DB().QueryRowContext(context.Background(), r.stmt).Scan(&n); err != nil {
				t.Fatalf("QueryRowContext scan = %v, want success", err)
			}
			if n != r.want {
				t.Fatalf("QueryRowContext count = %d, want %d", n, r.want)
			}
		})
	}
}
