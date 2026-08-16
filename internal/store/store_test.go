// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreMirroredItem_Atomicity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// Seed only the trash mirror to simulate the sync-reconciled case where
	// reconcileItemLifecycleTx deleted the live row (trash had higher version).
	trashRaw := json.RawMessage(`{"key":"ATOMIC1","version":5,"data":{"key":"ATOMIC1","itemType":"book","title":"AtomicPayload","version":5}}`)
	if err := s.UpsertKeyed("items-trash", []string{"ATOMIC1"}, []json.RawMessage{trashRaw}); err != nil {
		t.Fatalf("seed trash: %v", err)
	}
	// Ensure pre-call state is items=0, items-trash=1.
	if cnt, _ := s.Count("items"); cnt != 0 {
		t.Fatalf("pre-call items count = %d, want 0", cnt)
	}
	if cnt, _ := s.Count("items-trash"); cnt != 1 {
		t.Fatalf("pre-call items-trash count = %d, want 1", cnt)
	}
	// Verify FTS for the trash row exists.
	preSearch, err := s.Search("AtomicPayload", 10)
	if err != nil {
		t.Fatalf("pre-call search: %v", err)
	}
	if len(preSearch) == 0 {
		t.Fatalf("pre-call search should find trash row, got 0 results")
	}

	// Inject a failure partway through the transaction: a trigger that
	// aborts the DELETE on resources for this key. This makes the
	// transaction fail AFTER the live upsert and FTS maintenance have
	// executed but BEFORE commit. In a correct single-transaction
	// implementation the whole transaction rolls back; in a split two-
	// transaction implementation the live upsert would already be committed.
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_restore_delete BEFORE DELETE ON resources WHEN OLD.resource_type='items-trash' AND OLD.id='ATOMIC1' BEGIN SELECT RAISE(ABORT, 'injected restore failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	restored := json.RawMessage(`{"key":"ATOMIC1","data":{"key":"ATOMIC1","itemType":"book","title":"RestoredAtomic","version":6}}`)
	err = s.RestoreMirroredItem("ATOMIC1", restored)
	if err == nil {
		t.Fatalf("RestoreMirroredItem should have failed due to injected trigger, got nil error")
	}
	if !strings.Contains(err.Error(), "injected restore failure") && !strings.Contains(err.Error(), "items-trash/ATOMIC1") {
		t.Fatalf("unexpected error from injected failure: %v", err)
	}

	// Atomicity assertion: NEITHER the live upsert NOR the trash deletion
	// is committed. Pre-call state (items=0, items-trash=1) must be intact.
	if got, _ := s.Get("items", "ATOMIC1"); got != nil {
		t.Fatalf("atomicity violation: live row was committed despite transaction failure, got %s", string(got))
	}
	if got, _ := s.Get("items-trash", "ATOMIC1"); got == nil {
		t.Fatalf("atomicity violation: trash row was deleted despite transaction rollback")
	}
	if cnt, _ := s.Count("items"); cnt != 0 {
		t.Fatalf("items count after failed restore = %d, want 0 (atomic rollback)", cnt)
	}
	if cnt, _ := s.Count("items-trash"); cnt != 1 {
		t.Fatalf("items-trash count after failed restore = %d, want 1 (atomic rollback)", cnt)
	}
	// FTS must also be intact: the trash document should still be searchable
	// and no new live document should have been indexed.
	postSearchTrash, err := s.Search("AtomicPayload", 10)
	if err != nil {
		t.Fatalf("post-failure search trash: %v", err)
	}
	if len(postSearchTrash) == 0 {
		t.Fatalf("post-failure search should still find trash row via FTS, got 0")
	}
	postSearchLive, err := s.Search("RestoredAtomic", 10)
	if err != nil {
		t.Fatalf("post-failure search live: %v", err)
	}
	if len(postSearchLive) != 0 {
		t.Fatalf("post-failure search should not find live row (rolled back), got %d results", len(postSearchLive))
	}

	// Remove the trigger and verify the happy path commits atomically to
	// items=1, items-trash=0.
	if _, err := s.DB().Exec(`DROP TRIGGER fail_restore_delete`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if err := s.RestoreMirroredItem("ATOMIC1", restored); err != nil {
		t.Fatalf("RestoreMirroredItem after dropping trigger: %v", err)
	}
	if got, _ := s.Get("items", "ATOMIC1"); got == nil {
		t.Fatalf("live row should exist after successful restore")
	}
	if got, _ := s.Get("items-trash", "ATOMIC1"); got != nil {
		t.Fatalf("trash row should be gone after successful restore, got %s", string(got))
	}
	if cnt, _ := s.Count("items"); cnt != 1 {
		t.Fatalf("items count after successful restore = %d, want 1", cnt)
	}
	if cnt, _ := s.Count("items-trash"); cnt != 0 {
		t.Fatalf("items-trash count after successful restore = %d, want 0", cnt)
	}
}

func TestRestoreMirroredItem_HappyPathDoesNotNeedTrigger(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	trashRaw := json.RawMessage(`{"key":"HAPPY1","version":3,"data":{"key":"HAPPY1","itemType":"book","title":"HappyPath"}}`)
	if err := s.UpsertKeyed("items-trash", []string{"HAPPY1"}, []json.RawMessage{trashRaw}); err != nil {
		t.Fatalf("seed trash: %v", err)
	}
	restored := json.RawMessage(`{"key":"HAPPY1","data":{"key":"HAPPY1","itemType":"book","title":"HappyRestored"}}`)
	if err := s.RestoreMirroredItem("HAPPY1", restored); err != nil {
		t.Fatalf("RestoreMirroredItem: %v", err)
	}
	live, _ := s.Get("items", "HAPPY1")
	if live == nil || !strings.Contains(string(live), "HappyRestored") {
		t.Fatalf("live row after restore: %v", string(live))
	}
	trash, _ := s.Get("items-trash", "HAPPY1")
	if trash != nil {
		t.Fatalf("trash row should be reaped, got %s", string(trash))
	}
}
