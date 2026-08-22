// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
func TestPendingWritesRoundtrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if cnt, err := s.PendingWriteCount(); err != nil {
		t.Fatalf("PendingWriteCount initial: %v", err)
	} else if cnt != 0 {
		t.Fatalf("PendingWriteCount initial = %d, want 0", cnt)
	}

	changes1 := []byte(`[{"op":"replace","field":"title","value":"first"}]`)
	if err := s.RecordPendingWrite("items", "ABC123", changes1); err != nil {
		t.Fatalf("RecordPendingWrite: %v", err)
	}

	if cnt, err := s.PendingWriteCount(); err != nil {
		t.Fatalf("PendingWriteCount after one: %v", err)
	} else if cnt != 1 {
		t.Fatalf("PendingWriteCount after one = %d, want 1", cnt)
	}

	pending, err := s.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("PendingWrites len = %d, want 1", len(pending))
	}
	pw, ok := pending["ABC123"]
	if !ok {
		t.Fatalf("PendingWrites missing key ABC123")
	}
	if !bytes.Equal(pw.Changes, changes1) {
		t.Fatalf("PendingWrites Changes = %s, want %s", string(pw.Changes), string(changes1))
	}
	if pw.WrittenAt.IsZero() {
		t.Fatalf("PendingWrites WrittenAt is zero, want non-zero")
	}
	if time.Since(pw.WrittenAt) > 5*time.Second {
		t.Fatalf("PendingWrites WrittenAt = %v, want recent", pw.WrittenAt)
	}

	other, err := s.PendingWrites("collections")
	if err != nil {
		t.Fatalf("PendingWrites collections: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("PendingWrites collections len = %d, want 0", len(other))
	}

	changes2 := []byte(`[{"op":"add","field":"tag","value":"second"}]`)
	if err := s.RecordPendingWrite("items", "ABC123", changes2); err != nil {
		t.Fatalf("RecordPendingWrite second: %v", err)
	}
	if cnt, err := s.PendingWriteCount(); err != nil {
		t.Fatalf("PendingWriteCount after update: %v", err)
	} else if cnt != 1 {
		t.Fatalf("PendingWriteCount after ON CONFLICT update = %d, want 1", cnt)
	}
	pending, err = s.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites after update: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("PendingWrites len after update = %d, want 1", len(pending))
	}
	pw = pending["ABC123"]
	if !bytes.Equal(pw.Changes, changes2) {
		t.Fatalf("PendingWrites after ON CONFLICT Changes = %s, want %s", string(pw.Changes), string(changes2))
	}
	if pw.WrittenAt.IsZero() {
		t.Fatalf("PendingWrites WrittenAt after update is zero")
	}

	changes3 := []byte(`[{"op":"second"}]`)
	if err := s.RecordPendingWrite("items", "DEF456", changes3); err != nil {
		t.Fatalf("RecordPendingWrite second row: %v", err)
	}
	if cnt, err := s.PendingWriteCount(); err != nil {
		t.Fatalf("PendingWriteCount after second row: %v", err)
	} else if cnt != 2 {
		t.Fatalf("PendingWriteCount after second row = %d, want 2", cnt)
	}

	if err := s.ClearPendingWrite("items", "ABC123"); err != nil {
		t.Fatalf("ClearPendingWrite: %v", err)
	}
	if cnt, err := s.PendingWriteCount(); err != nil {
		t.Fatalf("PendingWriteCount after clear: %v", err)
	} else if cnt != 1 {
		t.Fatalf("PendingWriteCount after clear = %d, want 1", cnt)
	}
	pending, err = s.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites after clear: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("PendingWrites len after clear = %d, want 1", len(pending))
	}
	if _, ok := pending["ABC123"]; ok {
		t.Fatalf("PendingWrites still contains cleared key ABC123")
	}
	remaining, ok := pending["DEF456"]
	if !ok {
		t.Fatalf("PendingWrites missing untouched key DEF456 after clear")
	}
	if !bytes.Equal(remaining.Changes, changes3) {
		t.Fatalf("untouched row Changes = %s, want %s", string(remaining.Changes), string(changes3))
	}

	if err := s.ClearPendingWrite("items", "NONEXISTENT"); err != nil {
		t.Fatalf("ClearPendingWrite nonexistent: %v", err)
	}
	if cnt, err := s.PendingWriteCount(); err != nil {
		t.Fatalf("PendingWriteCount after nonexistent clear: %v", err)
	} else if cnt != 1 {
		t.Fatalf("PendingWriteCount after nonexistent clear = %d, want 1", cnt)
	}
}

func TestPlaneChanged(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	const rt = "items"
	const planeA = "https://www.zotero.org"
	const planeB = "http://127.0.0.1:23119"

	t.Run("no checkpoint", func(t *testing.T) {
		got, err := s.PlaneChanged(rt, planeA)
		if err != nil {
			t.Fatalf("PlaneChanged no checkpoint: %v", err)
		}
		if got != false {
			t.Fatalf("PlaneChanged no checkpoint = %v, want false", got)
		}
	})

	if err := s.SaveLibraryVersion(rt, planeA, 42); err != nil {
		t.Fatalf("SaveLibraryVersion: %v", err)
	}

	for _, tc := range []struct {
		name   string
		source string
		want   bool
	}{
		{name: "same plane", source: planeA, want: false},
		{name: "different plane", source: planeB, want: true},
		{name: "empty source vs stored plane", source: "", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.PlaneChanged(rt, tc.source)
			if err != nil {
				t.Fatalf("PlaneChanged %s: %v", tc.name, err)
			}
			if got != tc.want {
				t.Fatalf("PlaneChanged %s = %v, want %v", tc.name, got, tc.want)
			}
		})
	}

	if err := s.SaveLibraryVersion(rt, planeB, 99); err != nil {
		t.Fatalf("SaveLibraryVersion planeB: %v", err)
	}
	if got, _ := s.PlaneChanged(rt, planeB); got != false {
		t.Fatalf("PlaneChanged after switch same plane = %v, want false", got)
	}
	if got, _ := s.PlaneChanged(rt, planeA); got != true {
		t.Fatalf("PlaneChanged after switch different plane = %v, want true", got)
	}
}

func TestClearResourceVersions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	itemA := json.RawMessage(`{"key":"CVER_A","version":10,"data":{"key":"CVER_A","itemType":"book","title":"Versioned A","version":10}}`)
	itemB := json.RawMessage(`{"key":"CVER_B","version":5,"data":{"key":"CVER_B","itemType":"book","title":"Versioned B","version":5}}`)
	itemNoVer := json.RawMessage(`{"key":"CVER_NOVER","data":{"key":"CVER_NOVER","itemType":"book","title":"No Version"}}`)
	if _, _, err := s.UpsertBatch("items", []json.RawMessage{itemA, itemB, itemNoVer}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	coll := json.RawMessage(`{"key":"CVER_COLL","version":99,"data":{"key":"CVER_COLL","name":"Versioned Collection","version":99}}`)
	if err := s.Upsert("collections", "CVER_COLL", coll); err != nil {
		t.Fatalf("seed collection: %v", err)
	}

	if cnt, _ := s.Count("items"); cnt != 3 {
		t.Fatalf("pre-clear items count = %d, want 3", cnt)
	}
	if cnt, _ := s.Count("collections"); cnt != 1 {
		t.Fatalf("pre-clear collections count = %d, want 1", cnt)
	}

	if err := s.ClearResourceVersions("items"); err != nil {
		t.Fatalf("ClearResourceVersions: %v", err)
	}

	if cnt, _ := s.Count("items"); cnt != 3 {
		t.Fatalf("post-clear items count = %d, want 3", cnt)
	}
	if cnt, _ := s.Count("collections"); cnt != 1 {
		t.Fatalf("post-clear collections count = %d, want 1 (unrelated type must survive)", cnt)
	}

	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "item A", key: "CVER_A"},
		{name: "item B", key: "CVER_B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := s.Get("items", tc.key)
			if err != nil {
				t.Fatalf("Get %s: %v", tc.key, err)
			}
			if raw == nil {
				t.Fatalf("Get %s returned nil, want row", tc.key)
			}
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatalf("decode %s: %v", tc.key, err)
			}
			if _, ok := obj["version"]; ok {
				t.Fatalf("version field still present after clear for %s: %s", tc.key, string(raw))
			}
			if data, ok := obj["data"].(map[string]any); ok {
				if _, ok := data["version"]; ok {
					t.Fatalf("data.version still present after clear for %s: %s", tc.key, string(raw))
				}
			}
			if !strings.Contains(string(raw), "Versioned") {
				t.Fatalf("payload corrupted after clear for %s: %s", tc.key, string(raw))
			}
		})
	}

	raw, _ := s.Get("items", "CVER_NOVER")
	if raw == nil {
		t.Fatalf("versionless row missing after clear")
	}
	if !strings.Contains(string(raw), "No Version") {
		t.Fatalf("versionless row payload = %s, want containing No Version", string(raw))
	}

	collRaw, err := s.Get("collections", "CVER_COLL")
	if err != nil {
		t.Fatalf("Get collection: %v", err)
	}
	if collRaw == nil {
		t.Fatalf("collection row missing after clear")
	}
	var collObj map[string]any
	if err := json.Unmarshal(collRaw, &collObj); err != nil {
		t.Fatalf("decode collection: %v", err)
	}
	if _, ok := collObj["version"]; !ok {
		t.Fatalf("unrelated collection version was stripped, want preserved: %s", string(collRaw))
	}
}
