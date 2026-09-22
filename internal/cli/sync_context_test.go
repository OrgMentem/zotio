// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

// TestSyncBatchContextCancelledLeavesStoreConsistent asserts a cancelled sync
// stops waiting on the SQLite write and leaves no half-written batch behind:
// the rows the plane sent either all land or none do.
func TestSyncBatchContextCancelledLeavesStoreConsistent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	items := []json.RawMessage{
		json.RawMessage(`{"id": "X1"}`),
		json.RawMessage(`{"id": "X2"}`),
	}
	if _, _, err := upsertResourceBatch(ctx, db, "ctxbatch", items); !errors.Is(err, context.Canceled) {
		t.Fatalf("upsertResourceBatch err = %v, want context.Canceled", err)
	}

	stored, err := db.ResourceIDs("ctxbatch")
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("cancelled batch left %d rows, want 0", len(stored))
	}
}

// TestSyncSingleObjectContextCancelled asserts a singleton persist honours
// cancellation instead of writing past its deadline.
func TestSyncSingleObjectContextCancelled(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := upsertSingleObject(ctx, db, "numeric_ids", json.RawMessage(`{"id": 55043302}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("upsertSingleObject err = %v, want context.Canceled", err)
	}

	stored, err := db.ResourceIDs("numeric_ids")
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("cancelled singleton left %d rows, want 0", len(stored))
	}
}
