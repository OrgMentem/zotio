// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// A marker must hold the mirror against the read plane for exactly as long as
// the plane is known to be stale, and not one sync longer.

package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func pendingWritesTestItem(key string) json.RawMessage {
	return json.RawMessage(`{"key":"` + key + `","version":1,"data":{"key":"` + key + `","itemType":"book","title":"` + key + `"}}`)
}

func pendingWritesTestKeys(t *testing.T, page []json.RawMessage) []string {
	t.Helper()
	keys := make([]string, 0, len(page))
	for _, raw := range page {
		if raw == nil {
			t.Fatal("reconcilePendingWrites returned a nil row; the drop sentinel leaked into the stored page")
		}
		keys = append(keys, pendingWriteID("items", raw))
	}
	return keys
}

// A deletion marker removes the key from the page instead of merging into it.
// Merging is meaningless for a delete — there is no field to replay — and
// storing the row is the resurrection itself. Suppression is per key: the rest
// of the page is what the read plane genuinely reports and must still land, in
// the order it arrived.
func TestReconcilePendingWritesDropsRowsMarkedDeleted(t *testing.T) {
	db := syncTestOpenStore(t)
	defer db.Close()

	for _, key := range []string{"GONE1", "GONE2"} {
		if _, err := db.UpsertKeyed("items", []string{key}, []json.RawMessage{pendingWritesTestItem(key)}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
		if err := db.ReapMirroredItem(key); err != nil {
			t.Fatalf("ReapMirroredItem %s: %v", key, err)
		}
	}

	page := []json.RawMessage{
		pendingWritesTestItem("GONE1"),
		pendingWritesTestItem("KEEP1"),
		pendingWritesTestItem("GONE2"),
		pendingWritesTestItem("KEEP2"),
	}
	stderr := captureStderr(t, func() {
		merged, stillPending := reconcilePendingWrites(db, "items", page)
		got := pendingWritesTestKeys(t, merged)
		if strings.Join(got, ",") != "KEEP1,KEEP2" {
			t.Errorf("stored page = %v, want [KEEP1 KEEP2]: a purged key was re-inserted, or an unrelated row was dropped", got)
		}
		if stillPending != 2 {
			t.Errorf("stillPending = %d, want 2: both deletions are unconfirmed until the read plane stops listing them", stillPending)
		}
	})
	if stderr != "" {
		t.Errorf("stderr = %q, want silence: an in-window delete is the normal case, not a warning", stderr)
	}

	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	for _, key := range []string{"GONE1", "GONE2"} {
		if !pending[key].Deleted {
			t.Errorf("deletion marker for %s was cleared while the read plane still lists the key; the next page resurrects it", key)
		}
	}
}

// Documented TTL behaviour for a DELETION marker: the clock reports, it does
// not retire. The marker is written only after the write plane reported the
// delete applied, so a read plane still listing the key after the whole TTL
// means the desktop never synced — not that the object is back. Retiring on
// age would let elapsed time overrule a confirmed delete and put the item into
// offline reads and search in exactly the situation the marker exists for, and
// a warning does not undo a ghost. So the marker stays, the row stays
// suppressed, and zotio says the delete has not propagated.
func TestPendingWriteAgeCheckKeepsAConfirmedDeletionMarkerPastItsTTL(t *testing.T) {
	db := syncTestOpenStore(t)
	defer db.Close()

	if _, err := db.UpsertKeyed("items", []string{"STUCK"}, []json.RawMessage{pendingWritesTestItem("STUCK")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.ReapMirroredItem("STUCK"); err != nil {
		t.Fatalf("ReapMirroredItem: %v", err)
	}
	if _, err := db.DB().Exec(`UPDATE pending_writes SET written_at = ? WHERE id = 'STUCK'`,
		time.Now().Add(-2*pendingWriteTTL)); err != nil {
		t.Fatalf("age deletion marker: %v", err)
	}

	stderr := captureStderr(t, func() {
		checkPendingWriteAges(db, "items")
	})
	if !strings.Contains(stderr, "STUCK") || !strings.Contains(stderr, "permanently deleted") {
		t.Errorf("stderr = %q, want a warning naming the key and the unpropagated delete: an indefinite suppression the user is never told about is its own bug", stderr)
	}
	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if !pending["STUCK"].Deleted {
		t.Fatal("the age check retired a confirmed deletion marker; the next stale listing would resurrect a purged item")
	}

	// And the row stays suppressed, which is the whole reason not to retire it.
	merged, stillPending := reconcilePendingWrites(db, "items", []json.RawMessage{pendingWritesTestItem("STUCK")})
	if got := pendingWritesTestKeys(t, merged); len(got) != 0 {
		t.Fatalf("stored page = %v, want empty: a purged item must not be stored because a week passed", got)
	}
	if stillPending != 1 {
		t.Errorf("stillPending = %d, want 1: the deletion is still unconfirmed", stillPending)
	}
}

// The field-change half of the same pass keeps the old rule: a local guess that
// may never converge yields to the read plane once the TTL is up. Retiring it
// does not contradict any confirmed fact — the write plane confirmed a field
// value, not the absence of the object.
func TestPendingWriteAgeCheckRetiresAnExpiredFieldMarker(t *testing.T) {
	db := syncTestOpenStore(t)
	defer db.Close()

	if err := db.RecordPendingWrite("items", "DRIFTED", []byte(`[{"field":"collections","add":"TARGET"}]`)); err != nil {
		t.Fatalf("record pending write: %v", err)
	}
	if _, err := db.DB().Exec(`UPDATE pending_writes SET written_at = ? WHERE id = 'DRIFTED'`,
		time.Now().Add(-2*pendingWriteTTL)); err != nil {
		t.Fatalf("age field marker: %v", err)
	}

	stderr := captureStderr(t, func() {
		checkPendingWriteAges(db, "items")
	})
	if !strings.Contains(stderr, "DRIFTED") {
		t.Errorf("stderr = %q, want a warning naming the abandoned write", stderr)
	}
	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if _, ok := pending["DRIFTED"]; ok {
		t.Fatal("an expired field marker survived; it would pin the row to a local copy indefinitely")
	}
}

// A marker whose written_at is NULL cannot be aged, so the pass must leave it
// alone rather than treat an unrecorded clock as infinitely old and retire a
// marker that has held for no time at all.
func TestPendingWriteAgeCheckKeepsAFieldMarkerWithNoRecordedClock(t *testing.T) {
	db := syncTestOpenStore(t)
	defer db.Close()

	if err := db.RecordPendingWrite("items", "NOCLOCK", []byte(`[{"field":"collections","add":"TARGET"}]`)); err != nil {
		t.Fatalf("record pending write: %v", err)
	}
	if _, err := db.DB().Exec(`UPDATE pending_writes SET written_at = NULL WHERE id = 'NOCLOCK'`); err != nil {
		t.Fatalf("clear written_at: %v", err)
	}

	checkPendingWriteAges(db, "items")

	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if _, ok := pending["NOCLOCK"]; !ok {
		t.Fatal("the pass retired a marker with no recorded written_at; an unknown clock is not an expired one")
	}
}

// Markers are scoped per resource type. The reap writes one for items and one
// for items-trash because both planes keep listing a purged key, but a marker
// for one resource must never suppress a same-keyed row of another — Zotero
// keys are unique per library, not per resource, and a collection can share a
// key shape with an item.
func TestReconcilePendingWritesScopesDeletionMarkersToTheirResource(t *testing.T) {
	db := syncTestOpenStore(t)
	defer db.Close()

	if _, err := db.UpsertKeyed("items", []string{"SHARED"}, []json.RawMessage{pendingWritesTestItem("SHARED")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.ReapMirroredItem("SHARED"); err != nil {
		t.Fatalf("ReapMirroredItem: %v", err)
	}

	page := []json.RawMessage{json.RawMessage(`{"key":"SHARED","version":1,"data":{"key":"SHARED","name":"A collection"}}`)}
	merged, stillPending := reconcilePendingWrites(db, "collections", page)
	if len(merged) != 1 {
		t.Fatalf("stored collections page = %d rows, want 1: an items deletion marker suppressed a collection", len(merged))
	}
	if stillPending != 0 {
		t.Errorf("stillPending = %d, want 0", stillPending)
	}
}
