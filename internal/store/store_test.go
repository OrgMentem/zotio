// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestGetMissingRowReturnsErrNotFound pins missing rows to an identifiable error.
// Without the sentinel, Get returns nil, nil and hides a missing row as success.
func TestGetMissingRowReturnsErrNotFound(t *testing.T) {
	s := queryTestStore(t)

	raw, err := s.Get("items", "MISSING")
	if raw != nil {
		t.Fatalf("Get missing row returned %s, want nil", raw)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing row error = %v, want errors.Is(err, ErrNotFound)", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	raw, err = s.Get("items", "MISSING")
	if raw != nil {
		t.Fatalf("Get from closed store returned %s, want nil", raw)
	}
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get from closed store error = %v, want a non-not-found read error", err)
	}
}
func TestSyncResumeStateBindsCursorToRequestScope(t *testing.T) {
	s := queryTestStore(t)
	defer s.Close()

	const scope = `{"plane":"https://api.zotero.org/users/1","mode":"incremental","since":42}`
	if err := s.SaveSyncResumeState("items", "100", scope, 100); err != nil {
		t.Fatalf("SaveSyncResumeState: %v", err)
	}
	cursor, gotScope, syncedAt, count, err := s.GetSyncResumeState("items")
	if err != nil {
		t.Fatalf("GetSyncResumeState: %v", err)
	}
	if cursor != "100" || gotScope != scope || syncedAt.IsZero() || count != 100 {
		t.Fatalf("resume state = (%q, %q, %v, %d), want cursor and scope preserved", cursor, gotScope, syncedAt, count)
	}

	if err := s.SaveSyncState("items", "legacy", 1); err != nil {
		t.Fatalf("SaveSyncState: %v", err)
	}
	cursor, gotScope, _, _, err = s.GetSyncResumeState("items")
	if err != nil || cursor != "legacy" || gotScope != "" {
		t.Fatalf("unqualified state = (%q, %q, %v), want a cursor without resumable provenance", cursor, gotScope, err)
	}
	if err := s.ClearSyncCursor("items"); err != nil {
		t.Fatalf("ClearSyncCursor: %v", err)
	}
	cursor, gotScope, _, _, err = s.GetSyncResumeState("items")
	if err != nil || cursor != "" || gotScope != "" {
		t.Fatalf("cleared state = (%q, %q, %v), want no cursor or scope", cursor, gotScope, err)
	}
	if err := s.SaveSyncResumeState("items", "200", "", 200); err == nil {
		t.Fatal("SaveSyncResumeState accepted a cursor without request provenance")
	}
}

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
	if _, err := s.UpsertKeyed("items-trash", []string{"ATOMIC1"}, []json.RawMessage{trashRaw}); err != nil {
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
	if got, err := s.Get("items", "ATOMIC1"); got != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("atomicity violation: live row = %s, err = %v; want nil and ErrNotFound", got, err)
	}
	if got, err := s.Get("items-trash", "ATOMIC1"); got == nil || err != nil {
		t.Fatalf("atomicity violation: trash row was deleted despite transaction rollback: got=%s err=%v", got, err)
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
	if got, err := s.Get("items", "ATOMIC1"); got == nil || err != nil {
		t.Fatalf("live row should exist after successful restore: got=%s err=%v", got, err)
	}
	if got, err := s.Get("items-trash", "ATOMIC1"); got != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("trash row should be gone after successful restore: got=%s err=%v", got, err)
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
	if _, err := s.UpsertKeyed("items-trash", []string{"HAPPY1"}, []json.RawMessage{trashRaw}); err != nil {
		t.Fatalf("seed trash: %v", err)
	}
	restored := json.RawMessage(`{"key":"HAPPY1","data":{"key":"HAPPY1","itemType":"book","title":"HappyRestored"}}`)
	if err := s.RestoreMirroredItem("HAPPY1", restored); err != nil {
		t.Fatalf("RestoreMirroredItem: %v", err)
	}
	live, err := s.Get("items", "HAPPY1")
	if err != nil || live == nil || !strings.Contains(string(live), "HappyRestored") {
		t.Fatalf("live row after restore: %v, err=%v", string(live), err)
	}
	trash, err := s.Get("items-trash", "HAPPY1")
	if trash != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("trash row should be reaped: got=%s err=%v", trash, err)
	}
}

// TestReapMirroredItem_Atomicity pins the permanent-delete cleanup to one
// transaction. The former implementation called ReapResource twice, committing
// and releasing writeMu in between, so a failure on the second resource left
// the live row already deleted and the trash row behind for good. Injecting
// that failure is the only deterministic way to tell one transaction from two.
func TestReapMirroredItem_Atomicity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// A permanently deleted item is commonly in both mirrors: it was trashed
	// first, and mirrorTrashedItem deliberately keeps the live row.
	liveRaw := json.RawMessage(`{"key":"REAP1","version":7,"data":{"key":"REAP1","itemType":"book","title":"ReapLive"}}`)
	if _, err := s.UpsertKeyed("items", []string{"REAP1"}, []json.RawMessage{liveRaw}); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	trashRaw := json.RawMessage(`{"key":"REAP1","version":7,"data":{"key":"REAP1","itemType":"book","title":"ReapTrash"}}`)
	if _, err := s.UpsertKeyed("items-trash", []string{"REAP1"}, []json.RawMessage{trashRaw}); err != nil {
		t.Fatalf("seed trash: %v", err)
	}
	if err := s.RecordPendingWrite("items", "REAP1", []byte(`[{"field":"title","add":"ReapLive"}]`)); err != nil {
		t.Fatalf("seed pending write: %v", err)
	}

	// Abort the trash deletion, which is the second resource the helper
	// touches. One transaction rolls the live deletion back with it; two
	// transactions would have committed the live deletion already.
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_reap_trash BEFORE DELETE ON resources WHEN OLD.resource_type='items-trash' AND OLD.id='REAP1' BEGIN SELECT RAISE(ABORT, 'injected reap failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	err = s.ReapMirroredItem("REAP1")
	if err == nil {
		t.Fatal("ReapMirroredItem succeeded despite the injected trigger")
	}
	if !strings.Contains(err.Error(), "injected reap failure") && !strings.Contains(err.Error(), "items-trash/REAP1") {
		t.Fatalf("error from injected failure = %v, want it to name the trash resource", err)
	}
	if got, err := s.Get("items", "REAP1"); got == nil || err != nil {
		t.Fatalf("atomicity violation: live row was deleted despite rollback: got=%s err=%v", got, err)
	}
	if got, err := s.Get("items-trash", "REAP1"); got == nil || err != nil {
		t.Fatalf("atomicity violation: trash row = %s, err = %v; want it retained", got, err)
	}
	if hits, err := s.Search("ReapLive", 10); err != nil || len(hits) == 0 {
		t.Fatalf("live FTS document = %d hits, err = %v; want the rollback to keep it", len(hits), err)
	}
	pending, err := s.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	rolled, ok := pending["REAP1"]
	if !ok {
		t.Fatal("atomicity violation: the pending-write marker was cleared despite rollback")
	}
	if rolled.Deleted {
		t.Fatal("atomicity violation: a deletion marker was committed while the rows it guards were rolled back")
	}

	if _, err := s.DB().Exec(`DROP TRIGGER fail_reap_trash`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if err := s.ReapMirroredItem("REAP1"); err != nil {
		t.Fatalf("ReapMirroredItem after dropping the trigger: %v", err)
	}
	if got, err := s.Get("items", "REAP1"); got != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("live row after reap: got=%s err=%v, want ErrNotFound", got, err)
	}
	if got, err := s.Get("items-trash", "REAP1"); got != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("trash row after reap: got=%s err=%v, want ErrNotFound", got, err)
	}
	if hits, err := s.Search("ReapLive", 10); err != nil || len(hits) != 0 {
		t.Fatalf("live FTS document after reap = %d hits, err = %v; want 0", len(hits), err)
	}
	if hits, err := s.Search("ReapTrash", 10); err != nil || len(hits) != 0 {
		t.Fatalf("trash FTS document after reap = %d hits, err = %v; want 0", len(hits), err)
	}
	pending, err = s.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites after reap: %v", err)
	}
	mark, ok := pending["REAP1"]
	if !ok {
		t.Fatal("no deletion marker after the reap: the read plane still lists the key, so the next sync resurrects it")
	}
	if !mark.Deleted {
		t.Fatal("the reap left the field-change marker instead of a deletion marker; reconcilePendingWrites would merge into a resurrected row")
	}
	trashMark, err := s.PendingWrites("items-trash")
	if err != nil {
		t.Fatalf("PendingWrites(items-trash) after reap: %v", err)
	}
	if !trashMark["REAP1"].Deleted {
		t.Fatal("no deletion marker for items-trash: /items/trash keeps listing a purged item, so the trash row comes back")
	}
}

// TestReapMirroredItem_TrashRowNeverSurvivesContention runs the reap against
// the stale sync upsert it competes with in production. The trash row must be
// gone every time the call returns, whichever order the two writers take.
// A raw UpsertKeyed that lands after the reap still wins the LIVE row: the
// deletion marker is consulted by reconcilePendingWrites on the sync path, not
// by the store's unconditional upsert, which local write-through needs to stay
// authoritative. This test therefore pins the ordering invariant the split
// implementation broke; suppression of the sync re-insert is pinned in
// internal/cli's mirror_reap_test.go.
func TestReapMirroredItem_TrashRowNeverSurvivesContention(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	const iterations = 40
	for i := 0; i < iterations; i++ {
		key := fmt.Sprintf("RACE%04d", i)
		live := json.RawMessage(fmt.Sprintf(`{"key":%q,"version":1,"data":{"key":%q,"itemType":"book","title":"Racing"}}`, key, key))
		if _, err := s.UpsertKeyed("items", []string{key}, []json.RawMessage{live}); err != nil {
			t.Fatalf("seed live %s: %v", key, err)
		}
		if _, err := s.UpsertKeyed("items-trash", []string{key}, []json.RawMessage{live}); err != nil {
			t.Fatalf("seed trash %s: %v", key, err)
		}

		// The stale sync: Zotero's local read plane still lists the item, so
		// sync upserts it back into the live mirror while the delete cleans up.
		var wg sync.WaitGroup
		wg.Add(2)
		var upsertErr, reapErr error
		go func() {
			defer wg.Done()
			_, upsertErr = s.UpsertKeyed("items", []string{key}, []json.RawMessage{live})
		}()
		go func() {
			defer wg.Done()
			reapErr = s.ReapMirroredItem(key)
		}()
		wg.Wait()
		if upsertErr != nil {
			t.Fatalf("stale sync upsert %s: %v", key, upsertErr)
		}
		if reapErr != nil {
			t.Fatalf("ReapMirroredItem %s: %v", key, reapErr)
		}
		if got, err := s.Get("items-trash", key); got != nil || !errors.Is(err, ErrNotFound) {
			t.Fatalf("trash row for %s survived the reap: got=%s err=%v", key, got, err)
		}
	}
}

// The deletion marker and the row removal it guards must commit as one unit.
// If the rows could go without the marker, the very next sync — which still
// reads a plane that lists the key — would re-insert the item with nothing left
// to suppress it, which is the resurrection this marker exists to prevent.
// Aborting the marker insert is the only deterministic way to prove the reap
// does not treat it as a best-effort afterthought.
func TestReapMirroredItemRollsBackWhenTheDeletionMarkerCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	live := json.RawMessage(`{"key":"MARK1","version":3,"data":{"key":"MARK1","itemType":"book","title":"MarkLive"}}`)
	if _, err := s.UpsertKeyed("items", []string{"MARK1"}, []json.RawMessage{live}); err != nil {
		t.Fatalf("seed live: %v", err)
	}

	if _, err := s.DB().Exec(`CREATE TRIGGER fail_reap_marker BEFORE INSERT ON pending_writes WHEN NEW.deleted=1 AND NEW.id='MARK1' BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := s.ReapMirroredItem("MARK1"); err == nil {
		t.Fatal("ReapMirroredItem succeeded despite the injected marker failure")
	}
	if got, err := s.Get("items", "MARK1"); got == nil || err != nil {
		t.Fatalf("live row is gone but no deletion marker was written: got=%s err=%v", got, err)
	}

	if _, err := s.DB().Exec(`DROP TRIGGER fail_reap_marker`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if err := s.ReapMirroredItem("MARK1"); err != nil {
		t.Fatalf("ReapMirroredItem after dropping the trigger: %v", err)
	}
	pending, err := s.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if !pending["MARK1"].Deleted {
		t.Fatal("deletion marker missing after a successful reap")
	}
}

// A deletion outranks a field change for the same key. One mutation envelope
// can carry both (a title update and a permanent delete of the same item), and
// the applied order inside the envelope is not the store's to choose. The
// deletion marker must come out of that collision untouched: an overwritten
// payload puts replayable changes behind a flag that says the key is gone, and
// a refreshed written_at silently restarts the TTL clock that bounds how long
// the suppression may last.
func TestRecordPendingWriteNeverOverwritesADeletionMarker(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	live := json.RawMessage(`{"key":"ORD1","version":1,"data":{"key":"ORD1","itemType":"book","title":"Ordered"}}`)
	if _, err := s.UpsertKeyed("items", []string{"ORD1"}, []json.RawMessage{live}); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	if err := s.ReapMirroredItem("ORD1"); err != nil {
		t.Fatalf("ReapMirroredItem: %v", err)
	}
	// Age the marker so a refreshed written_at is detectable.
	aged := time.Now().Add(-48 * time.Hour).UTC()
	if _, err := s.DB().Exec(`UPDATE pending_writes SET written_at = ? WHERE id = 'ORD1'`, aged); err != nil {
		t.Fatalf("age marker: %v", err)
	}
	if err := s.RecordPendingWrite("items", "ORD1", []byte(`[{"field":"title","add":"Ordered"}]`)); err != nil {
		t.Fatalf("RecordPendingWrite: %v", err)
	}

	pending, err := s.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	mark := pending["ORD1"]
	if !mark.Deleted {
		t.Fatal("a field-change marker overwrote the deletion marker; the purged item can be resurrected again")
	}
	if string(mark.Changes) != "[]" {
		t.Errorf("deletion marker changes = %s, want []: field changes must not be parked behind a deletion flag", mark.Changes)
	}
	if time.Since(mark.WrittenAt) < 24*time.Hour {
		t.Errorf("deletion marker written_at = %v, want the aged value: recording a field write must not restart the suppression TTL", mark.WrittenAt)
	}

	// A key with no deletion marker still records normally.
	if err := s.RecordPendingWrite("items", "ORD2", []byte(`[{"field":"title","add":"Kept"}]`)); err != nil {
		t.Fatalf("RecordPendingWrite ORD2: %v", err)
	}
	pending, err = s.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if mark, ok := pending["ORD2"]; !ok || mark.Deleted {
		t.Fatalf("ordinary pending write for ORD2 = %+v, ok=%v; the guard must only protect deletion markers", mark, ok)
	}
}

// A deletion marker retires when a COMPLETE pass stops listing the key: that is
// the only evidence available that Zotero synced the delete down, because the
// local desktop API implements no /deleted feed. While the pass still lists the
// key the marker must stay, or the next page would re-insert the item.
func TestSweepMissingRetiresOnlyConfirmedDeletionMarkers(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	for _, key := range []string{"LANDED", "LAGGING"} {
		raw := json.RawMessage(fmt.Sprintf(`{"key":%q,"version":1,"data":{"key":%q,"itemType":"book","title":"Purged"}}`, key, key))
		if _, err := s.UpsertKeyed("items", []string{key}, []json.RawMessage{raw}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
		if err := s.ReapMirroredItem(key); err != nil {
			t.Fatalf("ReapMirroredItem %s: %v", key, err)
		}
	}
	// A full pass that still reports LAGGING: Zotero has not synced that delete
	// down yet. LANDED is absent, so its delete has landed.
	if _, err := s.SweepMissing("items", map[string]bool{"LAGGING": true}); err != nil {
		t.Fatalf("SweepMissing: %v", err)
	}

	pending, err := s.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if _, ok := pending["LANDED"]; ok {
		t.Error("confirmed deletion marker leaked: the read plane no longer lists the key, so it can never be cleared by a later page")
	}
	if !pending["LAGGING"].Deleted {
		t.Error("deletion marker retired while the read plane still lists the key; the next page resurrects the item")
	}
}

// pending_writes predates the deleted column, and CREATE TABLE IF NOT EXISTS is
// a no-op against a database an older binary created. Without the backfill,
// every PendingWrites call on such a database fails with "no such column:
// deleted" — which is every sync, not a corner case. A legacy row must also
// survive as an ordinary field-change marker rather than reading as a deletion.
func TestPendingWritesDeletedColumnIsBackfilledOnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Re-create the table exactly as the pre-ADR-0007 binary declared it.
	for _, stmt := range []string{
		`DROP TABLE pending_writes`,
		`CREATE TABLE pending_writes (
			resource_type TEXT NOT NULL,
			id TEXT NOT NULL,
			changes JSON NOT NULL,
			written_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (resource_type, id)
		)`,
		`INSERT INTO pending_writes (resource_type, id, changes, written_at)
		 VALUES ('items', 'LEGACY', '[{"field":"title","add":"Legacy"}]', CURRENT_TIMESTAMP)`,
	} {
		if _, err := s.DB().Exec(stmt); err != nil {
			t.Fatalf("simulate legacy schema (%s): %v", stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := OpenWithContext(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	pending, err := s2.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites on an upgraded database: %v", err)
	}
	mark, ok := pending["LEGACY"]
	if !ok {
		t.Fatal("the upgrade dropped a legacy pending write")
	}
	if mark.Deleted {
		t.Error("a legacy field-change marker read back as a deletion marker; it would suppress the row instead of merging into it")
	}
	if err := s2.ReapMirroredItem("LEGACY"); err != nil {
		t.Fatalf("ReapMirroredItem on an upgraded database: %v", err)
	}
	pending, err = s2.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites after reap: %v", err)
	}
	if !pending["LEGACY"].Deleted {
		t.Error("the upgraded database cannot record a deletion marker")
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
	itemNested := json.RawMessage(`{"key":"CVER_NESTED","data":{"key":"CVER_NESTED","itemType":"book","title":"Versioned Nested","version":20}}`)
	itemNoVer := json.RawMessage(`{"key":"CVER_NOVER","data":{"key":"CVER_NOVER","itemType":"book","title":"No Version"}}`)
	if _, _, err := s.UpsertBatch("items", []json.RawMessage{itemA, itemB, itemNested, itemNoVer}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	coll := json.RawMessage(`{"key":"CVER_COLL","version":99,"data":{"key":"CVER_COLL","name":"Versioned Collection","version":99}}`)
	if err := s.Upsert("collections", "CVER_COLL", coll); err != nil {
		t.Fatalf("seed collection: %v", err)
	}

	if cnt, _ := s.Count("items"); cnt != 4 {
		t.Fatalf("pre-clear items count = %d, want 4", cnt)
	}
	if cnt, _ := s.Count("collections"); cnt != 1 {
		t.Fatalf("pre-clear collections count = %d, want 1", cnt)
	}

	if err := s.ClearResourceVersions("items"); err != nil {
		t.Fatalf("ClearResourceVersions: %v", err)
	}

	if cnt, _ := s.Count("items"); cnt != 4 {
		t.Fatalf("post-clear items count = %d, want 4", cnt)
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
		{name: "nested-only item", key: "CVER_NESTED"},
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

	// Clearing must disarm the guard for both version locations, so a lower
	// version from the new plane can replace the former nested-only payload.
	if err := s.Upsert("items", "CVER_NESTED", json.RawMessage(`{"key":"CVER_NESTED","data":{"key":"CVER_NESTED","itemType":"book","title":"Lower After Clear","version":1}}`)); err != nil {
		t.Fatalf("lower nested upsert after clear: %v", err)
	}
	raw, err := s.Get("items", "CVER_NESTED")
	if err != nil {
		t.Fatalf("Get nested item after lower upsert: %v", err)
	}
	if raw == nil || !strings.Contains(string(raw), "Lower After Clear") {
		t.Fatalf("lower nested payload after clear = %s, want Lower After Clear", string(raw))
	}

	raw, _ = s.Get("items", "CVER_NOVER")
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

func TestSweepMissingReapsUnseenRows(t *testing.T) {
	s := queryTestStore(t)
	const resourceType = "sweep-test"
	if err := s.Upsert(resourceType, "SWEEP_SEEN", json.RawMessage(`{"id":"SWEEP_SEEN","label":"seen"}`)); err != nil {
		t.Fatalf("seed seen row: %v", err)
	}
	if err := s.Upsert(resourceType, "SWEEP_GONE", json.RawMessage(`{"id":"SWEEP_GONE","label":"gone"}`)); err != nil {
		t.Fatalf("seed unseen row: %v", err)
	}

	reaped, err := s.SweepMissing(resourceType, map[string]bool{"SWEEP_SEEN": true})
	if err != nil {
		t.Fatalf("SweepMissing: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("SweepMissing reaped %d rows, want 1", reaped)
	}
	if raw, err := s.Get(resourceType, "SWEEP_SEEN"); err != nil {
		t.Fatalf("get seen row: %v", err)
	} else if raw == nil {
		t.Fatal("SweepMissing removed seen row")
	}
	if raw, err := s.Get(resourceType, "SWEEP_GONE"); raw != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("SweepMissing retained unseen row: got=%s err=%v", string(raw), err)
	}

	var orphanFTS int
	if err := s.DB().QueryRow(
		`SELECT count(*) FROM resources_fts WHERE rowid = ?`,
		ftsRowID(resourceType, "SWEEP_GONE"),
	).Scan(&orphanFTS); err != nil {
		t.Fatalf("count reaped FTS row: %v", err)
	}
	if orphanFTS != 0 {
		t.Fatalf("reaped row left %d resources_fts rows, want 0", orphanFTS)
	}
}
