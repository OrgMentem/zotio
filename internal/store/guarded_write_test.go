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
