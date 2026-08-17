// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/store"
)

// TestResolveLocalItemList_CancelledContextAbortsContendedQuery proves cancellation
// propagates from the CLI local-read planner through QueryItemsContext. Before
// context threading, resolveLocalItemList called QueryItems (Background) and a
// canceled request could sit in the busy-retry window for migrationLockTimeout.
func TestResolveLocalItemList_CancelledContextAbortsContendedQuery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cancel_local_items.db")
	s, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.Upsert("items", "A", json.RawMessage(`{"key":"A","version":1,"data":{"key":"A","itemType":"journalArticle","title":"Alpha"}}`)); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := s.DB().Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatalf("journal_mode delete: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	readDB, err := store.OpenReadOnlyContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open read-only store: %v", err)
	}
	t.Cleanup(func() { _ = readDB.Close() })
	if _, err := readDB.DB().Exec(`PRAGMA busy_timeout=0`); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}

	holder, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatalf("begin exclusive: %v", err)
	}
	t.Cleanup(func() { _, _ = holder.Exec(`ROLLBACK`) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, handled, err := resolveLocalItemList(ctx, readDB, "/items", map[string]string{})
	elapsed := time.Since(start)
	if !handled {
		t.Fatalf("resolveLocalItemList not handled for /items")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("cancellation not honored promptly: elapsed %v", elapsed)
	}
	if err == nil {
		t.Fatalf("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(strings.ToLower(err.Error()), "canceled") {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
